package home

import (
	"reflect"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

func TestCredentialModelCandidatesSurviveExclusionsAliasesAndPrefixes(t *testing.T) {
	runtime := &Runtime{cfg: &config.Config{
		OAuthModelAlias: map[string][]config.OAuthModelAlias{
			"claude": {{Name: "source-a", Alias: "friendly"}},
		},
	}}
	runtime.cfg.ForceModelPrefix = true
	auth := &coreauth.Auth{
		ID: t.Name(), Provider: "claude", Prefix: "team",
		Attributes: map[string]string{"auth_kind": "oauth", "excluded_models": "source-b"},
		Metadata: map[string]any{homeConfigModelsMetadataKey: []map[string]any{
			{"id": "source-a"}, {"id": "source-b"},
		}},
	}
	modelRegistry := registry.GetGlobalRegistry()
	t.Cleanup(func() { modelRegistry.UnregisterClient(auth.ID) })
	ids := func(models []*ModelInfo) []string {
		result := make([]string, 0, len(models))
		for _, model := range models {
			result = append(result, model.ID)
		}
		return result
	}
	runtime.registerModelsForAuth(auth)
	if got := ids(modelRegistry.GetModelsForClient(auth.ID)); !reflect.DeepEqual(got, []string{"team/friendly"}) {
		t.Fatalf("routed models = %v", got)
	}
	candidates := modelRegistry.GetModelCandidatesForClient(auth.ID)
	if got := ids(candidates); !reflect.DeepEqual(got, []string{"source-a", "source-b"}) {
		t.Fatalf("exclusion candidates = %v", got)
	}

	// Saving the candidate ID must remove the alias and prefixed route.
	auth.Attributes["excluded_models"] = candidates[0].ID + ",source-b"
	runtime.registerModelsForAuth(auth)
	if got := modelRegistry.GetModelsForClient(auth.ID); len(got) != 0 {
		t.Fatalf("excluded models still routed: %+v", got)
	}
	candidates = modelRegistry.GetModelCandidatesForClient(auth.ID)
	if len(candidates) != 2 {
		t.Fatalf("all-excluded credential lost its candidates: %+v", candidates)
	}
	candidates[0].ID = "mutated-copy"
	if got := modelRegistry.GetModelCandidatesForClient(auth.ID)[0].ID; got != "source-a" {
		t.Fatalf("candidate reader mutated registry: %s", got)
	}

	auth.Attributes["excluded_models"] = "*"
	runtime.registerModelsForAuth(auth)
	if got := modelRegistry.GetModelCandidatesForClient(auth.ID); len(got) != 2 {
		t.Fatalf("wildcard exclusions removed candidates: %+v", got)
	}
	modelRegistry.UnregisterClient(auth.ID)
	if got := modelRegistry.GetModelCandidatesForClient(auth.ID); got != nil {
		t.Fatalf("unregistered client retained candidates: %+v", got)
	}
}
