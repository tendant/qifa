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

func TestLatestContainerUsesActiveTarget(t *testing.T) {
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	second := time.Now().UTC()
	if err := store.AppendActiveTarget(state.ActiveTarget{
		Service:   "app",
		Host:      "host1",
		Role:      "web",
		Version:   "v2",
		Image:     "img:v2",
		Container: "app-web-v2",
		UpdatedAt: second,
	}); err != nil {
		t.Fatal(err)
	}

	d := &Deployer{
		cfg:   &config.Config{Service: "app"},
		store: store,
	}
	got := d.latestContainer("web", "host1")
	if got != "app-web-v2" {
		t.Fatalf("unexpected latest container: %s", got)
	}
}

func TestDefaultTargetPrefersActiveTarget(t *testing.T) {
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AppendActiveTarget(state.ActiveTarget{
		Service:   "app",
		Host:      "host1",
		Role:      "web",
		Version:   "v2",
		Image:     "img:v2",
		Container: "app-web-v2",
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	d := &Deployer{
		cfg: &config.Config{
			Service: "app",
			Servers: map[string]config.Server{
				"web": {Hosts: []string{"host1"}},
			},
		},
		store: store,
	}
	host, container, err := d.defaultTarget()
	if err != nil {
		t.Fatal(err)
	}
	if host != "host1" || container != "app-web-v2" {
		t.Fatalf("unexpected target: %s %s", host, container)
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

func TestServerUsesProxy(t *testing.T) {
	falseValue := false
	trueValue := true

	tests := []struct {
		name   string
		role   string
		server config.Server
		want   bool
	}{
		{
			name: "web defaults true",
			role: "web",
			want: true,
		},
		{
			name: "non web defaults false",
			role: "app",
			want: false,
		},
		{
			name: "web can disable proxy",
			role: "web",
			server: config.Server{
				Proxy: &falseValue,
			},
			want: false,
		},
		{
			name: "non web can enable proxy",
			role: "app",
			server: config.Server{
				Proxy: &trueValue,
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := serverUsesProxy(tt.role, tt.server); got != tt.want {
				t.Fatalf("serverUsesProxy(%q) = %v, want %v", tt.role, got, tt.want)
			}
		})
	}
}
