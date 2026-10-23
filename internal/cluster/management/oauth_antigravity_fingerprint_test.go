package management

import (
	"io"
	"net/http"
	"strings"
	"testing"

	cpasdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
)

type antigravityLoginTransport func(*http.Request) (*http.Response, error)

func (f antigravityLoginTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestAntigravityLoadCodeAssistUsesSDKFingerprint(t *testing.T) {
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	calls := 0
	http.DefaultTransport = antigravityLoginTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.String() != "https://daily-cloudcode-pa.googleapis.com/v1internal:loadCodeAssist" {
			t.Errorf("unexpected project discovery URL: %s", req.URL)
		}
		if got, want := req.Header.Get("User-Agent"), cpasdkauth.AntigravityRequestUserAgent(""); got != want {
			t.Errorf("User-Agent = %q, want SDK fingerprint %q", got, want)
		}
		for _, name := range []string{"Accept", "X-Goog-Api-Client"} {
			if got := req.Header.Get(name); got != "" {
				t.Errorf("%s = %q, want omitted", name, got)
			}
		}
		if req.Header.Get("Authorization") != "Bearer diagnostic-token" || req.Header.Get("Content-Type") != "application/json" {
			t.Error("missing authentication or JSON content type")
		}
		body, errRead := io.ReadAll(req.Body)
		if errRead != nil {
			t.Fatal(errRead)
		}
		if string(body) != `{"metadata":{"ideType":"ANTIGRAVITY"}}` {
			t.Errorf("project discovery body = %s", body)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"cloudaicompanionProject":"diagnostic-project"}`)),
			Request:    req,
		}, nil
	})
	project, errFetch := (&Handler{}).fetchAntigravityProjectID(t.Context(), "diagnostic-token")
	if errFetch != nil {
		t.Fatal(errFetch)
	}
	if project != "diagnostic-project" || calls != 1 {
		t.Fatalf("project=%q requests=%d", project, calls)
	}
}
