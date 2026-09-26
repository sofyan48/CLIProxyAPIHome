package quota

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

// ResetCreditError contains only safe, bounded error codes; upstream bodies are never returned.
type ResetCreditError struct {
	Status int
	Code   string
}

func (e *ResetCreditError) Error() string { return e.Code }

func resetCreditError(status int, code string) error {
	return &ResetCreditError{Status: status, Code: code}
}

func resetOutcome(value any) string {
	if object, ok := value.(map[string]any); ok {
		for _, key := range []string{"code", "outcome", "status", "result", "type"} {
			if text, ok := object[key].(string); ok && strings.TrimSpace(text) != "" {
				value = text
				break
			}
		}
	}
	text, _ := value.(string)
	var normalized strings.Builder
	for _, char := range strings.ToLower(strings.TrimSpace(text)) {
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' {
			normalized.WriteRune(char)
		}
	}
	return normalized.String()
}

func resetOutcomeError(value any) error {
	switch resetOutcome(value) {
	case "nocredit", "nocredits":
		return resetCreditError(http.StatusConflict, "no_credit")
	case "nothingtoreset":
		return resetCreditError(http.StatusConflict, "nothing_to_reset")
	}
	return nil
}

func resetCreditCandidates(payload any) []any {
	if items, ok := payload.([]any); ok {
		return items
	}
	object, _ := payload.(map[string]any)
	for _, key := range []string{"credits", "reset_credits", "resetCredits", "rate_limit_reset_credits", "rateLimitResetCredits", "items", "data"} {
		if items, ok := object[key].([]any); ok {
			return items
		}
	}
	return nil
}

