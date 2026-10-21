package synthesizer

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

func mustSynthesizeAuthFile(t *testing.T, ctx *SynthesisContext, fullPath string, raw string) []*coreauth.Auth {
	t.Helper()
	auths, errSynthesize := SynthesizeAuthFile(ctx, fullPath, []byte(raw))
	if errSynthesize != nil {
		t.Fatalf("SynthesizeAuthFile(%s) error = %v", fullPath, errSynthesize)
	}
	return auths
}

func TestExtractExcludedModelsFromMetadata(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata map[string]any
		want     []string
	}{
		{name: "absent", metadata: map[string]any{}},
		{name: "null", metadata: map[string]any{"excluded_models": nil}},
		{name: "empty array", metadata: map[string]any{"excluded_models": []any{}}, want: []string{}},
		{name: "array", metadata: map[string]any{"excluded_models": []any{" a ", "", "b"}}, want: []string{"a", "b"}},
		{name: "string slice", metadata: map[string]any{"excluded_models": []string{"a"}}, want: []string{"a"}},
		{name: "single string", metadata: map[string]any{"excluded_models": "gemini-pro-agent"}, want: []string{"gemini-pro-agent"}},
		{name: "comma string", metadata: map[string]any{"excluded_models": " gemini-pro-agent , ,gemini-3-flash"}, want: []string{"gemini-pro-agent", "gemini-3-flash"}},
		{name: "blank string", metadata: map[string]any{"excluded_models": "  "}, want: []string{}},
		{name: "legacy key string", metadata: map[string]any{"excluded-models": "a,b"}, want: []string{"a", "b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, errExtract := ExtractExcludedModelsFromMetadata(tc.metadata)
			if errExtract != nil {
				t.Fatalf("ExtractExcludedModelsFromMetadata() error = %v", errExtract)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ExtractExcludedModelsFromMetadata() = %#v, want %#v", got, tc.want)
			}
		})
	}
	for _, tc := range []struct {
		value any
		want  []string
	}{
		{value: 123},
		{value: true},
		{value: map[string]any{}},
		{value: []any{"a", 123, " b "}, want: []string{"a", "b"}},
		{value: []any{nil}, want: []string{}},
	} {
		got, errExtract := ExtractExcludedModelsFromMetadata(map[string]any{"excluded_models": tc.value})
		if errExtract == nil {
			t.Fatalf("ExtractExcludedModelsFromMetadata(%#v) = %v, want error", tc.value, got)
		}
		// Valid string entries are still returned for lenient readers.
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("ExtractExcludedModelsFromMetadata(%#v) = %#v, want %#v", tc.value, got, tc.want)
		}
	}
}

