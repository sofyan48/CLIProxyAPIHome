package home

import (
	"os"
	"path/filepath"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

func TestReloadAuthsKeepsCredentialWithInvalidExcludedModels(t *testing.T) {
	authDir := t.TempDir()
	for name, raw := range map[string]string{
		"invalid-type.json":    `{"type":"kimi-ai","email":"type@example.test","excluded_models":123}`,
		"invalid-element.json": `{"type":"kimi-ai","email":"element@example.test","excluded_models":["Model-A",1]}`,
	} {
		if errWrite := os.WriteFile(filepath.Join(authDir, name), []byte(raw), 0o600); errWrite != nil {
			t.Fatalf("write %s: %v", name, errWrite)
		}
	}
	manager := coreauth.NewManager(nil, nil, nil)
	runtime := &Runtime{coreManager: manager, cfg: &config.Config{}, authDir: authDir}
	t.Cleanup(func() {
		for _, auth := range manager.List() {
			registry.GetGlobalRegistry().UnregisterClient(auth.ID)
		}
	})

	// One invalid excluded_models value must not fail loading the existing credentials.
	if errReload := runtime.ReloadAuths(t.Context()); errReload != nil {
		t.Fatalf("ReloadAuths() error = %v", errReload)
	}
	got := make(map[string]string)
	for _, auth := range manager.List() {
		got[filepath.Base(auth.Attributes["path"])] = auth.Attributes["excluded_models"]
	}
	if rules, ok := got["invalid-type.json"]; !ok || rules != "" {
		t.Fatalf("invalid-type.json loaded = %t with rules %q, want loaded without credential rules", ok, rules)
	}
	if rules, ok := got["invalid-element.json"]; !ok || rules != "model-a" {
		t.Fatalf("invalid-element.json loaded = %t with rules %q, want loaded keeping model-a", ok, rules)
	}
}

func TestRegisterModelsCredentialExclusionsOverrideGlobal(t *testing.T) {
	catalog := registry.GetKimiModels()
	if len(catalog) < 2 {
		t.Fatalf("kimi catalog has %d models, want at least 2", len(catalog))
	}
	globalModel := catalog[0].ID
	credentialModel := catalog[1].ID
	runtime := &Runtime{cfg: &config.Config{OAuthExcludedModels: map[string][]string{"kimi-ai": {globalModel}}}}

	registered := func(auth *coreauth.Auth) map[string]bool {
		t.Helper()
		runtime.registerModelsForAuth(auth)
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
		out := make(map[string]bool)
		for _, model := range registry.GetGlobalRegistry().GetModelsForClient(auth.ID) {
			out[model.ID] = true
		}
		return out
	}

	inheritAuth := &coreauth.Auth{
		ID: "kimi-exclusion-inherit", Provider: "kimi-ai", Status: coreauth.StatusActive,
		Attributes: map[string]string{"auth_kind": "oauth"},
	}
	inherited := registered(inheritAuth)
	if inherited[globalModel] || !inherited[credentialModel] {
		t.Fatalf("inherited models = %v, want %q excluded by global config and %q kept", inherited, globalModel, credentialModel)
	}

	// A later global config change must apply to the same credential on the next registration.
	runtime.cfg = &config.Config{OAuthExcludedModels: map[string][]string{"kimi-ai": {credentialModel}}}
	reloaded := registered(inheritAuth)
	if !reloaded[globalModel] || reloaded[credentialModel] {
		t.Fatalf("reloaded models = %v, want %q kept and %q excluded by the updated global config", reloaded, globalModel, credentialModel)
	}
	runtime.cfg = &config.Config{OAuthExcludedModels: map[string][]string{"kimi-ai": {globalModel}}}

	overridden := registered(&coreauth.Auth{
		ID: "kimi-exclusion-override", Provider: "kimi-ai", Status: coreauth.StatusActive,
		Attributes: map[string]string{"auth_kind": "oauth", "excluded_models": credentialModel},
	})
	if !overridden[globalModel] || overridden[credentialModel] {
		t.Fatalf("overridden models = %v, want %q kept and %q excluded by credential config", overridden, globalModel, credentialModel)
	}
}
