package home

import (
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
)

func TestClaudeNativeToolSearchSupported(t *testing.T) {
	const gateway = "https://gateway.example.com"
	for _, tc := range []struct {
		name  string
		attrs map[string]string
		model string
		want  bool
	}{
		{name: "default origin", attrs: map[string]string{"api_key": "k"}, model: "claude-sonnet-4-5", want: true},
		{name: "explicit anthropic", attrs: map[string]string{"api_key": "k", "base_url": "https://api.anthropic.com"}, model: "claude-sonnet-4-5", want: true},
		{name: "anthropic with port", attrs: map[string]string{"api_key": "k", "base_url": "https://api.anthropic.com:443/"}, model: "claude-sonnet-4-5", want: true},
		{name: "plain http anthropic", attrs: map[string]string{"api_key": "k", "base_url": "http://api.anthropic.com"}, model: "claude-sonnet-4-5", want: false},
		{name: "userinfo anthropic", attrs: map[string]string{"api_key": "k", "base_url": "https://user@api.anthropic.com"}, model: "claude-sonnet-4-5", want: false},
		{name: "gateway", attrs: map[string]string{"api_key": "k", "base_url": gateway}, model: "claude-sonnet-4-5", want: false},
		{name: "gateway forced", attrs: map[string]string{"api_key": "k", "base_url": gateway, "native_tool_search": "TRUE"}, model: "claude-sonnet-4-5", want: true},
		{name: "legacy model on anthropic", attrs: map[string]string{"api_key": "k"}, model: "claude-opus-4-1-20250805", want: false},
		{name: "legacy model with suffix", attrs: map[string]string{"api_key": "k"}, model: "claude-sonnet-4-20250514(high)", want: false},
		{name: "legacy model on forced gateway", attrs: map[string]string{"api_key": "k", "base_url": gateway, "native_tool_search": "true"}, model: "claude-3-7-sonnet-20250219", want: false},
		{name: "prefixed legacy model", attrs: map[string]string{"api_key": "k"}, model: "team/claude-opus-4-1", want: false},
		{name: "bracketed legacy model", attrs: map[string]string{"api_key": "k"}, model: "claude-sonnet-4[1m]", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClaudeNativeToolSearchSupported(&coreauth.Auth{Attributes: tc.attrs}, tc.model); got != tc.want {
				t.Fatalf("ClaudeNativeToolSearchSupported() = %v, want %v", got, tc.want)
			}
		})
	}
	if ClaudeNativeToolSearchSupported(nil, "claude-sonnet-4-5") {
		t.Fatal("nil auth must not support native tool search")
	}
}
