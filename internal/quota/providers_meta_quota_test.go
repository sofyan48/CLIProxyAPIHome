package quota

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseMetaQuotaWithPlanAndWindows(t *testing.T) {
	now := time.Date(2026, 7, 16, 9, 0, 0, 0, time.UTC)
	body := []byte(`{
		"api_key": "LLM|fixture-only|not-a-real-key",
		"subs_tier_id": "fixture-tier",
		"subs_tier_name": "Muse Code Everyday Usage",
		"is_subs_active": true,
		"subs_usage": {
			"window": {"used_percent": 2, "window_duration_mins": 300, "resets_at": 1789678120},
			"weekly": {"used_percent": "25", "resets_at": "1789948800"},
			"tier": "fallback-tier"
		}
	}`)
	windows, plan, errParse := parseMetaQuota(body, now)
	if errParse != nil {
		t.Fatalf("parseMetaQuota() error = %v", errParse)
	}
	if plan == nil || plan.Name != "Muse Code Everyday Usage" || plan.Premium {
		t.Fatalf("unexpected plan: %+v", plan)
	}
	if len(windows) != 2 {
		t.Fatalf("windows = %d, want 2", len(windows))
	}

	window := windows[0]
	if window.ID != "meta-window" || window.PeriodUnit != "hour" || window.PeriodValue == nil || *window.PeriodValue != 5 || window.WindowSeconds == nil || *window.WindowSeconds != 18000 {
		t.Fatalf("unexpected 5h window period: %+v", window)
	}
	if window.Used == nil || *window.Used != 2 || window.Remaining == nil || *window.Remaining != 98 || window.Status != "healthy" || window.ResetAt == nil {
		t.Fatalf("unexpected 5h window values: %+v", window)
	}

	weekly := windows[1]
	if weekly.ID != "meta-weekly" || weekly.PeriodUnit != "week" || weekly.PeriodValue == nil || *weekly.PeriodValue != 1 || weekly.WindowSeconds == nil || *weekly.WindowSeconds != 604800 {
		t.Fatalf("unexpected weekly window period: %+v", weekly)
	}
	if weekly.Used == nil || *weekly.Used != 25 || weekly.Remaining == nil || *weekly.Remaining != 75 || weekly.Status != "healthy" || weekly.ResetAt == nil {
		t.Fatalf("unexpected weekly window values: %+v", weekly)
	}
}

func TestParseMetaQuotaNonHourMinutesAndFallbackTier(t *testing.T) {
	now := time.Date(2026, 7, 16, 9, 0, 0, 0, time.UTC)
	body := []byte(`{
		"subs_usage": {
			"tier": "Muse Starter",
			"window": {"used_percent": -4, "window_duration_mins": 90, "resets_at": 0},
			"weekly": {"used_percent": 120}
		}
	}`)
	windows, plan, errParse := parseMetaQuota(body, now)
	if errParse != nil {
		t.Fatalf("parseMetaQuota() error = %v", errParse)
	}
	if plan == nil || plan.Name != "Muse Starter" {
		t.Fatalf("unexpected fallback plan: %+v", plan)
	}
	if windows[0].PeriodUnit != "minute" || windows[0].PeriodValue == nil || *windows[0].PeriodValue != 90 || windows[0].WindowSeconds == nil || *windows[0].WindowSeconds != 5400 {
		t.Fatalf("unexpected 90m window period: %+v", windows[0])
	}
	if windows[0].Used == nil || *windows[0].Used != 0 || windows[0].ResetAt != nil {
		t.Fatalf("expected clamped 0 used and nil resetAt: %+v", windows[0])
	}
	if windows[1].Used == nil || *windows[1].Used != 100 || windows[1].Status != "exhausted" {
		t.Fatalf("expected clamped 100 used exhausted weekly window: %+v", windows[1])
	}
}

