package quota

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestConsumeCodexResetCreditSelectsAndReserves(t *testing.T) {
	repo := newCollectorTestRepository(t)
	seedCollectorAuth(t, repo, "codex-reset", map[string]any{"type": "codex", "access_token": "private-token", "account_id": "account-1"})
	now := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer private-token" || request.Header.Get("Chatgpt-Account-Id") != "account-1" || request.Header.Get("User-Agent") == "" || request.Header.Get("originator") == "" || request.Header.Get("Version") != "0.76.0" {
			t.Error("missing Codex identity headers")
		}
		if request.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"credits":[{"id":"expired","expires_at":"2026-09-25T00:00:00Z"},{"id":"later","expires_at":"2026-09-29T00:00:00Z"},{"credit_id":"soon","expires_at":"2026-09-27T00:00:00Z"},{"id":"used","status":"redeemed"}]}`))
			return
		}
		posts.Add(1)
		var body map[string]string
		if errDecode := json.NewDecoder(request.Body).Decode(&body); errDecode != nil || body["redeem_request_id"] != "stable-key" || body["credit_id"] != "soon" || request.URL.Path != "/resets/consume" {
			t.Errorf("unexpected consume body/path: %#v %s %v", body, request.URL.Path, errDecode)
		}
		_, _ = w.Write([]byte(`{"outcome":"alreadyRedeemed"}`))
	}))
	defer server.Close()
	collector := NewCollector(repo, Options{CodexResetCreditsURL: server.URL + "/resets", Now: func() time.Time { return now }})
	outcome, errConsume := collector.ConsumeCodexResetCredit(context.Background(), "codex-reset", "stable-key")
	if errConsume != nil || outcome != "alreadyRedeemed" {
		t.Fatalf("consume outcome=%q error=%v", outcome, errConsume)
	}
	_, errDuplicate := collector.ConsumeCodexResetCredit(context.Background(), "codex-reset", "stable-key")
	if errDuplicate == nil || errDuplicate.Error() != "reset_request_already_submitted" || posts.Load() != 1 {
		t.Fatalf("duplicate error=%v posts=%d", errDuplicate, posts.Load())
	}
}

func TestCodexResetConsumeOutcomesAndSelection(t *testing.T) {
	now := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
	if id := selectCodexResetCredit(map[string]any{"items": []any{map[string]any{"id": "x", "expires_at": "invalid"}, map[string]any{"id": "y", "status": "available"}}}, now); id != "y" {
		t.Fatalf("selected %q", id)
	}
	for _, test := range []struct {
		input any
		want  string
	}{
		{map[string]any{"code": "noCredit"}, "no_credit"},
		{map[string]any{"status": "nothingToReset"}, "nothing_to_reset"},
		{map[string]any{"outcome": "unexpected"}, ""},
	} {
		errOutcome := resetOutcomeError(test.input)
		if test.want == "" && errOutcome != nil || test.want != "" && (errOutcome == nil || errOutcome.Error() != test.want) {
			t.Fatalf("outcome %#v: %v", test.input, errOutcome)
		}
	}
}
