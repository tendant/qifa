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

	if err := store.AppendActiveTarget(ActiveTarget{
		Service:      "app",
		Host:         "host1",
		Role:         "web",
		DeploymentID: "1",
		Version:      "v1",
		Image:        "img:v1",
		Container:    "app-web-v1",
		TargetHost:   "10.0.0.2",
		TargetPort:   3000,
		UpdatedAt:    now,
	}); err != nil {
		t.Fatal(err)
	}

	active, err := store.ActiveTarget("app", "host1", "web")
	if err != nil {
		t.Fatal(err)
	}
	if active.Container != "app-web-v1" {
		t.Fatalf("unexpected active container: %s", active.Container)
	}
}

func TestRollbackTargetPrefersPreviousSuccessful(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "state.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	first := time.Now().UTC().Add(-2 * time.Minute)
	second := time.Now().UTC().Add(-time.Minute)
	if err := store.AppendDeployment(Deployment{
		ID:        "1",
		Service:   "app",
		Version:   "v1",
		Image:     "img:v1",
		Status:    StatusSucceeded,
		StartedAt: first,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendDeployment(Deployment{
		ID:        "2",
		Service:   "app",
		Version:   "v2",
		Image:     "img:v2",
		Status:    StatusSucceeded,
		StartedAt: second,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendActiveTarget(ActiveTarget{
		Service:      "app",
		Host:         "host1",
		Role:         "web",
		DeploymentID: "2",
		Version:      "v2",
		Image:        "img:v2",
		Container:    "app-web-v2",
		UpdatedAt:    second,
	}); err != nil {
		t.Fatal(err)
	}

	target, err := store.RollbackTarget("app")
	if err != nil {
		t.Fatal(err)
	}
	if target.Version != "v1" {
		t.Fatalf("unexpected rollback target: %s", target.Version)
	}
}

func TestRollbackTargetFallsBackToActiveVersionWhenNoPreviousExists(t *testing.T) {
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
	if err := store.AppendActiveTarget(ActiveTarget{
		Service:      "app",
		Host:         "host1",
		Role:         "web",
		DeploymentID: "1",
		Version:      "v1",
		Image:        "img:v1",
		Container:    "app-web-v1",
		UpdatedAt:    now,
	}); err != nil {
		t.Fatal(err)
	}

	target, err := store.RollbackTarget("app")
	if err != nil {
		t.Fatal(err)
	}
	if target.Version != "v1" {
		t.Fatalf("unexpected rollback target: %s", target.Version)
	}
}
