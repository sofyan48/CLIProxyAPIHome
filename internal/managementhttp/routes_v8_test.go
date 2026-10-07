package managementhttp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	cpasdkapi "github.com/router-for-me/CLIProxyAPI/v8/sdk/api"
	cpaconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	clustermanagement "github.com/router-for-me/CLIProxyAPIHome/internal/cluster/management"
	"github.com/router-for-me/CLIProxyAPIHome/internal/userapi"
	"golang.org/x/crypto/bcrypt"
)

func TestManagementV8ApprovalRequiresAdministrator(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	ctx := context.Background()
	db, errOpen := cluster.OpenSQLite(ctx, filepath.Join(t.TempDir(), "home.db"))
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatal(errDB)
	}
	t.Cleanup(func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Error(errClose)
		}
	})
	if errMigrate := cluster.AutoMigrate(db); errMigrate != nil {
		t.Fatal(errMigrate)
	}
	repo := cluster.NewRepository(db)
	username, password := "ordinary-user", "password"
	if _, errCreate := repo.CreateUser(ctx, cluster.UserUpdate{Username: &username, Password: &password}); errCreate != nil {
		t.Fatal(errCreate)
	}
	pendingName, pending := "pending-user", true
	user, errCreate := repo.CreateUser(ctx, cluster.UserUpdate{Username: &pendingName, ApprovalPending: &pending})
	if errCreate != nil {
		t.Fatal(errCreate)
	}
	cfg := &cpaconfig.Config{}
	hashedSecret, errHash := bcrypt.GenerateFromPassword([]byte("admin-test-secret"), bcrypt.MinCost)
	if errHash != nil {
		t.Fatal(errHash)
	}
	cfg.RemoteManagement.SecretKey = string(hashedSecret)
	sdk := cpasdkapi.NewHandlerWithoutConfigFilePath(cfg, nil)
	home := clustermanagement.NewHandler(repo, nil, "", 0)
	legacy := defaultRoutes(sdk)
	registerClusterManagementRoutes(legacy, home)
	engine := gin.New()
	userapi.Register(engine.Group("/user"), userapi.NewHandler(repo, nil))
	managementV8Routes(legacy, sdk, home).Register(engine.Group("/v8/management", sdk.Middleware()))
	login := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/user/login", strings.NewReader(`{"username":"ordinary-user","password":"password"}`))
	request.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(login, request)
	var session struct {
		Token string `json:"token"`
	}
	if errDecode := json.Unmarshal(login.Body.Bytes(), &session); errDecode != nil {
		t.Fatal(errDecode)
	}
	if login.Code != http.StatusOK || session.Token == "" {
		t.Fatalf("login = %d %s", login.Code, login.Body.String())
	}
	for _, token := range []string{"", session.Token, "admin-test-secret"} {
		req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v8/management/users/%d/approve", user.ID), nil)
		req.RemoteAddr = "127.0.0.1:1234"
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, req)
		want := http.StatusUnauthorized
		if token == "admin-test-secret" {
			want = http.StatusOK
		}
		if response.Code != want {
			t.Fatalf("approval status = %d, want %d: %s", response.Code, want, response.Body.String())
		}
		stored, errLoad := repo.GetUser(ctx, user.ID)
		if errLoad != nil {
			t.Fatal(errLoad)
		}
		if stored.ApprovalPending != (token != "admin-test-secret") {
			t.Fatal("unauthorized approval or failed authorized approval")
		}
	}
	topup, balance, errTopup := repo.CreateBillingRechargeRequest(ctx, user.ID, 5, "authorization")
	if errTopup != nil {
		t.Fatal(errTopup)
	}
	for _, token := range []string{"", session.Token, "admin-test-secret", "admin-test-secret"} {
		req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v8/management/users/%d/topup/approve", user.ID), strings.NewReader(fmt.Sprintf(`{"request_id":%d}`, topup.ID)))
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, req)
		want, wantBalance := http.StatusUnauthorized, balance
		if token == "admin-test-secret" {
			want = http.StatusOK
			wantBalance += 5
		}
		if response.Code != want {
			t.Fatalf("topup authorization=%d %s", response.Code, response.Body.String())
		}
		stored, errStored := repo.GetUser(ctx, user.ID)
		if errStored != nil || stored.Credits != wantBalance {
			t.Fatalf("user=%+v error=%v", stored, errStored)
		}
	}
	ledger, errLedger := repo.ListBillingBalanceRecords(ctx, cluster.BillingBalanceQuery{UserID: &user.ID})
	if errLedger != nil || ledger.Total != 1 {
		t.Fatalf("ledger=%+v error=%v", ledger, errLedger)
	}
}

