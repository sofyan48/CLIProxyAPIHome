package quota

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

const claudeResetCreditsUserAgent = "claude-cli/2.1.304 (external, cli)"

func (c *Collector) probeClaudeResetCredits(ctx context.Context, auth *coreauth.Auth, headers http.Header) (*cluster.QuotaResetCredits, *probeError) {
	target, errURL := url.Parse(c.options.ClaudeUsageURL)
	if errURL != nil {
		return nil, &probeError{code: "PROBE_REQUEST_INVALID", message: "Claude usage URL is invalid."}
	}
	query := target.Query()
	query.Set("cedar_ember", "1")
	query.Set("skip_spend", "1")
	target.RawQuery = query.Encode()
	resetHeaders := headers.Clone()
	resetHeaders.Set("User-Agent", claudeResetCreditsUserAgent)
	body, _, errRequest := c.probeRequest(ctx, auth, http.MethodGet, target.String(), nil, resetHeaders)
	if errRequest != nil {
		return nil, errRequest
	}
	credits, errParse := parseClaudeResetCredits(body, c.options.Now().UTC())
	if errParse != nil {
		return nil, &probeError{code: "RESET_CREDITS_RESPONSE_INVALID", message: "Claude reset credits response could not be parsed.", retryable: true}
	}
	return credits, nil
}

// parseClaudeResetCredits normalizes the grant counts returned by Claude Code's
// read-only /api/oauth/usage?cedar_ember=1&skip_spend=1 request (CLI 2.1.304).
// A grant can cover multiple resets, so it is not a Codex-style individual credit.
func parseClaudeResetCredits(body []byte, observedAt time.Time) (*cluster.QuotaResetCredits, error) {
	var payload struct {
		FiveHour   json.RawMessage `json:"five_hour"`
		SevenDay   json.RawMessage `json:"seven_day"`
		ExtraUsage json.RawMessage `json:"extra_usage"`
		Grants     *struct {
			Eligible         *bool  `json:"eligible"`
			IneligibleReason string `json:"ineligible_reason"`
			Grants           []struct {
				ID         string `json:"id"`
				ResetsLeft *int   `json:"resets_left"`
				StartsAt   string `json:"starts_at"`
				EndsAt     string `json:"ends_at"`
				Paused     bool   `json:"paused"`
			} `json:"grants"`
		} `json:"cedar_ember"`
	}
	if errDecode := json.Unmarshal(body, &payload); errDecode != nil {
		return nil, fmt.Errorf("decode Claude reset grants: %w", errDecode)
	}
	if payload.Grants == nil {
		if len(payload.FiveHour)+len(payload.SevenDay)+len(payload.ExtraUsage) == 0 {
			return nil, fmt.Errorf("Claude reset response does not contain usage data")
		}
		return nil, nil
	}
	status := payload.Grants
	if status.Eligible == nil || status.IneligibleReason == "unavailable" {
		return nil, fmt.Errorf("Claude reset grant status is unavailable")
	}
	if !*status.Eligible {
		return nil, nil
	}
	if status.Grants == nil {
		return nil, fmt.Errorf("Claude reset grants are missing")
	}
	count := 0
	seen := make(map[string]struct{}, len(status.Grants))
	for _, grant := range status.Grants {
		id := strings.TrimSpace(grant.ID)
		if id == "" || grant.ResetsLeft == nil || *grant.ResetsLeft < 0 || *grant.ResetsLeft > 1_000_000 {
			return nil, fmt.Errorf("invalid Claude reset grant")
		}
		if _, exists := seen[id]; exists {
			return nil, fmt.Errorf("duplicate Claude reset grant")
		}
		seen[id] = struct{}{}
		active := !grant.Paused
		if grant.StartsAt != "" {
			startsAt, errStart := time.Parse(time.RFC3339Nano, grant.StartsAt)
			if errStart != nil {
				return nil, fmt.Errorf("invalid Claude reset grant start")
			}
			active = active && !startsAt.After(observedAt)
		}
		if grant.EndsAt != "" {
			endsAt, errEnd := time.Parse(time.RFC3339Nano, grant.EndsAt)
			if errEnd != nil {
				return nil, fmt.Errorf("invalid Claude reset grant expiry")
			}
			active = active && endsAt.After(observedAt)
		}
		if active {
			count += *grant.ResetsLeft
			if count > 1_000_000 {
				return nil, fmt.Errorf("Claude reset grant count is out of range")
			}
		}
	}
	return &cluster.QuotaResetCredits{
		AvailableCount: &count,
		ObservedAt:     observedAt.UTC(),
		Credits:        []cluster.QuotaResetCredit{},
	}, nil
}
