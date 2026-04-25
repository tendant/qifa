package state

import (
	"path/filepath"
	"testing"
	"time"
)

func TestStoreSnapshotAndLatestSuccessful(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "state.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := store.AppendDeployment(Deployment{
		ID:        "1",
		Service:   "app",
		Version:   "v1",
		Image:     "img:v1",
		Status:    StatusSucceeded,
		StartedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendEvent(Event{
		ID:           "e1",
		DeploymentID: "1",
		EventType:    "test",
		Message:      "ok",
		CreatedAt:    now,
	}); err != nil {
		t.Fatal(err)
	}

	deployments, events, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(deployments) != 1 || len(events) != 1 {
		t.Fatalf("unexpected snapshot sizes: %d %d", len(deployments), len(events))
	}

	latest, err := store.LatestSuccessful("app")
	if err != nil {
		t.Fatal(err)
	}
	if latest.Version != "v1" {
		t.Fatalf("unexpected version: %s", latest.Version)
	}
}
