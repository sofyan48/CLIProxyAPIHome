package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
)

func TestApplyOAuthFieldPatchWeight(t *testing.T) {
	for _, test := range []struct {
		name  string
		patch string
		want  *int64
	}{
		{name: "positive", patch: `{"weight":5}`, want: weightPointer(5)},
		{name: "maximum", patch: `{"weight":1000000}`, want: weightPointer(1000000)},
		{name: "zero", patch: `{"weight":0}`, want: weightPointer(0)},
		{name: "negative normalizes to zero", patch: `{"weight":-5}`, want: weightPointer(0)},
		{name: "null removes", patch: `{"weight":null}`},
		{name: "omitted preserves", patch: `{"note":"updated"}`, want: weightPointer(7)},
	} {
		t.Run(test.name, func(t *testing.T) {
			auth := &coreauth.Auth{
				Attributes: map[string]string{"weight": "7", "priority": "10"},
				Metadata:   map[string]any{"weight": int64(7), "token": map[string]any{"access_token": "fixture"}},
			}
			changed, errPatch := applyOAuthFieldPatch(auth, mustRawFields(t, test.patch))
			if errPatch != nil || !changed {
				t.Fatalf("patch = (%t, %v), want successful change", changed, errPatch)
			}
			weight, exists := auth.Metadata["weight"]
			if test.want == nil {
				if exists {
					t.Fatalf("weight remains after clear: %#v", weight)
				}
			} else if weight != *test.want {
				t.Fatalf("weight = %#v, want %d", weight, *test.want)
			}
			if test.name == "omitted preserves" {
				if auth.Attributes["weight"] != "7" {
					t.Fatal("omitted weight changed scheduler attribute")
				}
			} else if _, exists := auth.Attributes["weight"]; exists {
				t.Fatal("stale scheduler weight attribute remains")
			}
			if auth.Attributes["priority"] != "10" || !reflect.DeepEqual(auth.Metadata["token"], map[string]any{"access_token": "fixture"}) {
				t.Fatal("unrelated metadata or attributes changed")
			}
		})
	}
}

func weightPointer(value int64) *int64 { return &value }

func TestApplyOAuthFieldPatchWeightRejectsInvalidAtomically(t *testing.T) {
	for _, patch := range []string{
		`{"note":"changed","weight":"2"}`,
		`{"note":"changed","weight":true}`,
		`{"note":"changed","weight":1.5}`,
		`{"note":"changed","weight":1e2}`,
		`{"note":"changed","weight":1000001}`,
		`{"note":"changed","weight":9223372036854775808}`,
		`{"note":"changed","weight":-9223372036854775809}`,
		`{"note":"changed","weight":{}}`,
		`{"note":"changed","weight":[]}`,
		`{"note":"changed","weight.child":2}`,
	} {
		t.Run(patch, func(t *testing.T) {
			auth := &coreauth.Auth{Attributes: map[string]string{"weight": "7"}, Metadata: map[string]any{"weight": int64(7), "note": "original"}}
			before := auth.Clone()
			changed, errPatch := applyOAuthFieldPatch(auth, mustRawFields(t, patch))
			if errPatch == nil || changed || !reflect.DeepEqual(auth, before) {
				t.Fatalf("invalid weight patch = (%t, %v), want unchanged auth", changed, errPatch)
			}
		})
	}
}

func TestPatchCredentialFieldsWeightDatabaseRoundTrip(t *testing.T) {
	handler, engine, _, closeRepo := newConcurrencyManagementTestServer(t)
	defer closeRepo()
	seedOAuthAuth(t, handler.repo, "oauth-weight")
	engine.PATCH("/v8/management/credentials/fields", handler.PatchAuthFileFields)
	auth, _, errAuth := handler.repo.GetAuth(t.Context(), "oauth-weight")
	if errAuth != nil {
		t.Fatal(errAuth)
	}
	auth.Metadata["weight"] = 7
	auth.Metadata["token"] = map[string]any{"access_token": "fixture"}
	auth.Attributes = map[string]string{"weight": "7", "priority": "10"}
	if _, errUpsert := handler.repo.UpsertAuth(t.Context(), auth, "test"); errUpsert != nil {
		t.Fatal(errUpsert)
	}
	for _, test := range []struct {
		patch  string
		status int
		want   string
	}{
		{patch: `"weight":3`, status: http.StatusOK, want: "3"},
		{patch: `"weight":0`, status: http.StatusOK, want: "0"},
		{patch: `"weight":null`, status: http.StatusOK},
		{patch: `"weight":null`, status: http.StatusOK},
		{patch: `"weight":5`, status: http.StatusOK, want: "5"},
		{patch: `"note":"updated"`, status: http.StatusOK, want: "5"},
		{patch: `"note":"rejected","weight":"2","max_in_flight":2`, status: http.StatusBadRequest, want: "5"},
	} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPatch, "/v8/management/credentials/fields", strings.NewReader(`{"id":"oauth-weight",`+test.patch+`}`))
		request.Header.Set("Content-Type", "application/json")
		engine.ServeHTTP(response, request)
		if response.Code != test.status {
			t.Fatalf("PATCH %s: status = %d body=%s", test.patch, response.Code, response.Body.String())
		}
		persisted, _, errPersisted := handler.repo.GetAuth(t.Context(), "oauth-weight")
		if errPersisted != nil {
			t.Fatal(errPersisted)
		}
		weight, exists := persisted.Metadata["weight"]
		if test.want == "" {
			if exists {
				t.Fatal("null weight persisted instead of removing the key")
			}
		} else {
			raw, errMarshal := json.Marshal(weight)
			if errMarshal != nil || string(raw) != test.want {
				t.Fatalf("persisted weight = %s, want %s", raw, test.want)
			}
		}
		if _, exists := persisted.Attributes["weight"]; exists {
			t.Fatal("stale scheduler weight attribute persisted")
		}
		if persisted.Attributes["priority"] != strconv.Itoa(10) || !reflect.DeepEqual(persisted.Metadata["token"], map[string]any{"access_token": "fixture"}) {
			t.Fatal("unrelated auth metadata changed")
		}
		if test.status == http.StatusBadRequest {
			if persisted.Metadata["note"] != "updated" {
				t.Fatal("rejected patch changed metadata")
			}
			policy, errPolicy := handler.repo.GetCredentialConcurrencyPolicy(t.Context(), "oauth-weight")
			if errPolicy != nil || policy.MaxInFlight != nil {
				t.Fatalf("rejected patch changed policy: %#v, %v", policy, errPolicy)
			}
		}
	}
}
