package management

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

func TestUserManagementApprovalRuntimeRefresh(t *testing.T) {
	handler, engine, closeRepo := newAPIKeyManagementRuntimeTestServer(t)
	defer closeRepo()
	engine.POST("/users/:id/approve", handler.ApproveUser)
	name, pending := "runtime-pending", true
	user, errUser := handler.repo.CreateUser(t.Context(), cluster.UserUpdate{Username: &name, ApprovalPending: &pending})
	if errUser != nil {
		t.Fatal(errUser)
	}
	group, errGroup := handler.repo.CreateModelGroup(t.Context(), "runtime-scope", false)
	if errGroup != nil {
		t.Fatal(errGroup)
	}
	before := handler.runtime.Config()
	response := performUserManagementRequest(t, engine, http.MethodPost, fmt.Sprintf("/users/%d/approve", user.ID), fmt.Sprintf(`{"model_groups":[%d]}`, group.ID))
	if response.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", response.Code, response.Body.String())
	}
	if handler.runtime.Config() == before {
		t.Fatal("approval did not refresh runtime config")
	}
	keys, errKeys := handler.repo.ListAPIKeyRecordsForUser(t.Context(), user.ID)
	if errKeys != nil || len(keys) != 1 {
		t.Fatal("missing generated key")
	}
	// Home authenticates client keys through the DB, not public runtime config.
	valid, errValidate := handler.repo.ValidateAPIKey(t.Context(), keys[0].APIKey)
	if errValidate != nil || !valid {
		t.Fatalf("generated key validation = %t, error = %v", valid, errValidate)
	}
	if len(handler.runtime.Config().APIKeys) != 0 {
		t.Fatal("runtime config exposed database API keys")
	}
}

func TestUserManagementApprovalScopes(t *testing.T) {
	handler, engine, closeRepo := newUserManagementHTTPTestServer(t)
	defer closeRepo()
	ctx := t.Context()
	group, errGroup := handler.repo.CreateModelGroup(ctx, "enabled", false)
	if errGroup != nil {
		t.Fatal(errGroup)
	}
	disabled, errDisabled := handler.repo.CreateModelGroup(ctx, "disabled", true)
	if errDisabled != nil {
		t.Fatal(errDisabled)
	}
	name, pending := "scoped-pending", true
	user, errUser := handler.repo.CreateUser(ctx, cluster.UserUpdate{Username: &name, ApprovalPending: &pending})
	if errUser != nil {
		t.Fatal(errUser)
	}
	path := fmt.Sprintf("/users/%d/approve", user.ID)
	for _, tc := range []struct {
		body   string
		status int
		code   string
	}{
		{`{`, 400, "invalid body"}, {`[]`, 400, "invalid body"},
		{`{"model_groups":null}`, 400, "invalid_model_groups"},
		{`{"model_groups":[]}`, 400, "invalid_model_groups"},
		{`{"model_groups":[0]}`, 400, "invalid_model_groups"},
		{`{"model_groups":[-1]}`, 400, "invalid_model_groups"},
		{`{"model_groups":[1.5]}`, 400, "invalid_model_groups"},
		{`{"model_groups":["1"]}`, 400, "invalid_model_groups"},
		{fmt.Sprintf(`{"model_groups":[%d]}`, disabled.ID), 400, "invalid_model_groups"},
		{fmt.Sprintf(`{"model_groups":[%d,999999]}`, group.ID), 404, "not_found"},
	} {
		response := performUserManagementRequest(t, engine, http.MethodPost, path, tc.body)
		if response.Code != tc.status || !strings.Contains(response.Body.String(), `"error":"`+tc.code+`"`) {
			t.Fatalf("body %s: %d %s", tc.body, response.Code, response.Body.String())
		}
		stored, errLoad := handler.repo.GetUser(ctx, user.ID)
		if errLoad != nil || !stored.ApprovalPending {
			t.Fatal("invalid request approved user")
		}
		keys, errKeys := handler.repo.ListAPIKeyRecordsForUser(ctx, user.ID)
		if errKeys != nil || len(keys) != 0 {
			t.Fatal("invalid request created keys")
		}
	}
	response := performUserManagementRequest(t, engine, http.MethodPost, path, fmt.Sprintf(`{"model_groups":[%d]}`, group.ID))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"approval_pending":false`) {
		t.Fatalf("approval: %d %s", response.Code, response.Body.String())
	}
	keys, errKeys := handler.repo.ListAPIKeyRecordsForUser(ctx, user.ID)
	if errKeys != nil || len(keys) != 1 {
		t.Fatal("approval did not generate key")
	}
	if strings.Contains(response.Body.String(), keys[0].APIKey) || strings.Contains(response.Body.String(), `"api_key"`) {
		t.Fatal("approval response exposed key")
	}
	// Well-formed retries do not validate scopes again or create duplicate keys.
	retry := performUserManagementRequest(t, engine, http.MethodPost, path, `{"model_groups":[999999]}`)
	if retry.Code != http.StatusOK {
		t.Fatalf("retry: %d %s", retry.Code, retry.Body.String())
	}
	newKeys, errRetryKeys := handler.repo.ListAPIKeyRecordsForUser(ctx, user.ID)
	if errRetryKeys != nil || len(newKeys) != 1 || newKeys[0].APIKey != keys[0].APIKey || string(newKeys[0].ModelGroups) != string(keys[0].ModelGroups) {
		t.Fatal("retry changed keys")
	}
}
