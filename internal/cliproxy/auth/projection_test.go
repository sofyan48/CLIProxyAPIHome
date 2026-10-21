package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

func TestProjectionSkipsResultSnapshotsButRejectsCompleteSave(t *testing.T) {
	ctx := context.Background()
	for _, metadata := range []map[string]any{nil, {"weight": 5}, {"request_retry": 2}, {"weight": 5, "request_retry": 2}} {
		store := &nonVersionedUpdateStore{}
		manager := NewManager(store, nil, nil)
		t.Cleanup(manager.Shutdown)
		auth := &Auth{ID: "projection", Index: "projection", Provider: "codex", Status: StatusActive, RuntimeProjection: true, Metadata: metadata}
		if _, errRegister := manager.Register(ctx, auth); !errors.Is(errRegister, ErrRuntimeProjectionPersist) {
			t.Fatalf("Register error = %v, want projection rejection", errRegister)
		}
		if _, errRegister := manager.Register(WithSkipPersist(ctx), auth); errRegister != nil {
			t.Fatal(errRegister)
		}
		if _, errUpdate := manager.Update(ctx, auth); !errors.Is(errUpdate, ErrRuntimeProjectionPersist) {
			t.Fatalf("Update error = %v, want projection rejection", errUpdate)
		}
		manager.MarkResult(ctx, Result{AuthID: auth.ID, Model: "model", Error: &Error{HTTPStatus: http.StatusInternalServerError}})
		local, ok := manager.GetByID(auth.ID)
		if !ok || local.ModelStates["model"] == nil || local.ModelStates["model"].Status != StatusError {
			t.Fatal("local execution state was lost")
		}
		manager.resultPersistMu.Lock()
		pending, active := len(manager.resultPersistPending), len(manager.resultPersistActive)
		manager.resultPersistMu.Unlock()
		if pending != 0 || active != 0 {
			t.Fatal("projection started background snapshot persistence")
		}
		if errPersist := manager.persistQueued(ctx, auth); errPersist != nil {
			t.Fatal(errPersist)
		}
		if _, saves := store.snapshot(); saves != 0 {
			t.Fatal("projection reached the complete-record store")
		}
	}
}

func TestQueuedFullAuthReplacedByProjectionIsNotSaved(t *testing.T) {
	ctx := context.Background()
	full := &Auth{ID: "reload", Index: "reload", Provider: "codex", StateVersion: 1, Metadata: map[string]any{"access_token": "preserved"}}
	store := newBlockingVersionedUpdateStore(full)
	// Do not block a save if the guard regresses; assert the resulting content.
	close(store.releaseFirst)
	manager := NewManager(store, nil, nil)
	t.Cleanup(manager.Shutdown)
	if _, errRegister := manager.Register(WithSkipPersist(ctx), full); errRegister != nil {
		t.Fatal(errRegister)
	}
	projection := full.Clone()
	projection.RuntimeProjection = true
	projection.Metadata = map[string]any{"weight": 5}
	if _, errUpdate := manager.Update(WithSkipPersist(ctx), projection); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	if errPersist := manager.persistQueued(ctx, full); errPersist != nil {
		t.Fatal(errPersist)
	}
	if stored := store.snapshot(); stored.StateVersion != 1 || stored.Metadata["access_token"] != "preserved" {
		t.Fatal("queued full snapshot saved the replacement projection")
	}
}

func TestProjectionMarkerCloneAndCompleteReplacement(t *testing.T) {
	auth := &Auth{ID: "projection", RuntimeProjection: true}
	if !auth.Clone().RuntimeProjection {
		t.Fatal("Clone lost projection marker")
	}
	payload, errMarshal := json.Marshal(auth)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	var full Auth
	if errUnmarshal := json.Unmarshal(payload, &full); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if full.RuntimeProjection {
		t.Fatal("projection marker was serialized")
	}
	store := &nonVersionedUpdateStore{}
	manager := NewManager(store, nil, nil)
	t.Cleanup(manager.Shutdown)
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatal(errRegister)
	}
	full.Metadata = map[string]any{"access_token": "replacement"}
	updated, errUpdate := manager.Update(context.Background(), &full)
	if errUpdate != nil || updated.RuntimeProjection {
		t.Fatalf("complete replacement failed: %v", errUpdate)
	}
	if stored, saves := store.snapshot(); saves != 1 || stored.Metadata["access_token"] != "replacement" {
		t.Fatal("complete credential replacement was not persisted")
	}
}
