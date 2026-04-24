package deploy

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/gokamal/gocart/internal/config"
	"github.com/gokamal/gocart/internal/state"
)

func TestOrderedRolesWebFirst(t *testing.T) {
	roles := orderedRoles(map[string]config.Server{
		"worker": {},
		"cron":   {},
		"web":    {},
	})
	if len(roles) != 3 {
		t.Fatalf("unexpected roles: %#v", roles)
	}
	if roles[0] != "web" {
		t.Fatalf("expected web first, got %#v", roles)
	}
}

func TestLatestContainerUsesLatestDeployment(t *testing.T) {
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	first := time.Now().UTC().Add(-time.Minute)
	second := time.Now().UTC()
	if err := store.AppendDeployment(state.Deployment{
		ID:        "1",
		Service:   "app",
		Version:   "v1",
		Image:     "img:v1",
		Status:    state.StatusSucceeded,
		StartedAt: first,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendDeployment(state.Deployment{
		ID:        "2",
		Service:   "app",
		Version:   "v2",
		Image:     "img:v2",
		Status:    state.StatusSucceeded,
		StartedAt: second,
	}); err != nil {
		t.Fatal(err)
	}

	d := &Deployer{
		cfg: &config.Config{Service: "app"},
		store: store,
	}
	got := d.latestContainer("web")
	if got != "app-web-v2" {
		t.Fatalf("unexpected latest container: %s", got)
	}
}

func TestDefaultTargetRequiresHosts(t *testing.T) {
	d := &Deployer{
		cfg: &config.Config{
			Servers: map[string]config.Server{
				"web": {},
			},
		},
	}
	_, _, err := d.defaultTarget()
	if err == nil {
		t.Fatal("expected error")
	}
}
