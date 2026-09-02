package deploy

import (
	"strings"
	"testing"

	"github.com/gokamal/gocart/internal/config"
)

func TestDoctorTargetsCoverEveryHostAndSayWhy(t *testing.T) {
	d := &Deployer{cfg: &config.Config{
		Service: "testapp",
		Image:   "ghcr.io/acme/app:v1",
		Servers: map[string]config.Server{
			"web":    {Hosts: []string{"10.0.0.11", "10.0.0.12"}},
			"worker": {Hosts: []string{"10.0.0.12"}},
		},
		ProxyBoot:   config.ProxyBoot{Hosts: []string{"10.0.0.11"}},
		Accessories: map[string]config.Accessory{"db": {Host: "10.0.0.13"}},
		Builder:     &config.Builder{Host: "10.0.0.14"},
	}}

	targets := d.doctorTargets()
	if len(targets) != 4 {
		t.Fatalf("want 4 distinct hosts, got %d: %+v", len(targets), targets)
	}
	// Sorted, so the order is stable across runs.
	if targets[0].host != "10.0.0.11" || targets[3].host != "10.0.0.14" {
		t.Fatalf("hosts are not sorted: %+v", targets)
	}
	// A host serving two roles is listed once, with both reasons.
	roles := strings.Join(targets[1].roles, ",")
	if !strings.Contains(roles, "app/web") || !strings.Contains(roles, "app/worker") {
		t.Fatalf("10.0.0.12 should list both roles, got %q", roles)
	}
	if got := strings.Join(targets[0].roles, ","); !strings.Contains(got, "proxy") {
		t.Fatalf("10.0.0.11 should be flagged as a proxy host, got %q", got)
	}
	if got := strings.Join(targets[3].roles, ","); got != "builder" {
		t.Fatalf("10.0.0.14 should be the builder, got %q", got)
	}
}

func TestDoctorTargetsSkipsBlankHosts(t *testing.T) {
	d := &Deployer{cfg: &config.Config{
		Servers:     map[string]config.Server{"web": {Hosts: []string{"10.0.0.11"}}},
		Accessories: map[string]config.Accessory{"db": {}}, // no host set
	}}
	targets := d.doctorTargets()
	if len(targets) != 1 {
		t.Fatalf("want 1 host, got %+v", targets)
	}
}
