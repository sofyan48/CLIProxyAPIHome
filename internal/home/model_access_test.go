package home

import (
	"context"
	"errors"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
)

type modelAccessTestAdapter struct {
	ClusterAdapter
	policy *coreauth.ModelAccess
	err    error
}

func (a *modelAccessTestAdapter) ModelAccessForAPIKey(context.Context, string) (*coreauth.ModelAccess, error) {
	return a.policy, a.err
}

func TestRuntimeModelAccessForAPIKey(t *testing.T) {
	policy := coreauth.NewModelAccess([]string{"allowed"}, nil, nil)
	tests := []struct {
		name    string
		runtime *Runtime
		want    *coreauth.ModelAccess
		wantErr bool
	}{
		{name: "nil runtime", wantErr: true},
		{name: "standalone", runtime: &Runtime{}},
		{name: "missing policy interface", runtime: &Runtime{clusterAdapter: &modelChannelDispatchTestAdapter{}}, wantErr: true},
		{name: "nil policy", runtime: &Runtime{clusterAdapter: &modelAccessTestAdapter{}}, wantErr: true},
		{name: "policy error", runtime: &Runtime{clusterAdapter: &modelAccessTestAdapter{err: errors.New("unavailable")}}, wantErr: true},
		{name: "policy loaded", runtime: &Runtime{clusterAdapter: &modelAccessTestAdapter{policy: policy}}, want: policy},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, errPolicy := test.runtime.ModelAccessForAPIKey(context.Background(), "key")
			if (errPolicy != nil) != test.wantErr || got != test.want {
				t.Fatalf("ModelAccessForAPIKey() = %v, %v; want %v, error=%v", got, errPolicy, test.want, test.wantErr)
			}
		})
	}
}