func resetCreditField(object map[string]any, keys ...string) string {
	for _, key := range keys {
		if text, ok := object[key].(string); ok && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return ""
}

func selectCodexResetCredit(payload any, now time.Time) string {
	selected := ""
	var earliest time.Time
	for _, candidate := range resetCreditCandidates(payload) {
		object, ok := candidate.(map[string]any)
		if !ok || object["consumed"] == true || object["redeemed"] == true || object["available"] == false {
			continue
		}
		switch resetOutcome(resetCreditField(object, "status", "state", "outcome", "result", "code")) {
		case "consumed", "redeeming", "redeemed", "used", "expired", "unavailable":
			continue
		}
		id := resetCreditField(object, "credit_id", "creditId", "id")
		if id == "" {
			continue
		}
		expiry := resetCreditField(object, "expires_at", "expiresAt", "expiration_at", "expirationAt")
		var expiresAt time.Time
		if expiry != "" {
			var errParse error
			expiresAt, errParse = time.Parse(time.RFC3339Nano, expiry)
			if errParse != nil || !expiresAt.After(now) {
				continue
			}
		}
		if selected == "" || (!expiresAt.IsZero() && (earliest.IsZero() || expiresAt.Before(earliest))) {
			selected, earliest = id, expiresAt
		}
	}
	return selected
}

func (c *Collector) resetCreditRequest(ctx context.Context, authToken string, accountID string, authProxyClient *http.Client, method, target string, body []byte) (any, int, error) {
	requestCtx, cancel := context.WithTimeout(ctx, c.options.ProbeTimeout)
	defer cancel()
	req, errRequest := http.NewRequestWithContext(requestCtx, method, target, bytes.NewReader(body))
	if errRequest != nil {
		return nil, 0, resetCreditError(http.StatusBadGateway, "upstream_unavailable")
	}
	req.Header.Set("Authorization", "Bearer "+authToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", codexUserAgent)
	req.Header.Set("originator", "codex_cli_rs")
	req.Header.Set("Version", "0.76.0")
	if accountID != "" {
		req.Header.Set("Chatgpt-Account-Id", accountID)
	}
	resp, errDo := authProxyClient.Do(req)
	if errDo != nil {
		return nil, 0, resetCreditError(http.StatusBadGateway, "upstream_unavailable")
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.WithError(errClose).Debug("reset credit: close response body failed")
		}
	}()
	data, errRead := io.ReadAll(io.LimitReader(resp.Body, maxProbeResponseBytes+1))
	if errRead != nil || len(data) > maxProbeResponseBytes {
		return nil, 0, resetCreditError(http.StatusBadGateway, "upstream_response_invalid")
	}
	var payload any
	if errDecode := json.Unmarshal(data, &payload); errDecode != nil {
		return nil, resp.StatusCode, resetCreditError(http.StatusBadGateway, "unknown_reset_credit_response")
	}
	return payload, resp.StatusCode, nil
}

// ConsumeCodexResetCredit reads a live credit and makes at most one POST per
// caller key. A reservation is retained on every outcome, including timeouts.
func (c *Collector) ConsumeCodexResetCredit(ctx context.Context, credentialID, requestKey string) (string, error) {
	if c == nil || c.repo == nil {
		return "", resetCreditError(http.StatusNotFound, "reset_credit_unsupported")
	}
	auth, _, errGet := c.repo.GetAuth(ctx, credentialID)
	if errGet != nil {
		if errors.Is(errGet, gorm.ErrRecordNotFound) {
			return "", resetCreditError(http.StatusNotFound, "quota_credential_not_found")
		}
		return "", resetCreditError(http.StatusInternalServerError, "credential_load_failed")
	}
	if !strings.EqualFold(auth.Provider, "codex") || quotaProviderAPIKeyAuth(auth) || (quotaExplicitAuthKind(auth) != "oauth" && quotaMetadataString(auth.Metadata, "type") != "codex") {
		return "", resetCreditError(http.StatusBadRequest, "codex_oauth_required")
	}
	requestKey = strings.TrimSpace(requestKey)
	if requestKey == "" || len(requestKey) > 256 {
		return "", resetCreditError(http.StatusBadRequest, "idempotency_key_required")
	}
	token := quotaAccessToken(auth)
	if token == "" {
		return "", resetCreditError(http.StatusUnauthorized, "codex_access_token_missing")
	}
	client, errClient := c.options.HTTPClient(auth, c.options.ProbeTimeout)
	if errClient != nil {
		return "", resetCreditError(http.StatusBadGateway, "proxy_configuration_invalid")
	}
	if client == nil {
		return "", resetCreditError(http.StatusBadGateway, "proxy_configuration_invalid")
	}
	// Never forward the OAuth bearer or repeat a consume POST on a redirect.
	clientCopy := *client
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	accountID := quotaMetadataString(auth.Metadata, "account_id", "accountId", "chatgpt_account_id", "chatgptAccountId")
	payload, status, errList := c.resetCreditRequest(ctx, token, accountID, &clientCopy, http.MethodGet, c.options.CodexResetCreditsURL, nil)
	if errList != nil {
		return "", errList
	}
	if known := resetOutcomeError(payload); known != nil {
		return "", known
	}
	if status < 200 || status >= 300 {
		return "", resetCreditError(http.StatusBadGateway, "codex_reset_credit_upstream_error")
	}
	if resetCreditCandidates(payload) == nil {
		return "", resetCreditError(http.StatusBadGateway, "unknown_reset_credit_response")
	}
	creditID := selectCodexResetCredit(payload, c.options.Now().UTC())
	if creditID == "" {
		return "", resetCreditError(http.StatusConflict, "no_credit")
	}
	// Reserve before POST so concurrent requests and process restarts cannot
	// submit the same caller key twice, even if the upstream result is ambiguous.
	if errReserve := c.repo.ReserveCodexResetRequest(ctx, credentialID, requestKey); errReserve != nil {
		if errors.Is(errReserve, cluster.ErrCodexResetRequestUsed) {
			return "", resetCreditError(http.StatusConflict, "reset_request_already_submitted")
		}
		return "", resetCreditError(http.StatusInternalServerError, "reset_request_reservation_failed")
	}
	encoded, _ := json.Marshal(map[string]string{"redeem_request_id": requestKey, "credit_id": creditID})
	// The POST target is fixed; never derive it from a caller-supplied URL.
	payload, status, errConsume := c.resetCreditRequest(ctx, token, accountID, &clientCopy, http.MethodPost, c.options.CodexResetCreditsURL+"/consume", encoded)
	if errConsume != nil {
		return "", errConsume
	}
	if known := resetOutcomeError(payload); known != nil {
		return "", known
	}
	if status < 200 || status >= 300 {
		return "", resetCreditError(http.StatusBadGateway, "codex_reset_credit_upstream_error")
	}
	switch resetOutcome(payload) {
	case "reset":
		return "reset", nil
	case "alreadyredeemed":
		return "alreadyRedeemed", nil
	default:
		return "", resetCreditError(http.StatusBadGateway, "unknown_reset_credit_response")
	}
}
