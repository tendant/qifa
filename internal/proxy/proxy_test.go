package proxy

import (
	"strings"
	"testing"
	"time"

	"github.com/gokamal/gocart/internal/config"
)

func TestDeployCommandIncludesConfiguredFlags(t *testing.T) {
	tlsRedirect := false
	forwardHeaders := true
	stripPathPrefix := false

	p := &KamalProxy{
		app: config.Proxy{
			Host:            "app.example.com",
			Hosts:           []string{"www.example.com"},
			DeployTimeout:   45 * time.Second,
			DrainTimeout:    20 * time.Second,
			TargetTimeout:   15 * time.Second,
			TLS:             true,
			TLSRedirect:     &tlsRedirect,
			TLSStaging:      true,
			ForwardHeaders:  &forwardHeaders,
			PathPrefixes:    []string{"/", "/api"},
			StripPathPrefix: &stripPathPrefix,
			Healthcheck: config.Healthcheck{
				Path:     "/ready",
				Interval: 3 * time.Second,
				Timeout:  4 * time.Second,
			},
		},
	}

	command := p.deployCommand(Target{
		Service: "app",
		Host:    "172.18.0.5",
		Port:    3000,
	})

	for _, fragment := range []string{
		"kamal-proxy deploy 'app'",
		"--host 'app.example.com'",
		"--host 'www.example.com'",
		"--target '172.18.0.5:3000'",
		"--health-check-path '/ready'",
		"--health-check-interval '3s'",
		"--health-check-timeout '4s'",
		"--deploy-timeout '45s'",
		"--drain-timeout '20s'",
		"--target-timeout '15s'",
		"--tls",
		"--tls-redirect=false",
		"--tls-staging",
		"--forward-headers=true",
		"--path-prefix '/'",
		"--path-prefix '/api'",
		"--strip-path-prefix=false",
	} {
		if !strings.Contains(command, fragment) {
			t.Fatalf("missing fragment %q in %q", fragment, command)
		}
	}
}

func TestBootCommandWithBindIPs(t *testing.T) {
	tests := []struct {
		name    string
		bindIPs []string
		want    []string // fragments that must appear
		notWant []string // fragments that must NOT appear
	}{
		{
			name:    "no bind_ips defaults to 0.0.0.0",
			bindIPs: nil,
			want:    []string{"-p 80:80", "-p 443:443"},
			notWant: []string{":80:80"},
		},
		{
			name:    "single IPv4",
			bindIPs: []string{"192.168.1.5"},
			want:    []string{"-p 192.168.1.5:80:80", "-p 192.168.1.5:443:443"},
			notWant: []string{"-p 80:80", "-p 443:443"},
		},
		{
			name:    "multiple IPv4 (e.g. public + internal NIC)",
			bindIPs: []string{"1.2.3.4", "192.168.1.5"},
			want: []string{
				"-p 1.2.3.4:80:80", "-p 1.2.3.4:443:443",
				"-p 192.168.1.5:80:80", "-p 192.168.1.5:443:443",
			},
		},
		{
			name:    "IPv6 wrapped in brackets",
			bindIPs: []string{"::1"},
			want:    []string{"-p [::1]:80:80", "-p [::1]:443:443"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &KamalProxy{
				boot: config.ProxyBoot{
					HTTPPort:      80,
					HTTPSPort:     443,
					Image:         "basecamp/kamal-proxy",
					Network:       "kamal",
					StateVolume:   "kamal-proxy-config",
					AppsConfigDir: ".kamal/proxy/apps-config",
					BindIPs:       tt.bindIPs,
				},
			}
			cmd := p.bootCommand()
			for _, want := range tt.want {
				if !strings.Contains(cmd, want) {
					t.Errorf("missing %q in %q", want, cmd)
				}
			}
			for _, notWant := range tt.notWant {
				if strings.Contains(cmd, notWant) {
					t.Errorf("unexpected %q in %q", notWant, cmd)
				}
			}
		})
	}
}

func TestBootCommandUsesConfiguredPorts(t *testing.T) {
	p := &KamalProxy{
		boot: config.ProxyBoot{
			HTTPPort:      8080,
			HTTPSPort:     8443,
			Image:         "basecamp/kamal-proxy",
			Version:       "v0.9.2",
			Network:       "kamal",
			StateVolume:   "kamal-proxy-config",
			AppsConfigDir: ".kamal/proxy/apps-config",
		},
	}

	command := p.bootCommand()
	for _, fragment := range []string{
		"docker network create 'kamal'",
		"mkdir -p '.kamal/proxy/apps-config'",
		"-p 8080:80",
		"-p 8443:443",
		"--network 'kamal'",
		"--volume 'kamal-proxy-config:/home/kamal-proxy/.config/kamal-proxy'",
		"--volume '.kamal/proxy/apps-config:/home/kamal-proxy/.apps-config'",
		"--log-opt max-size=10m",
		"kamal-proxy run",
		"basecamp/kamal-proxy:v0.9.2",
	} {
		if !strings.Contains(command, fragment) {
			t.Fatalf("missing fragment %q in %q", fragment, command)
		}
	}
}
