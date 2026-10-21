package cluster

import (
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
)

func TestExportCodexCredentialKeepsConfigurationUpdateSupport(t *testing.T) {
	auth := &coreauth.Auth{Provider: "codex", Metadata: map[string]any{"home_config_models": []any{map[string]any{
		"id": "public", "name": "private", "user_defined": true, "support_configuration_update": true,
	}}}}
	models := credentialCodexModels(auth)
	if len(models) != 1 || !models[0].SupportConfigurationUpdate {
		t.Fatalf("exported codex models = %+v, want enabled", models)
	}
}

func TestExportCodexCredentialSkipsNonUserBuiltins(t *testing.T) {
	auth := &coreauth.Auth{Provider: "codex", Metadata: map[string]any{"home_config_models": []any{
		map[string]any{"id": "gpt-image-2", "user_defined": false},
		map[string]any{"id": "gpt-image-2.5", "user_defined": false},
		map[string]any{"id": "gpt-image-2.5-flare", "user_defined": false},
		map[string]any{"id": "gpt-image-2.5-sunburst", "user_defined": false},
		map[string]any{"id": "custom", "name": "upstream", "user_defined": true},
		map[string]any{"id": "gpt-image-2.5", "name": "vendor/upstream-image-25", "user_defined": true, "config_display_name": "Custom Image", "force_mapping": true},
	}}}
	models := credentialCodexModels(auth)
	if len(models) != 2 || models[0].Alias != "custom" {
		t.Fatalf("exported codex models = %+v, want two configured models", models)
	}
	if image := models[1]; image.Alias != "gpt-image-2.5" || image.Name != "vendor/upstream-image-25" || image.DisplayName != "Custom Image" || !image.ForceMapping {
		t.Fatalf("exported image mapping changed: %+v", image)
	}
}
