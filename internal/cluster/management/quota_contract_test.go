package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	appconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
)

func TestCredentialListPluginQuotaMetadata(t *testing.T) {
	for _, test := range []struct {
		name      string
		provider  string
		pluginIDs map[string]string
		legacy    bool
		wantID    string
	}{
		{name: "v0 unchanged", provider: "foo", pluginIDs: map[string]string{"foo": "multi-quota"}, legacy: true},
		{name: "no plugins", provider: "foo"},
		{name: "normalized provider", provider: " FOO ", pluginIDs: map[string]string{"foo": "multi-quota"}, wantID: "multi-quota"},
		{name: "built-in without plugin", provider: "codex", pluginIDs: map[string]string{"foo": "multi-quota"}},
		{name: "plugin supports built-in", provider: "codex", pluginIDs: map[string]string{"codex": "codex-quota"}, wantID: "codex-quota"},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, closeRepo := newUsageObservabilityTestHandler(t)
			defer closeRepo()
			auth := &coreauth.Auth{
				ID: "quota-auth", Index: "quota-auth", Provider: test.provider, Status: coreauth.StatusActive,
				Metadata: map[string]any{"type": test.provider, "quota_probe": map[string]any{"url": "https://api.example.com/quota"}},
			}
			if _, errUpsert := handler.repo.UpsertAuth(t.Context(), auth, "test"); errUpsert != nil {
				t.Fatal(errUpsert)
			}
			rec := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(rec)
			ctx.Request = httptest.NewRequest(http.MethodGet, "/credentials", nil)
			if test.legacy {
				handler.ListAuthFiles(ctx)
			} else {
				handler.listAuthFiles(ctx, test.pluginIDs)
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			var response struct {
				Files []map[string]any `json:"files"`
			}
			if errDecode := json.Unmarshal(rec.Body.Bytes(), &response); errDecode != nil {
				t.Fatal(errDecode)
			}
			if len(response.Files) != 1 {
				t.Fatalf("got %d files, want 1", len(response.Files))
			}
			entry := response.Files[0]
			if _, exists := entry["quota_probe"]; exists {
				t.Fatal("credential list must not expose quota_probe")
			}
			if test.wantID == "" {
				for _, field := range []string{"supports_quota", "quota_provider", "quota_plugin_id"} {
					if _, exists := entry[field]; exists {
						t.Fatalf("entry unexpectedly includes %s", field)
					}
				}
				return
			}
			if entry["supports_quota"] != true || entry["quota_provider"] != entry["provider"] || entry["quota_plugin_id"] != test.wantID {
				t.Fatalf("unexpected quota metadata: %#v", entry)
			}
		})
	}
}

func TestAuthFilesListDoesNotAdvertiseCollectorsOrProbes(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()
	runtime, errRuntime := home.NewRuntime(&appconfig.Config{AuthDir: t.TempDir()})
	if errRuntime != nil {
		t.Fatal(errRuntime)
	}
	t.Cleanup(runtime.Stop)
	handler.runtime = runtime
	trigger := &fakeQuotaRecollectTrigger{}
	handler.SetQuotaRecollectTrigger(trigger)
	for _, provider := range []string{"codex", "custom"} {
		auth := &coreauth.Auth{
			ID: provider + "-quota-auth", Index: provider + "-quota-auth", Provider: provider, Status: coreauth.StatusActive,
			Metadata: map[string]any{"type": provider, "quota_probe": map[string]any{"url": "https://api.example.com/quota"}},
		}
		if _, errUpsert := handler.repo.UpsertAuth(t.Context(), auth, "test"); errUpsert != nil {
			t.Fatal(errUpsert)
		}
	}
	for _, v8 := range []bool{false, true} {
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/credentials", nil)
		if v8 {
			handler.ListCredentialsV8(ctx)
		} else {
			handler.ListAuthFiles(ctx)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("v8=%v: status=%d body=%s", v8, rec.Code, rec.Body.String())
		}
		var response struct {
			Files []map[string]any `json:"files"`
		}
		if errDecode := json.Unmarshal(rec.Body.Bytes(), &response); errDecode != nil {
			t.Fatal(errDecode)
		}
		if len(response.Files) != 2 {
			t.Fatalf("v8=%v: got %d files, want 2", v8, len(response.Files))
		}
		for _, entry := range response.Files {
			for _, field := range []string{"supports_quota", "quota_provider", "quota_plugin_id", "quota_probe"} {
				if _, exists := entry[field]; exists {
					t.Fatalf("v8=%v: collector/probe entry unexpectedly includes %s", v8, field)
				}
			}
		}
	}
	if trigger.calls != 0 {
		t.Fatal("listing credentials must not trigger quota collection")
	}
}