func TestManagementV8Routes(t *testing.T) {
	sdk := cpasdkapi.NewHandlerWithoutConfigFilePath(&cpaconfig.Config{}, nil)
	home := clustermanagement.NewHandler(nil, nil, "", 0)
	legacy := defaultRoutes(sdk)
	registerClusterManagementRoutes(legacy, home)
	engine := gin.New()
	managementV8Routes(legacy, sdk, home).Register(engine.Group("/v8/management"))
	registered := make(map[string]bool)
	for _, route := range engine.Routes() {
		registered[route.Method+" "+route.Path] = true
	}
	for _, route := range []string{
		"GET /config", "PUT /config", "PATCH /config", "GET /config.yaml", "PUT /config.yaml", "DELETE /config/*path",
		"GET /credentials", "POST /credentials/refresh", "GET /credentials/in-flight", "PATCH /credentials/:credential_id/concurrency-policy",
		"GET /oauth/auth-url", "POST /oauth/import", "GET /oauth/status", "DELETE /oauth/session",
		"GET /server/latest-version", "POST /requests/api-call", "POST /routing/cooldown/reset",
		"GET /observability/logs", "GET /observability/usage/api-keys", "GET /plugins/store", "DELETE /plugins/:id",
		"GET /credentials/quota/providers", "POST /credentials/quota/fetch", "POST /credentials/quota/reset", "GET /plugins/:id/quota",
		"GET /users", "POST /users/:id/approve", "POST /users/:id/topup/approve", "GET /nodes", "GET /billing/overview", "GET /access/api-keys",
	} {
		method, path, _ := strings.Cut(route, " ")
		if !registered[method+" /v8/management"+path] {
			t.Errorf("missing %s", route)
		}
	}
	for _, path := range []string{"/debug", "/api-keys", "/codex-api-key", "/auth-files", "/codex-auth-url"} {
		if registered["GET /v8/management"+path] {
			t.Errorf("legacy setting registered in v8: %s", path)
		}
	}
}

func TestManagementV8AccessControlAndOAuthCallbacks(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	for _, enabled := range []bool{true, false} {
		t.Run(map[bool]string{true: "enabled", false: "disabled"}[enabled], func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			secret := ""
			if enabled {
				secret = "v8-fixture-password"
			}
			content := "port: 8317\nauth-dir: " + filepath.ToSlash(dir) + "\nremote-management: {secret-key: '" + secret + "'}\n"
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			built, err := Build(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, authorized := range []bool{true, false} {
				for _, route := range []string{"/v0/management/config", "/v8/management/config", "/v8/management/config/server/port"} {
					req := httptest.NewRequest("GET", route, nil)
					req.RemoteAddr = "127.0.0.1:1234"
					if authorized {
						req.Header.Set("Authorization", "Bearer v8-fixture-password")
					}
					rec := httptest.NewRecorder()
					built.Engine.ServeHTTP(rec, req)
					want := http.StatusNotFound
					if enabled {
						want = http.StatusUnauthorized
						if authorized {
							want = http.StatusOK
						}
					}
					if rec.Code != want {
						t.Fatalf("%s authorized=%v: %d want %d: %s", route, authorized, rec.Code, want, rec.Body.String())
					}
				}
			}
			rec := httptest.NewRecorder()
			built.Engine.ServeHTTP(rec, httptest.NewRequest("GET", "/v8/management/oauth/callback", nil))
			want := http.StatusNotFound
			if enabled {
				want = http.StatusBadRequest
			}
			if rec.Code != want {
				t.Fatalf("callback should validate state without bearer: %d, want %d", rec.Code, want)
			}
		})
	}
}