func TestParseMetaQuotaUnknownWithoutUsageAndRejectsMalformed(t *testing.T) {
	now := time.Date(2026, 7, 16, 9, 0, 0, 0, time.UTC)
	for _, raw := range []string{
		`{}`,
		`{"api_key":"LLM|fixture"}`,
		`{"subs_usage":null}`,
		`{"subs_usage":{"window":{"used_percent":false},"weekly":{"used_percent":null}}}`,
	} {
		windows, _, errParse := parseMetaQuota([]byte(raw), now)
		if errParse != nil {
			t.Fatalf("parseMetaQuota(%s) error = %v", raw, errParse)
		}
		if len(windows) != 2 || windows[0].Used != nil || windows[0].Status != "unknown" || windows[1].Used != nil || windows[1].Status != "unknown" {
			t.Fatalf("expected unknown windows for %s, got %+v", raw, windows)
		}
	}

	for _, invalid := range []string{`{bad`, `null`, `[]`, `"text"`, `42`} {
		if _, _, errParse := parseMetaQuota([]byte(invalid), now); errParse == nil {
			t.Fatalf("parseMetaQuota(%s) succeeded, want error", invalid)
		}
	}
}

func TestCollectorPersistsMetaQuotaAndUsesDCATokenOnly(t *testing.T) {
	repo := newCollectorTestRepository(t)
	now := time.Date(2026, 7, 16, 9, 0, 0, 0, time.UTC)
	seedCollectorProviderAuth(t, repo, "meta-valid", "meta", map[string]any{
		"type":         "meta",
		"dca_token":    "dca:fixture-token",
		"access_token": "LLM|must-not-be-used",
		"api_key":      "LLM|must-not-be-used",
	})
	seedCollectorProviderAuth(t, repo, "meta-missing-dca", "meta", map[string]any{
		"type":         "meta",
		"access_token": "dca:wrong-field",
		"api_key":      "LLM|only",
	})

	var requestCount atomic.Int32
	var observedMethod, observedBody string
	var observedHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		observedMethod = r.Method
		observedHeaders = r.Header.Clone()
		bodyBytes, _ := io.ReadAll(r.Body)
		observedBody = string(bodyBytes)
		_, _ = w.Write([]byte(`{
			"subs_tier_name": "Muse Code Everyday Usage",
			"subs_usage": {
				"window": {"used_percent": 10, "window_duration_mins": 300, "resets_at": 1789678120},
				"weekly": {"used_percent": 5, "resets_at": 1789948800}
			}
		}`))
	}))
	defer server.Close()

	collector := NewCollector(repo, Options{
		Owner:        "home-a",
		MetaQuotaURL: server.URL,
		Now:          func() time.Time { return now },
	})
	collector.collect(context.Background())

	if got := requestCount.Load(); got != 1 {
		t.Fatalf("HTTP request count = %d, want 1 (missing dca_token must not call upstream)", got)
	}
	if observedMethod != http.MethodPost || observedBody != "{}" {
		t.Fatalf("method/body = %s %q, want POST {}", observedMethod, observedBody)
	}
	if observedHeaders.Get("Authorization") != "Bearer dca:fixture-token" || observedHeaders.Get(metaAPIVersionHeader) != metaAPIVersionValue || observedHeaders.Get("User-Agent") != metaMuseUserAgent {
		t.Fatalf("unexpected Meta request headers: %v", observedHeaders)
	}

	validItem, errValid := repo.GetQuotaCredential(context.Background(), "meta-valid", now.Add(time.Minute))
	if errValid != nil {
		t.Fatalf("GetQuotaCredential(meta-valid) error = %v", errValid)
	}
	if validItem.QuotaStatus != "healthy" || validItem.CollectionStatus != "success" {
		t.Fatalf("unexpected validItem status: %+v", validItem)
	}
	if validItem.Plan == nil || validItem.Plan.Name != "Muse Code Everyday Usage" {
		t.Fatalf("unexpected validItem plan: %+v", validItem.Plan)
	}
	if len(validItem.PrimaryWindows) != 2 || validItem.PrimaryWindows[0].ID != "meta-window" || validItem.PrimaryWindows[1].ID != "meta-weekly" {
		t.Fatalf("unexpected validItem primary windows: %+v", validItem.PrimaryWindows)
	}

	missingItem, errMissing := repo.GetQuotaCredential(context.Background(), "meta-missing-dca", now.Add(time.Minute))
	if errMissing != nil {
		t.Fatalf("GetQuotaCredential(meta-missing-dca) error = %v", errMissing)
	}
	if missingItem.CollectionStatus != "failed" || missingItem.Error == nil || missingItem.Error.Code != "DCA_TOKEN_UNAVAILABLE" {
		t.Fatalf("unexpected missingItem error state: %+v", missingItem)
	}
}
