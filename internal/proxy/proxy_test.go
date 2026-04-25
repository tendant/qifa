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
		cfg: config.Proxy{
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

func TestBootCommandUsesConfiguredPorts(t *testing.T) {
	p := &KamalProxy{
		cfg: config.Proxy{
			HTTPPort:  8080,
			HTTPSPort: 8443,
		},
	}

	command := p.bootCommand()
	for _, fragment := range []string{
		"-p 8080:80",
		"-p 8443:443",
		"qifa-proxy",
		"basecamp/kamal-proxy:latest",
	} {
		if !strings.Contains(command, fragment) {
			t.Fatalf("missing fragment %q in %q", fragment, command)
		}
	}
}
