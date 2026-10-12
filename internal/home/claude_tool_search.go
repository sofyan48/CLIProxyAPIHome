package home

import (
	"net/url"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
)

// claudeToolSearchLegacyPrefixes and claudeToolSearchLegacyModels list the
// Anthropic models that predate tool search (Claude Opus 4.1 and earlier). They
// mirror helps.ClaudeModelLacksToolSearch in CLIProxyAPI; keep both in sync.
var claudeToolSearchLegacyPrefixes = []string{"claude-3", "claude-2", "claude-instant"}

var claudeToolSearchLegacyModels = map[string]struct{}{
	"claude-opus-4":            {},
	"claude-opus-4-0":          {},
	"claude-opus-4-20250514":   {},
	"claude-opus-4-1":          {},
	"claude-opus-4-1-20250805": {},
	"claude-sonnet-4":          {},
	"claude-sonnet-4-0":        {},
	"claude-sonnet-4-20250514": {},
}

// ClaudeNativeToolSearchSupported reports whether a Claude credential can take
// Codex tool_search as Anthropic native tool search for an upstream model. It
// mirrors ClaudeExecutor.SupportsNativeToolSearch in CLIProxyAPI: Anthropic's
// own origin qualifies, a custom base URL needs native_tool_search, and models
// that predate tool search never qualify.
func ClaudeNativeToolSearchSupported(auth *coreauth.Auth, upstreamModel string) bool {
	if auth == nil || claudeModelLacksToolSearch(upstreamModel) {
		return false
	}
	var baseURL, forced string
	if auth.Attributes != nil {
		baseURL = strings.TrimSpace(auth.Attributes["base_url"])
		forced = strings.TrimSpace(auth.Attributes["native_tool_search"])
	}
	if baseURL == "" || isAnthropicBaseURL(baseURL) {
		return true
	}
	return strings.EqualFold(forced, "true")
}

func isAnthropicBaseURL(baseURL string) bool {
	parsed, errParse := url.Parse(baseURL)
	if errParse != nil || parsed.User != nil || !strings.EqualFold(parsed.Scheme, "https") || !strings.EqualFold(parsed.Hostname(), "api.anthropic.com") {
		return false
	}
	port := parsed.Port()
	return port == "" || port == "443"
}

func claudeModelLacksToolSearch(model string) bool {
	model = strings.ToLower(coreauth.CanonicalModelID(model))
	if slash := strings.LastIndexByte(model, '/'); slash >= 0 {
		model = model[slash+1:]
	}
	if bracket := strings.IndexByte(model, '['); bracket >= 0 {
		model = model[:bracket]
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	if _, legacy := claudeToolSearchLegacyModels[model]; legacy {
		return true
	}
	for _, prefix := range claudeToolSearchLegacyPrefixes {
		if strings.HasPrefix(model, prefix) {
			return true
		}
	}
	return false
}
