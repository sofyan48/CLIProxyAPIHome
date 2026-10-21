package auth

import "testing"

func TestModelAccessAllows(t *testing.T) {
	tests := []struct {
		name       string
		authIDs    []string
		modelIDs   []string
		modelAuths map[string][]string
		auth       string
		model      string
		want       bool
	}{
		{name: "unrestricted", auth: "a", model: "model", want: true},
		{name: "empty credentials", authIDs: []string{}, auth: "a", model: "model"},
		{name: "empty models", modelIDs: []string{}, auth: "a", model: "model"},
		{name: "credential allowed", authIDs: []string{" a "}, auth: "a", model: "model", want: true},
		{name: "credential denied", authIDs: []string{"b"}, auth: "a", model: "model"},
		{name: "canonical public model", modelIDs: []string{" TENANT/alias(high) "}, auth: "a", model: "tenant/alias(low)", want: true},
		{name: "prefix preserved", modelIDs: []string{"alias"}, auth: "a", model: "tenant/alias"},
		{name: "model denied", modelIDs: []string{"other"}, auth: "a", model: "model"},
		{name: "model credential allowed", modelAuths: map[string][]string{"MODEL(high)": {"a"}}, auth: "a", model: "model(low)", want: true},
		{name: "canonical collision first credential", modelAuths: map[string][]string{"MODEL(high)": {"a"}, "model": {"b"}}, auth: "a", model: "model", want: true},
		{name: "canonical collision second credential", modelAuths: map[string][]string{"MODEL(high)": {"a"}, "model": {"b"}}, auth: "b", model: "model", want: true},
		{name: "canonical collision empty does not erase", modelAuths: map[string][]string{"MODEL(high)": {"a"}, "model": {}}, auth: "a", model: "model", want: true},
		{name: "canonical collision nil does not lift restriction", modelAuths: map[string][]string{"MODEL(high)": {}, "model": nil}, auth: "a", model: "model"},
		{name: "model credential denied", modelAuths: map[string][]string{"model": {"b"}}, auth: "a", model: "model"},
		{name: "empty model credentials", modelAuths: map[string][]string{"model": {}}, auth: "a", model: "model"},
		{name: "inherit model credentials", authIDs: []string{"a"}, modelAuths: map[string][]string{"model": nil}, auth: "a", model: "model", want: true},
		{name: "intersect credentials", authIDs: []string{"a"}, modelAuths: map[string][]string{"model": {"b"}}, auth: "b", model: "model"},
		{name: "unbound model inherits", authIDs: []string{"a"}, modelAuths: map[string][]string{"other": {}}, auth: "a", model: "model", want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy := NewModelAccess(test.authIDs, test.modelIDs, test.modelAuths)
			if got := policy.Allows(test.auth, test.model); got != test.want {
				t.Fatalf("Allows(%q, %q) = %v, want %v", test.auth, test.model, got, test.want)
			}
		})
	}
	var unscoped *ModelAccess
	if !unscoped.Allows("a", "model") {
		t.Fatal("nil policy must preserve the unscoped catalog")
	}
}
