package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

func TestOpenAICompatDiscoveryAndPatchPreservesModels(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("unexpected models request: path=%s auth=%s", r.URL.Path, r.Header.Get("Authorization"))
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"model-b"},{"id":"model-a"},{"id":"model-a"}]}`))
	}))
	defer upstream.Close()

	db, cleanup := openManagementLogTestDB(t)
	defer cleanup()
	handler := NewHandler(cluster.NewRepository(db), nil, "127.0.0.1", 0)
	engine := gin.New()
	engine.PUT("/openai-compatibility", handler.PutOpenAICompat)
	engine.PATCH("/openai-compatibility", handler.PatchOpenAICompat)
	engine.GET("/openai-compatibility", handler.GetOpenAICompat)
	put := httptest.NewRecorder()
	engine.ServeHTTP(put, httptest.NewRequest(http.MethodPut, "/openai-compatibility", strings.NewReader(`[{"name":"compat","base-url":"`+upstream.URL+`/v1","api-key-entries":[{"api-key":"test-key"}]}]`)))
	if put.Code != http.StatusOK {
		t.Fatalf("PUT status = %d: %s", put.Code, put.Body.String())
	}

	readModels := func() ([]map[string]any, string) {
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/openai-compatibility", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("GET status = %d: %s", response.Code, response.Body.String())
		}
		var result map[string][]map[string]any
		if errDecode := json.Unmarshal(response.Body.Bytes(), &result); errDecode != nil {
			t.Fatal(errDecode)
		}
		item := result["openai-compatibility"][0]
		models, _ := item["models"].([]any)
		out := make([]map[string]any, 0, len(models))
		for _, model := range models {
			out = append(out, model.(map[string]any))
		}
		entries := item["api-key-entries"].([]any)
		return out, entries[0].(map[string]any)["id"].(string)
	}
	models, id := readModels()
	if len(models) != 2 || models[0]["name"] != "model-a" || models[1]["name"] != "model-b" {
		t.Fatalf("discovered models = %#v", models)
	}
	patch := httptest.NewRecorder()
	engine.ServeHTTP(patch, httptest.NewRequest(http.MethodPatch, "/openai-compatibility", strings.NewReader(`{"id":"`+id+`","value":{"priority":3}}`)))
	if patch.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d: %s", patch.Code, patch.Body.String())
	}
	models, _ = readModels()
	if len(models) != 2 || models[0]["name"] != "model-a" || models[1]["name"] != "model-b" {
		t.Fatalf("models after PATCH = %#v", models)
	}
}

func TestOpenAICompatStoredModelsKeepAliasPoolAndThinking(t *testing.T) {
	handler := &Handler{}
	auths, errSynthesize := handler.synthesizeAPIKeyBody("openai-compatibility", []byte(`[{"name":"compat","base-url":"https://example.test/v1","api-key-entries":[{"api-key":"key"}],"models":[{"name":"upstream-a","alias":"pool","thinking":{"levels":["low"]}},{"name":"upstream-b","alias":"pool"}]}]`))
	if errSynthesize != nil || len(auths) != 1 {
		t.Fatalf("synthesize: auths=%d error=%v", len(auths), errSynthesize)
	}
	models := cluster.OpenAICompatModelsFromAuth(auths[0])
	if len(models) != 2 || models[0].Name != "upstream-a" || models[1].Name != "upstream-b" || models[0].Alias != "pool" || models[1].Alias != "pool" || models[0].Thinking == nil {
		t.Fatalf("restored models = %#v", models)
	}
}

func TestDiscoverOpenAICompatBodyDoesNotReplaceExplicitModels(t *testing.T) {
	body := []byte(`[{"name":"compat","base-url":"https://example.test/v1","api-key-entries":[{"api-key":"key"}],"models":[{"name":"upstream","alias":"custom"}]}]`)
	result, errDiscover := discoverOpenAICompatBody(context.Background(), body)
	if errDiscover != nil || !strings.Contains(string(result), `"alias":"custom"`) {
		t.Fatalf("discovery result = %s, error = %v", result, errDiscover)
	}
}
