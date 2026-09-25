package management

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

// discoverOpenAICompatBody fills omitted model lists without changing explicit models.
func discoverOpenAICompatBody(ctx context.Context, body []byte) ([]byte, error) {
	var entries []map[string]any
	if errDecode := decodeListBody(body, "openai-compatibility", &entries); errDecode != nil {
		return nil, errDecode
	}
	for _, raw := range entries {
		if models, specified := raw["models"]; specified && models != nil {
			if list, isList := models.([]any); !isList || len(list) > 0 {
				continue
			}
		}
		encoded, errMarshal := json.Marshal(raw)
		if errMarshal != nil {
			return nil, errMarshal
		}
		var entry config.OpenAICompatibility
		if errUnmarshal := json.Unmarshal(encoded, &entry); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		if entry.Disabled || len(entry.APIKeyEntries) == 0 {
			continue
		}
		key := strings.TrimSpace(entry.APIKeyEntries[0].APIKey)
		if key == "" {
			continue
		}
		models, errFetch := fetchOpenAICompatModels(ctx, entry.BaseURL, key, entry.APIKeyEntries[0].ProxyURL, entry.Headers)
		if errFetch == nil {
			raw["models"] = models
		}
	}
	return json.Marshal(entries)
}

func fetchOpenAICompatModels(ctx context.Context, baseURL, key, proxyURL string, headers map[string]string) ([]config.OpenAICompatibilityModel, error) {
	base, errParse := url.Parse(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	if errParse != nil || base.Host == "" || (base.Scheme != "https" && base.Scheme != "http") || base.User != nil {
		return nil, fmt.Errorf("invalid OpenAI-compatible base URL")
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/models"
	base.RawQuery = ""
	base.Fragment = ""
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, base.String(), nil)
	if errRequest != nil {
		return nil, errRequest
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	for name, value := range headers {
		if !strings.EqualFold(name, "Authorization") && !strings.EqualFold(name, "Host") {
			req.Header.Set(name, value)
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if proxyURL = strings.TrimSpace(proxyURL); proxyURL != "" {
		if strings.EqualFold(proxyURL, "direct") {
			transport.Proxy = nil
		} else {
			proxy, errProxy := url.Parse(proxyURL)
			if errProxy != nil || proxy.Host == "" {
				return nil, fmt.Errorf("invalid model discovery proxy URL")
			}
			transport.Proxy = http.ProxyURL(proxy)
		}
	}
	client := &http.Client{Timeout: 3 * time.Second, Transport: transport, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	defer transport.CloseIdleConnections()
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, errDo
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models endpoint returned HTTP %d", resp.StatusCode)
	}
	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if errDecode := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&result); errDecode != nil {
		return nil, errDecode
	}
	seen := make(map[string]bool, len(result.Data))
	models := make([]config.OpenAICompatibilityModel, 0, len(result.Data))
	for _, model := range result.Data {
		id := strings.TrimSpace(model.ID)
		if id != "" && !seen[id] {
			seen[id] = true
			models = append(models, config.OpenAICompatibilityModel{Name: id})
		}
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("models endpoint returned no model IDs")
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Name < models[j].Name })
	return models, nil
}
