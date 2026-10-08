package userapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

type userRequestLogsPayload struct {
	Items   []map[string]any                 `json:"items"`
	Total   int64                            `json:"total"`
	Limit   int                              `json:"limit"`
	Offset  int                              `json:"offset"`
	Summary cluster.UserRequestLogAggregates `json:"summary"`
}

func requestUserLogs(t *testing.T, router http.Handler, token, query string, status int) userRequestLogsPayload {
	t.Helper()
	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/user/request-logs"+query, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	router.ServeHTTP(resp, req)
	if resp.Code != status {
		t.Fatalf("%s status=%d want=%d body=%s", query, resp.Code, status, resp.Body.String())
	}
	var payload userRequestLogsPayload
	if status == http.StatusOK {
		if errDecode := json.Unmarshal(resp.Body.Bytes(), &payload); errDecode != nil {
			t.Fatal(errDecode)
		}
		var envelope map[string]any
		if errDecode := json.Unmarshal(resp.Body.Bytes(), &envelope); errDecode != nil {
			t.Fatal(errDecode)
		}
		assertKeys := func(value map[string]any, want ...string) {
			t.Helper()
			keys := make([]string, 0, len(value))
			for key := range value {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			sort.Strings(want)
			if !reflect.DeepEqual(keys, want) {
				t.Fatalf("unsafe response keys: %v want %v; body=%s", keys, want, resp.Body.String())
			}
		}
		assertKeys(envelope, "items", "total", "limit", "offset", "summary")
		summary, ok := envelope["summary"].(map[string]any)
		if !ok || payload.Items == nil || payload.Summary.ByModel == nil {
			t.Fatalf("invalid envelope: %s", resp.Body.String())
		}
		assertKeys(summary, "by_model", "average_latency_ms", "status")
		assertKeys(summary["status"].(map[string]any), "success", "failed")
		for _, model := range summary["by_model"].([]any) {
			assertKeys(model.(map[string]any), "model", "requests")
		}
		var modelTotal int64
		for _, model := range payload.Summary.ByModel {
			modelTotal += model.Requests
		}
		if modelTotal != payload.Total || payload.Summary.Status.Success+payload.Summary.Status.Failed != payload.Total {
			t.Fatalf("summary counts disagree: %#v", payload)
		}
		for _, item := range payload.Items {
			keys := make([]string, 0, len(item))
			for key := range item {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			want := []string{"event_type", "latency_ms", "model", "request_id", "status", "timestamp", "tokens"}
			if !reflect.DeepEqual(keys, want) {
				t.Fatalf("unsafe item keys: %v", keys)
			}
			for _, key := range []string{"event_type", "model", "request_id", "status", "timestamp"} {
				if _, ok := item[key].(string); !ok {
					t.Fatalf("%s must be string: %#v", key, item)
				}
			}
			for _, key := range []string{"tokens", "latency_ms"} {
				if _, ok := item[key].(float64); !ok {
					t.Fatalf("%s must be number: %#v", key, item)
				}
			}
		}
		if strings.Contains(resp.Body.String(), "sensitive-marker") {
			t.Fatalf("metadata leaked: %s", resp.Body.String())
		}
	}
	return payload
}

func TestUserRequestLogsOwnershipAndSafeSummary(t *testing.T) {
	t.Parallel()
	h, closeRepo := newUserBillingTestHandler(t)
	defer closeRepo()
	first, second := seedUserBillingCharges(t, h)
	firstToken := createUserBillingBearerToken(t, h, first.ID)
	secondToken := createUserBillingBearerToken(t, h, second.ID)
	router := gin.New()
	Register(router.Group("/user"), h)
	ctx := context.Background()
	// Payload identities must not establish ownership. Failed and non-billable
	// records have no billing snapshot even if the key currently belongs to first.
	for _, raw := range []string{
		`{"timestamp":"2026-06-10T02:00:00Z","api_key":"unknown-key","request_id":"unknown","tokens":{"total_tokens":42},"user_id":1,"user":"first-user"}`,
		`{"timestamp":"2026-06-10T02:01:00Z","api_key":"first-client-key","request_id":"failed-unattributed","failed":true,"tokens":{"total_tokens":42}}`,
		`{"timestamp":"2026-06-10T02:02:00Z","api_key":"first-client-key","request_id":"nonbillable","tokens":{"total_tokens":0}}`,
		`{"timestamp":"2026-06-10T02:03:00Z","api_key":"first-client-key","request_id":"safe-summary","event_type":"sensitive-marker","model":"gpt-4.1-mini","latency_ms":123,"provider":"sensitive-marker","endpoint":"sensitive-marker","client_ip":"sensitive-marker","routing":{"secret":"sensitive-marker"},"download_url":"sensitive-marker","fail_body":"sensitive-marker","tokens":{"input_tokens":10,"output_tokens":2,"total_tokens":12}}`,
	} {
		if _, errAppend := h.repo.AppendUsage(ctx, raw, "sensitive-marker"); errAppend != nil {
			t.Fatal(errAppend)
		}
	}
	assertRequests := func(token, query string, want ...string) {
		t.Helper()
		payload := requestUserLogs(t, router, token, query, http.StatusOK)
		got := make([]string, 0, len(payload.Items))
		for _, item := range payload.Items {
			got = append(got, item["request_id"].(string))
		}
		if payload.Total != int64(len(want)) || !reflect.DeepEqual(got, append([]string{}, want...)) {
			t.Fatalf("query=%s total=%d items=%v want=%v", query, payload.Total, got, want)
		}
		wantSummary := cluster.UserRequestLogAggregates{ByModel: []cluster.UserRequestLogModelCount{}, Status: cluster.UserRequestLogStatusCounts{Success: int64(len(want))}}
		if len(want) > 0 {
			wantSummary.ByModel = append(wantSummary.ByModel, cluster.UserRequestLogModelCount{Model: "gpt-4.1-mini", Requests: int64(len(want))})
		}
		for _, id := range want {
			if id == "safe-summary" {
				latency := 123.0
				wantSummary.AverageLatencyMS = &latency
			}
		}
		if !reflect.DeepEqual(payload.Summary, wantSummary) {
			t.Fatalf("query=%s summary=%#v want=%#v", query, payload.Summary, wantSummary)
		}
	}
	assertRequests(firstToken, "", "safe-summary", "req-first")
	assertRequests(secondToken, "", "req-second")
	for _, query := range []string{
		fmt.Sprintf("?user_id=%d&user=second-user&api_key=second-client-key&key=second-client-key&client_key_id=2", second.ID),
		"?search=sensitive-marker&q=second-user&provider=sensitive-marker&status=failed",
		"?request_id=req-first",
	} {
		if query == "?request_id=req-first" {
			assertRequests(firstToken, query, "req-first")
		} else {
			assertRequests(firstToken, query, "safe-summary", "req-first")
		}
	}
	assertRequests(firstToken, "?request_id=req-second")
	assertRequests(secondToken, "?request_id=req-first&user_id=1")
	// Eight characters must not use the admin suffix lookup.
	assertRequests(firstToken, "?request_id=eq-first")
	assertRequests(firstToken, "?request_id=%25")
	item := requestUserLogs(t, router, firstToken, "?request_id=safe-summary", http.StatusOK).Items[0]
	if item["event_type"] != "unknown" || item["status"] != "success" || item["tokens"] != float64(12) || item["latency_ms"] != float64(123) {
		t.Fatalf("summary = %#v", item)
	}
	fullSummary := requestUserLogs(t, router, firstToken, "", http.StatusOK).Summary
	page := requestUserLogs(t, router, firstToken, "?limit=1&offset=1", http.StatusOK)
	if !reflect.DeepEqual(page.Summary, fullSummary) {
		t.Fatalf("pagination changed summary: %#v", page)
	}
	if page.Total != 2 || page.Limit != 1 || page.Offset != 1 || len(page.Items) != 1 || page.Items[0]["request_id"] != "req-first" {
		t.Fatalf("page = %#v", page)
	}
	page = requestUserLogs(t, router, firstToken, "?limit=999&offset=999", http.StatusOK)
	if page.Total != 2 || page.Limit != 200 || page.Offset != 999 || len(page.Items) != 0 || !reflect.DeepEqual(page.Summary, fullSummary) {
		t.Fatalf("clamped empty page = %#v", page)
	}
	if _, errReassign := h.repo.UpdateAPIKeyBindings(ctx, "first-client-key", &second.ID, nil, nil); errReassign != nil {
		t.Fatal(errReassign)
	}
	assertRequests(firstToken, "", "safe-summary", "req-first")
	assertRequests(secondToken, "", "req-second")
	if errDeleteKey := h.repo.DeleteAPIKeyForUser(ctx, second.ID, 0, "first-client-key"); errDeleteKey != nil {
		t.Fatal(errDeleteKey)
	}
	assertRequests(firstToken, "", "safe-summary", "req-first")
	assertRequests(secondToken, "", "req-second")
	if errDeleteUser := h.repo.DeleteUser(ctx, first.ID); errDeleteUser != nil {
		t.Fatal(errDeleteUser)
	}
	requestUserLogs(t, router, firstToken, "", http.StatusUnauthorized)
	assertRequests(secondToken, "", "req-second")
}

func TestUserRequestLogsAuthDatesAndPagination(t *testing.T) {
	t.Parallel()
	h, closeRepo := newUserBillingTestHandler(t)
	defer closeRepo()
	first, _ := seedUserBillingCharges(t, h)
	token := createUserBillingBearerToken(t, h, first.ID)
	router := gin.New()
	Register(router.Group("/user"), h)
	requestUserLogs(t, router, "", "", http.StatusUnauthorized)
	requestUserLogs(t, router, "invalid-token", "", http.StatusUnauthorized)
	for _, query := range []string{
		"?from=invalid", "?to=2026-06-10", "?from=1780000000", "?from=", "?to=",
		"?from=2026-06-11T00:00:00Z&to=2026-06-10T00:00:00Z",
		"?limit=0", "?limit=-1", "?limit=abc", "?limit=999999999999999999999999", "?offset=-1", "?offset=abc", "?offset=999999999999999999999999",
	} {
		requestUserLogs(t, router, token, query, http.StatusBadRequest)
	}
	for _, test := range []struct {
		from, to string
		total    int64
	}{
		{"2026-06-10T01:02:03Z", "2026-06-10T01:02:04Z", 1},
		{"2026-06-10T00:00:00Z", "2026-06-10T01:02:03Z", 0},
		{"2026-06-10T01:02:03Z", "2026-06-10T01:02:03Z", 0},
		{"2026-06-10T09:02:03+08:00", "2026-06-10T09:02:04+08:00", 1},
	} {
		query := "?" + url.Values{"from": {test.from}, "to": {test.to}}.Encode()
		got := requestUserLogs(t, router, token, query, http.StatusOK)
		wantSummary := cluster.UserRequestLogAggregates{ByModel: []cluster.UserRequestLogModelCount{}, Status: cluster.UserRequestLogStatusCounts{Success: test.total}}
		if test.total > 0 {
			wantSummary.ByModel = append(wantSummary.ByModel, cluster.UserRequestLogModelCount{Model: "gpt-4.1-mini", Requests: test.total})
		}
		if got.Total != test.total || !reflect.DeepEqual(got.Summary, wantSummary) {
			t.Fatalf("%s payload=%#v want total=%d summary=%#v", query, got, test.total, wantSummary)
		}
	}
	pending := true
	if _, errUpdate := h.repo.UpdateUser(context.Background(), first.ID, cluster.UserUpdate{ApprovalPending: &pending}); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	requestUserLogs(t, router, token, "", http.StatusForbidden)
}
