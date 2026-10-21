package management

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

func TestAccessGroupDeletionClearsAPIKeyBindings(t *testing.T) {
	for _, kind := range []string{"channel", "model"} {
		t.Run(kind, func(t *testing.T) {
			handler, engine, closeRepo := newAPIKeyManagementRuntimeTestServer(t)
			defer closeRepo()
			ctx := t.Context()
			channel, errChannel := handler.repo.CreateChannelGroup(ctx, "channel", false)
			if errChannel != nil {
				t.Fatal(errChannel)
			}
			model, errModel := handler.repo.CreateModelGroup(ctx, "model", false)
			if errModel != nil {
				t.Fatal(errModel)
			}
			channels, models := []uint{channel.ID}, []uint{model.ID}
			key, errKey := handler.repo.CreateAPIKey(ctx, cluster.APIKeyEntryUpdate{APIKey: "client-key", Channels: &channels, ModelGroups: &models})
			if errKey != nil {
				t.Fatal(errKey)
			}
			engine.DELETE("/channel-groups/:id", handler.DeleteChannelGroup)
			engine.DELETE("/model-groups/:id", handler.DeleteModelGroup)
			groupID, field := channel.ID, "channels"
			if kind == "model" {
				groupID, field = model.ID, "model_groups"
			}
			before := performAPIKeyManagementRequest(t, engine, http.MethodGet, "/api-keys", nil)
			response := performAPIKeyManagementRequest(t, engine, http.MethodDelete, fmt.Sprintf("/%s-groups/%d", kind, groupID), nil)
			if response.Code != http.StatusOK {
				t.Fatalf("delete status=%d body=%s", response.Code, response.Body.String())
			}
			after := performAPIKeyManagementRequest(t, engine, http.MethodGet, "/api-keys", nil)
			var collection struct {
				Entries []apiKeyManagementEntryResponse `json:"api_key_entries"`
			}
			decodeAPIKeyManagementResponse(t, after, &collection)
			if len(collection.Entries) != 1 {
				t.Fatalf("keys = %+v", collection.Entries)
			}
			entry := collection.Entries[0]
			if entry.ID != key.ID || (kind == "channel" && (len(entry.Channels) != 0 || len(entry.ModelGroups) != 1)) || (kind == "model" && (len(entry.ModelGroups) != 0 || len(entry.Channels) != 1)) {
				t.Fatalf("unexpected bindings after deletion: %+v", entry)
			}
			if before.Header().Get("ETag") == after.Header().Get("ETag") {
				t.Fatal("group deletion did not invalidate API key collection ETag")
			}
			for _, method := range []string{http.MethodPost, http.MethodPatch, http.MethodPut} {
				payload := map[string]any{"id": key.ID, "api_key": key.APIKey, field: []uint{groupID}}
				var body any = payload
				if method == http.MethodPost {
					payload["api_key"] = "new-key"
				}
				if method == http.MethodPut {
					body = map[string]any{"api_key_entries": []any{payload}}
				}
				rejected := performAPIKeyManagementRequest(t, engine, method, "/api-keys", body)
				if rejected.Code != http.StatusBadRequest || !strings.Contains(rejected.Body.String(), "invalid_access_group") {
					t.Fatalf("%s stale binding status=%d body=%s", method, rejected.Code, rejected.Body.String())
				}
			}
		})
	}
}
