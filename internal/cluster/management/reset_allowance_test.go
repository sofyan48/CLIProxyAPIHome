package management

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

func TestUserResetAllowanceManagementHTTPContract(t *testing.T) {
	handler, engine, closeRepo := newUserManagementHTTPTestServer(t)
	defer closeRepo()
	engine.PUT("/users/:id/reset-allowance", handler.SetUserResetAllowance)
	engine.GET("/capabilities", handler.GetCapabilities)
	name := "reset-policy"
	user, errCreate := handler.repo.CreateUser(context.Background(), cluster.UserUpdate{Username: &name})
	if errCreate != nil {
		t.Fatal(errCreate)
	}
	path := fmt.Sprintf("/users/%d/reset-allowance", user.ID)
	for _, payload := range []string{`{}`, `{"mode":"one_time","amount":null}`, `{"mode":"one_time","amount":-1}`, `{"mode":"one_time","amount":1.5}`, `{"mode":"one_time","amount":"1"}`, `{"mode":"bad","amount":1}`, `{"mode":"monthly","amount":1,"expires_at":"2027-01-01T00:00:00Z"}`, `{"mode":"one_time","amount":1,"expires_at":"bad"}`} {
		response := performUserManagementRequest(t, engine, http.MethodPut, path, payload)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("payload=%s status=%d body=%s", payload, response.Code, response.Body.String())
		}
	}
	for _, payload := range []string{`{"mode":"one_time","amount":2,"expires_at":"2027-01-01T00:00:00Z"}`, `{"mode":"monthly","amount":3,"expires_at":null}`, `{"mode":"one_time","amount":0,"expires_at":null}`} {
		response := performUserManagementRequest(t, engine, http.MethodPut, path, payload)
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		var result struct {
			User struct {
				ResetAllowance cluster.UserResetAllowanceStatus `json:"reset_allowance"`
			} `json:"user"`
		}
		decodeUserManagementResponse(t, response, &result)
		if result.User.ResetAllowance.Mode == "" {
			t.Fatalf("missing policy: %s", response.Body.String())
		}
	}
	missing := performUserManagementRequest(t, engine, http.MethodPut, "/users/999999/reset-allowance", `{"mode":"one_time","amount":1}`)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing status=%d", missing.Code)
	}
	capabilities := performUserManagementRequest(t, engine, http.MethodGet, "/capabilities", "")
	var manifest struct {
		Capabilities map[string]bool `json:"capabilities"`
	}
	decodeUserManagementResponse(t, capabilities, &manifest)
	if !manifest.Capabilities["user_reset_allowance"] {
		t.Fatal("missing capability")
	}
}
