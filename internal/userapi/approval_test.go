package userapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

func TestPublicRegistrationApproval(t *testing.T) {
	handler, router, _ := newUserEmailTestHandler(t, nil)
	ctx := context.Background()
	credentials := map[string]any{"username": "pending", "password": "password", "email": "pending@example.com", "approval_pending": false}
	response := performUserJSONRequest(t, router, http.MethodPost, "/user/register", credentials, "")
	var body map[string]any
	if errDecode := json.Unmarshal(response.Body.Bytes(), &body); errDecode != nil {
		t.Fatal(errDecode)
	}
	if response.Code != http.StatusAccepted || len(body) != 2 || body["approval_pending"] != true || body["message"] != approvalPendingMessage || len(response.Result().Cookies()) != 0 {
		t.Fatalf("registration = %d %s", response.Code, response.Body.String())
	}
	user, errUser := handler.repo.GetUserByUsername(ctx, "pending")
	if errUser != nil {
		t.Fatal(errUser)
	}
	if !user.ApprovalPending {
		t.Fatal("registration was not persisted as pending")
	}
	bearer := createUserTestBearerToken(t, handler, user.ID, user.SessionVersion)
	for _, tc := range []struct {
		method, path string
		body         any
		token        string
	}{
		{http.MethodPost, "/user/login", credentials, ""},
		{http.MethodPost, "/user/login/totp", credentials, ""},
		{http.MethodPost, "/user/login/passkey/begin", credentials, ""},
		{http.MethodPost, "/user/login/passkey/options", credentials, ""},
		{http.MethodPost, "/user/login/passkey", map[string]any{"username": "pending", "challenge_id": "challenge", "credential": map[string]any{"id": "credential"}}, ""},
		{http.MethodGet, "/user/me", nil, bearer},
		{http.MethodGet, "/user/api-keys", nil, bearer},
		{http.MethodPost, "/user/password", map[string]any{"new_password": "changed"}, bearer},
		{http.MethodPut, "/user/email", map[string]any{"email": "other@example.com"}, bearer},
		{http.MethodPost, "/user/passkeys/begin", map[string]any{}, bearer},
	} {
		t.Run(tc.path, func(t *testing.T) {
			denied := performUserJSONRequest(t, router, tc.method, tc.path, tc.body, tc.token)
			var failure map[string]any
			if errDecode := json.Unmarshal(denied.Body.Bytes(), &failure); errDecode != nil {
				t.Fatal(errDecode)
			}
			if denied.Code != http.StatusForbidden || failure["error"] != "approval_pending" || failure["message"] != approvalPendingMessage {
				t.Fatalf("response = %d %s", denied.Code, denied.Body.String())
			}
		})
	}
	secret := "JBSWY3DPEHPK3PXP"
	mfa, errMFA := marshalTOTP(user.Username, secret, "test")
	if errMFA != nil {
		t.Fatal(errMFA)
	}
	if _, errUpdate := handler.repo.UpdateUser(ctx, user.ID, cluster.UserUpdate{MFA: &mfa}); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	code, errCode := hotpCode(secret, time.Now().UTC().Unix()/int64(defaultTOTPPeriod))
	if errCode != nil {
		t.Fatal(errCode)
	}
	totp := performUserJSONRequest(t, router, http.MethodPost, "/user/login/totp", map[string]any{"username": user.Username, "password": "password", "totp_code": code}, "")
	if totp.Code != http.StatusForbidden {
		t.Fatalf("valid TOTP bypassed approval: %d %s", totp.Code, totp.Body.String())
	}
	emptyMFA := cluster.JSONB(nil)
	if _, errUpdate := handler.repo.UpdateUser(ctx, user.ID, cluster.UserUpdate{MFA: &emptyMFA}); errUpdate != nil {
		t.Fatal(errUpdate)
	}

	// Public email verification and recovery may succeed, but neither approves nor logs in.
	for _, tc := range []struct {
		purpose, path string
		body          map[string]any
	}{
		{cluster.UserSecurityTokenPurposeEmailVerification, "/user/email/verify", map[string]any{"token": "verify-token"}},
		{cluster.UserSecurityTokenPurposePasswordReset, "/user/password/reset", map[string]any{"token": "reset-token", "new_password": "new-password"}},
	} {
		now := time.Now().UTC()
		if errStore := handler.repo.ReplaceUserSecurityToken(ctx, cluster.UserSecurityTokenRecord{UserID: user.ID, Purpose: tc.purpose, TokenHash: cluster.HashUserSecurityValue(tc.body["token"].(string)), EmailVersion: user.EmailVersion, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); errStore != nil {
			t.Fatal(errStore)
		}
		result := performUserJSONRequest(t, router, http.MethodPost, tc.path, tc.body, "")
		if result.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", tc.path, result.Code, result.Body.String())
		}
		var resultBody map[string]any
		if errDecode := json.Unmarshal(result.Body.Bytes(), &resultBody); errDecode != nil {
			t.Fatal(errDecode)
		}
		if resultBody["token"] != nil {
			t.Fatal("recovery issued a session")
		}
		stored, errLoad := handler.repo.GetUser(ctx, user.ID)
		if errLoad != nil {
			t.Fatal(errLoad)
		}
		if !stored.ApprovalPending {
			t.Fatal("recovery approved pending user")
		}
	}
	credentials["password"] = "new-password"
	denied := performUserJSONRequest(t, router, http.MethodPost, "/user/login", credentials, "")
	if denied.Code != http.StatusForbidden {
		t.Fatalf("reset bypassed approval: %d %s", denied.Code, denied.Body.String())
	}
	pending := false
	if _, errApprove := handler.repo.UpdateUser(ctx, user.ID, cluster.UserUpdate{ApprovalPending: &pending}); errApprove != nil {
		t.Fatal(errApprove)
	}
	login := performUserJSONRequest(t, router, http.MethodPost, "/user/login", credentials, "")
	var session struct {
		Token string `json:"token"`
	}
	if errDecode := json.Unmarshal(login.Body.Bytes(), &session); errDecode != nil {
		t.Fatal(errDecode)
	}
	if login.Code != http.StatusOK || session.Token == "" {
		t.Fatalf("approved login = %d %s", login.Code, login.Body.String())
	}
	me := performUserJSONRequest(t, router, http.MethodGet, "/user/me", nil, session.Token)
	if me.Code != http.StatusOK {
		t.Fatalf("approved session = %d %s", me.Code, me.Body.String())
	}
}
