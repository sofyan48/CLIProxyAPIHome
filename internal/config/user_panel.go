package config

import (
	"fmt"
	"net/url"
	"strings"
)

// UserPanelConfig contains display-only metadata for the embedded user panel.
type UserPanelConfig struct {
	CPAPublicURL string `yaml:"cpa-public-url" json:"cpa-public-url"`
}

// NormalizeAndValidateUserPanelConfig accepts an optional public HTTP(S) URL.
func (cfg *Config) NormalizeAndValidateUserPanelConfig() error {
	if cfg == nil {
		return nil
	}
	cfg.UserPanel.CPAPublicURL = strings.TrimSpace(cfg.UserPanel.CPAPublicURL)
	if cfg.UserPanel.CPAPublicURL == "" {
		return nil
	}
	parsed, errParse := url.Parse(cfg.UserPanel.CPAPublicURL)
	if errParse != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil || parsed.Opaque != "" {
		return fmt.Errorf("user-panel.cpa-public-url must be an absolute HTTP or HTTPS URL without credentials")
	}
	return nil
}
