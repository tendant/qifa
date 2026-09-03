package deploy

import (
	"errors"
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

// A container that dies at startup is usually looking for an accessory that is
// not running — and since accessories are booted separately from app deploys,
// nothing else in the failure output mentions them.
func TestFormatAccessoryState(t *testing.T) {
	cases := []struct {
		name      string
		state     string
		err       error
		wantParts []string
	}{
		{
			name:      "missing container",
			err:       errors.New("Error: No such object"),
			wantParts: []string{"no container xiaoxiang-liver-accessory-postgres", "qifa accessory boot postgres"},
		},
		{
			name:      "never booted, empty state",
			state:     "",
			wantParts: []string{"no container", "accessory boot postgres"},
		},
		{
			name:      "stopped",
			state:     "exited exit=1 error=",
			wantParts: []string{"exited exit=1", "accessory boot postgres"},
		},
		{
			name:      "healthy",
			state:     "running exit=0 error=",
			wantParts: []string{"running"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatAccessoryState("postgres", "xiaoxiang-liver-accessory-postgres", tc.state, tc.err)
			for _, want := range tc.wantParts {
				if !strings.Contains(got, want) {
					t.Errorf("line %q is missing %q", got, want)
				}
			}
			if tc.name == "healthy" && strings.Contains(got, "accessory boot") {
				t.Errorf("a running accessory needs no fix suggestion: %q", got)
			}
		})
	}
}

// Printing https:// for an app served over plain HTTP points people at a port
// with no certificate behind it — the confusion this exists to avoid.
func TestProxyURL(t *testing.T) {
	cases := []struct {
		name                string
		host                string
		ssl                 bool
		httpPort, httpsPort int
		want                string
	}{
		{name: "plain http on 80", host: "app.example.com", httpPort: 80, httpsPort: 443, want: "http://app.example.com/"},
		{name: "tls on 443", host: "app.example.com", ssl: true, httpPort: 80, httpsPort: 443, want: "https://app.example.com/"},
		{name: "http on a non-default port", host: "app.example.com", httpPort: 8080, httpsPort: 443, want: "http://app.example.com:8080/"},
		{name: "tls on a non-default port", host: "app.example.com", ssl: true, httpPort: 80, httpsPort: 8443, want: "https://app.example.com:8443/"},
		{name: "unset ports fall back to the standard ones", host: "app.example.com", want: "http://app.example.com/"},
		{name: "host already carrying a port keeps it", host: "10.0.0.11:64600", httpPort: 6000, httpsPort: 443, want: "http://10.0.0.11:64600/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := proxyURL(tc.host, tc.ssl, tc.httpPort, tc.httpsPort); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
