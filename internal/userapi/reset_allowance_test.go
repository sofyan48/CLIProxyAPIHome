package userapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

func TestUserResetAllowanceHTTPContract(t *testing.T) {
	handler, router, _ := newUserEmailTestHandler(t, nil)
	ctx := context.Background()
	name := "reset-user"
	user, errCreate := handler.repo.CreateUser(ctx, cluster.UserUpdate{Username: &name, Limit5hCredits: cluster.OptionalFloatUpdate{Set: true, Value: 0}})
	if errCreate != nil {
		t.Fatal(errCreate)
	}
	otherName := "other-user"
	other, errOther := handler.repo.CreateUser(ctx, cluster.UserUpdate{Username: &otherName, Limit5hCredits: cluster.OptionalFloatUpdate{Set: true, Value: 10}})
	if errOther != nil {
		t.Fatal(errOther)
	}
	for _, id := range []uint{user.ID, other.ID} {
		if _, errSet := handler.repo.SetUserResetAllowance(ctx, id, cluster.ResetAllowanceOneTime, 1, nil, time.Now().UTC()); errSet != nil {
			t.Fatal(errSet)
		}
	}
	bearer := createUserTestBearerToken(t, handler, user.ID, user.SessionVersion)
	for _, test := range []struct {
		path  string
		body  any
		token string
		want  int
	}{
		{"/user/period-limits/reset", nil, "", http.StatusUnauthorized},
		{"/user/period-limits/reset", map[string]any{}, bearer, http.StatusBadRequest},
		{"/user/period-limits/reset?windows=5h", nil, bearer, http.StatusBadRequest},
	} {
		response := performUserJSONRequest(t, router, http.MethodPost, test.path, test.body, test.token)
		if response.Code != test.want {
			t.Fatalf("status=%d body=%s want=%d", response.Code, response.Body.String(), test.want)
		}
	}
	response := performUserJSONRequest(t, router, http.MethodGet, "/user/period-limits", nil, bearer)
	var status cluster.UserPeriodLimitsStatus
	if errDecode := json.Unmarshal(response.Body.Bytes(), &status); errDecode != nil {
		t.Fatal(errDecode)
	}
	if response.Code != http.StatusOK || status.ResetAllowance.Available != 1 {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	response = performUserJSONRequest(t, router, http.MethodPost, "/user/period-limits/reset", nil, bearer)
	var result struct {
		Status string                         `json:"status"`
		Limits cluster.UserPeriodLimitsStatus `json:"limits"`
	}
	if errDecode := json.Unmarshal(response.Body.Bytes(), &result); errDecode != nil {
		t.Fatal(errDecode)
	}
	if response.Code != http.StatusOK || result.Status != "ok" || result.Limits.UserID != user.ID || result.Limits.ResetAllowance.Available != 0 {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	saved, errGet := handler.repo.GetUser(ctx, other.ID)
	if errGet != nil || saved.ResetAllowanceAvailable != 1 || saved.UsageEpoch5h != nil {
		t.Fatalf("other user changed: %+v %v", saved, errGet)
	}
	response = performUserJSONRequest(t, router, http.MethodPost, "/user/period-limits/reset", nil, bearer)
	if response.Code != http.StatusConflict {
		t.Fatalf("exhausted status=%d body=%s", response.Code, response.Body.String())
	}
	capabilities := performUserJSONRequest(t, router, http.MethodGet, "/user/capabilities", nil, "")
	var manifest struct {
		Capabilities map[string]bool `json:"capabilities"`
	}
	if errDecode := json.Unmarshal(capabilities.Body.Bytes(), &manifest); errDecode != nil {
		t.Fatal(errDecode)
	}
	if !manifest.Capabilities["user_reset_allowance"] {
		t.Fatal("missing capability")
	}
}