func TestSynthesizeAuthFileHandlesInvalidExcludedModels(t *testing.T) {
	authDir := t.TempDir()
	ctx := &SynthesisContext{Config: &config.Config{}, AuthDir: authDir, Now: time.Date(2026, time.October, 10, 12, 0, 0, 0, time.UTC)}
	badPath := filepath.Join(authDir, "bad.json")

	// SynthesizeAuthFile returns the error for strict callers alongside the auth built from valid rules.
	for _, tc := range []struct {
		raw  string
		want string
	}{
		{raw: `{"type":"claude","email":"bad@example.test","excluded_models":123}`},
		{raw: `{"type":"claude","email":"bad@example.test","excluded_models":{"x":1}}`},
		{raw: `{"type":"claude","email":"bad@example.test","excluded_models":[null]}`},
		{raw: `{"type":"claude","email":"bad@example.test","excluded_models":["Model-A",null,1]}`, want: "model-a"},
	} {
		auths, errSynthesize := SynthesizeAuthFile(ctx, badPath, []byte(tc.raw))
		if errSynthesize == nil || !strings.Contains(errSynthesize.Error(), badPath) {
			t.Fatalf("%s: SynthesizeAuthFile() error = %v, want error naming %s", tc.raw, errSynthesize, badPath)
		}
		if len(auths) != 1 {
			t.Fatalf("%s: len(auths) = %d, want 1", tc.raw, len(auths))
		}
		if got := auths[0].Attributes["excluded_models"]; got != tc.want {
			t.Fatalf("%s: excluded_models = %q, want %q", tc.raw, got, tc.want)
		}
	}

	pluginCtx := *ctx
	pluginCtx.PluginAuthParser = multiAuthParser{t: t, parseAuths: func(context.Context, pluginapi.AuthParseRequest) ([]*coreauth.Auth, bool, error) {
		return []*coreauth.Auth{{ID: "plugin.json", Provider: "acme"}}, true, nil
	}}
	pluginPath := filepath.Join(authDir, "plugin.json")
	pluginAuths, errPlugin := SynthesizeAuthFile(&pluginCtx, pluginPath, []byte(`{"type":"acme","excluded_models":["a",false]}`))
	if errPlugin == nil || !strings.Contains(errPlugin.Error(), pluginPath) {
		t.Fatalf("SynthesizeAuthFile(plugin) error = %v, want error naming %s", errPlugin, pluginPath)
	}
	if len(pluginAuths) != 1 || pluginAuths[0].Attributes["excluded_models"] != "a" {
		t.Fatalf("plugin auths = %+v, want one auth keeping the valid rule", pluginAuths)
	}

	// A directory scan logs a warning and keeps both credentials.
	if errWrite := os.WriteFile(badPath, []byte(`{"type":"claude","email":"bad@example.test","excluded_models":123}`), 0o600); errWrite != nil {
		t.Fatalf("write bad auth file: %v", errWrite)
	}
	if errWrite := os.WriteFile(filepath.Join(authDir, "good.json"), []byte(`{"type":"claude","email":"good@example.test"}`), 0o600); errWrite != nil {
		t.Fatalf("write good auth file: %v", errWrite)
	}
	scanned, errScan := NewFileSynthesizer().Synthesize(ctx)
	if errScan != nil || len(scanned) != 2 {
		t.Fatalf("FileSynthesizer.Synthesize() = (%v, %v), want bad.json and good.json", scanned, errScan)
	}

	auths := mustSynthesizeAuthFile(t, ctx, filepath.Join(authDir, "legacy.json"), `{"type":"claude","email":"legacy@example.test","excluded_models":"Model-A,model-b"}`)
	if len(auths) != 1 || auths[0].Attributes["excluded_models"] != "model-a,model-b" {
		t.Fatalf("legacy string auths = %+v, want comma string split into two rules", auths)
	}
}

func TestSynthesizeAuthFileDoesNotPersistGlobalOAuthExclusions(t *testing.T) {
	authDir := t.TempDir()
	ctx := &SynthesisContext{
		Config:  &config.Config{OAuthExcludedModels: map[string][]string{"claude": {"global-model"}}},
		AuthDir: authDir,
		Now:     time.Date(2026, time.October, 10, 12, 0, 0, 0, time.UTC),
	}

	inherited := mustSynthesizeAuthFile(t, ctx, filepath.Join(authDir, "inherit.json"), `{"type":"claude","email":"inherit@example.test"}`)
	if len(inherited) != 1 {
		t.Fatalf("len(inherited) = %d, want 1", len(inherited))
	}
	if value, ok := inherited[0].Attributes["excluded_models"]; ok {
		t.Fatalf("inherited excluded_models = %q, want absent so global config is resolved at runtime", value)
	}
	if value, ok := inherited[0].Attributes["excluded_models_hash"]; ok {
		t.Fatalf("inherited excluded_models_hash = %q, want absent", value)
	}

	own := mustSynthesizeAuthFile(t, ctx, filepath.Join(authDir, "own.json"), `{"type":"claude","email":"own@example.test","excluded_models":["Own-Model"]}`)
	if len(own) != 1 {
		t.Fatalf("len(own) = %d, want 1", len(own))
	}
	if got, want := own[0].Attributes["excluded_models"], "own-model"; got != want {
		t.Fatalf("own excluded_models = %q, want %q", got, want)
	}
}
