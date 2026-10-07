package userapi

import (
	"encoding/json"
	"net/http"
	"testing"

	appconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

type userCapabilitiesPayload struct {
	Capabilities struct {
		EmailRegistration bool `json:"email_registration"`
		EmailVerification bool `json:"email_verification"`
		PasswordRecovery  bool `json:"password_recovery"`
		ModelCatalog      bool `json:"model_catalog"`
		TopupApproval     bool `json:"topup_approval"`
	} `json:"capabilities"`
	ServerInfo struct {
		HomeVersion   string `json:"home_version"`
		HomeCommit    string `json:"home_commit"`
		HomeBuildDate string `json:"home_build_date"`
	} `json:"server_info"`
}

func TestUserCapabilitiesReflectUsableConfiguration(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*appconfig.Config)
		enabled bool
	}{
		{name: "disabled", mutate: func(cfg *appconfig.Config) { cfg.UserEmail.Enabled = false }, enabled: false},
		{name: "invalid", mutate: func(cfg *appconfig.Config) { cfg.UserEmail.FromAddress = "" }, enabled: false},
		{name: "enabled", mutate: func(*appconfig.Config) {}, enabled: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, router, _ := newUserEmailTestHandler(t, test.mutate)
			body := getUserCapabilities(t, router)
			if body.Capabilities.EmailRegistration != test.enabled || body.Capabilities.EmailVerification != test.enabled || body.Capabilities.PasswordRecovery != test.enabled {
				t.Fatalf("capabilities = %#v, want enabled=%v", body.Capabilities, test.enabled)
			}
			// The catalog is served from the model registry, so it stays
			// advertised on a Home that cannot send mail at all.
			if !body.Capabilities.TopupApproval {
				t.Error("topup_approval = false, want true")
			}
			if !body.Capabilities.ModelCatalog {
				t.Errorf("model_catalog = false, want true")
			}
		})
	}
}

func TestUserCapabilitiesReportBuildMetadata(t *testing.T) {
	_, router, _ := newUserEmailTestHandler(t, nil)
	info := getUserCapabilities(t, router).ServerInfo
	if info.HomeVersion == "" || info.HomeCommit == "" || info.HomeBuildDate == "" {
		t.Fatalf("server_info = %#v, want every field populated", info)
	}
}

func TestUserCapabilitiesCPAPublicURL(t *testing.T) {
	for _, publicURL := range []string{"", "https://cpa.example.com/v1"} {
		t.Run(publicURL, func(t *testing.T) {
			_, router, _ := newUserEmailTestHandler(t, func(cfg *appconfig.Config) {
				cfg.UserPanel.CPAPublicURL = publicURL
			})
			response := performUserJSONRequest(t, router, http.MethodGet, "/user/capabilities", nil, "")
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d", response.Code)
			}
			var body struct {
				ServerInfo map[string]any `json:"server_info"`
			}
			if errDecode := json.Unmarshal(response.Body.Bytes(), &body); errDecode != nil {
				t.Fatal(errDecode)
			}
			value, exists := body.ServerInfo["cpa_public_url"]
			if publicURL == "" {
				if exists {
					t.Fatalf("empty cpa_public_url must be omitted: %v", value)
				}
			} else if value != publicURL {
				t.Fatalf("cpa_public_url = %v, want %s", value, publicURL)
			}
		})
	}
}

func getUserCapabilities(t *testing.T, router http.Handler) userCapabilitiesPayload {
	t.Helper()
	response := performUserJSONRequest(t, router, http.MethodGet, "/user/capabilities", nil, "")
	if response.Code != http.StatusOK {
		t.Fatalf("capabilities status = %d body=%s", response.Code, response.Body.String())
	}
	var body userCapabilitiesPayload
	if errDecode := json.Unmarshal(response.Body.Bytes(), &body); errDecode != nil {
		t.Fatalf("decode capabilities: %v", errDecode)
	}
	return body
}
