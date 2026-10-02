package config

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestUserPanelConfigValidation(t *testing.T) {
	for _, test := range []struct {
		value string
		valid bool
	}{
		{"", true}, {"   ", true}, {" https://cpa.example.com/v1 ", true},
		{"http://localhost:8317", true}, {"https://[::1]:8317/v1", true},
		{"/v1", false}, {"//cpa.example.com/v1", false}, {"ftp://cpa.example.com", false},
		{"https://", false}, {"https://user:password@cpa.example.com", false},
		{"https://user@cpa.example.com", false}, {"https://cpa.example.com:bad", false},
	} {
		t.Run(test.value, func(t *testing.T) {
			cfg := &Config{UserPanel: UserPanelConfig{CPAPublicURL: test.value}}
			errValidate := cfg.NormalizeAndValidateUserPanelConfig()
			if (errValidate == nil) != test.valid {
				t.Fatalf("validation error = %v, valid = %v", errValidate, test.valid)
			}
			if cfg.UserPanel.CPAPublicURL != strings.TrimSpace(test.value) {
				t.Fatal("URL was not trimmed")
			}
		})
	}
}

func TestUserPanelConfigLayout(t *testing.T) {
	data := []byte("config-version: 8\nuser-panel:\n  cpa-public-url: https://cpa.example.com/v1\n")
	if errValidate := ValidateV8Config(data); errValidate != nil {
		t.Fatal(errValidate)
	}
	var cfg Config
	if errDecode := yaml.Unmarshal(data, &cfg); errDecode != nil {
		t.Fatal(errDecode)
	}
	encoded, errEncode := yaml.Marshal(&cfg)
	if errEncode != nil || !strings.Contains(string(encoded), "cpa-public-url: https://cpa.example.com/v1") {
		t.Fatalf("layout lost metadata: %s, %v", encoded, errEncode)
	}
	jsonData, errJSON := json.Marshal(&cfg)
	if errJSON != nil || strings.Contains(string(jsonData), "cpa.example.com") {
		t.Fatalf("Home metadata leaked into runtime JSON: %s, %v", jsonData, errJSON)
	}
	for _, invalid := range []string{
		"config-version: 8\nuser-panel:\n  cpa-public-url: /v1\n",
		"config-version: 8\nuser-panel:\n  typo: https://cpa.example.com\n",
	} {
		if errValidate := ValidateV8Config([]byte(invalid)); errValidate == nil {
			t.Fatalf("accepted invalid config: %s", invalid)
		}
	}
}
