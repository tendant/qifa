package dockererr

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestClassifyRecognisesCommonPullFailures(t *testing.T) {
	cases := []struct {
		name      string
		output    string
		want      Category
		retryable bool
	}{
		{
			name:      "dns",
			output:    `Error response from daemon: Get "https://ghcr.io/v2/": dial tcp: lookup ghcr.io on 10.0.0.2:53: no such host`,
			want:      CategoryDNS,
			retryable: true,
		},
		{
			name:      "tls handshake timeout",
			output:    `Error response from daemon: Get "https://registry-1.docker.io/v2/": net/http: TLS handshake timeout`,
			want:      CategoryTLSHandshake,
			retryable: true,
		},
		{
			name:      "i/o timeout",
			output:    `Get "https://registry.example.com/v2/": dial tcp 10.1.2.3:443: i/o timeout`,
			want:      CategoryTimeout,
			retryable: true,
		},
		{
			name:   "unauthorized",
			output: `Error response from daemon: Head "https://ghcr.io/v2/acme/app/manifests/v1": unauthorized`,
			want:   CategoryAuth,
		},
		{
			name:      "rate limit",
			output:    `toomanyrequests: You have reached your pull rate limit.`,
			want:      CategoryRateLimit,
			retryable: true,
		},
		{
			name:   "missing tag",
			output: `Error response from daemon: manifest for registry.example.com/app:v9 not found: manifest unknown`,
			want:   CategoryNotFound,
		},
		{
			name:   "disk full",
			output: `failed to register layer: write /var/lib/docker/overlay2/...: no space left on device`,
			want:   CategoryDisk,
		},
		{
			name:   "platform",
			output: `no matching manifest for linux/arm64/v8 in the manifest list entries`,
			want:   CategoryPlatform,
		},
		{
			name:      "daemon proxy",
			output:    `Get "https://ghcr.io/v2/": proxyconnect tcp: dial tcp 10.0.0.9:3128: connect: connection refused`,
			want:      CategoryDaemonProxy,
			retryable: true,
		},
		{
			name:   "daemon down",
			output: `Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?`,
			want:   CategoryDaemonDown,
		},
		{
			name:   "insecure registry",
			output: `http: server gave HTTP response to HTTPS client`,
			want:   CategoryHTTPSMismatch,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cause, ok := Classify(tc.output)
			if !ok {
				t.Fatalf("expected %q to be classified", tc.output)
			}
			if cause.Category != tc.want {
				t.Fatalf("got category %q, want %q", cause.Category, tc.want)
			}
			if cause.Retryable != tc.retryable {
				t.Fatalf("got retryable=%v, want %v", cause.Retryable, tc.retryable)
			}
			if cause.Summary == "" {
				t.Fatal("classified causes must carry a summary")
			}
		})
	}
}

// The daemon-proxy signature contains "connection refused"; the more specific
// rule has to win, since the fix is entirely different.
func TestClassifyPrefersTheMoreSpecificRule(t *testing.T) {
	cause, _ := Classify(`proxyconnect tcp: dial tcp 10.0.0.9:3128: connect: connection refused`)
	if cause.Category != CategoryDaemonProxy {
		t.Fatalf("got %q, want %q", cause.Category, CategoryDaemonProxy)
	}
}

func TestClassifyUnknownOutput(t *testing.T) {
	if _, ok := Classify("something entirely novel went wrong"); ok {
		t.Fatal("expected no classification")
	}
}

type outputErr struct{ out string }

func (e outputErr) Error() string        { return "remote command failed" }
func (e outputErr) RemoteOutput() string { return e.out }

func TestClassifyErrReadsCapturedRemoteOutput(t *testing.T) {
	err := fmt.Errorf("deploy web: %w", outputErr{out: "dial tcp: lookup ghcr.io: no such host"})
	cause, ok := ClassifyErr(err)
	if !ok || cause.Category != CategoryDNS {
		t.Fatalf("got (%q, %v), want dns", cause.Category, ok)
	}
	if !Retryable(err) {
		t.Fatal("DNS failures should be retryable")
	}
}

func TestRetryableIsFalseForUnknownAndPermanentFailures(t *testing.T) {
	if Retryable(errors.New("no idea")) {
		t.Fatal("unknown failures must not be retried")
	}
	if Retryable(outputErr{out: "manifest unknown"}) {
		t.Fatal("a missing tag must not be retried")
	}
}

func TestRegistryHost(t *testing.T) {
	cases := map[string]string{
		"nginx":                            "registry-1.docker.io",
		"library/nginx:1.27":               "registry-1.docker.io",
		"docker.io/acme/app:v1":            "registry-1.docker.io",
		"ghcr.io/acme/app:v1":              "ghcr.io",
		"registry.example.com:5000/app:v1": "registry.example.com:5000",
		"localhost:5000/app":               "localhost:5000",
		"ghcr.io/acme/app@sha256:abc":      "ghcr.io",
	}
	for image, want := range cases {
		if got := RegistryHost(image); got != want {
			t.Errorf("RegistryHost(%q) = %q, want %q", image, got, want)
		}
	}
}

func TestDockerHubProbesAllThreeEndpoints(t *testing.T) {
	hosts := probeHosts("registry-1.docker.io")
	if len(hosts) != 3 {
		t.Fatalf("want 3 Docker Hub endpoints, got %v", hosts)
	}
	script := diagnoseScript("registry-1.docker.io", "nginx:1.27")
	for _, host := range hosts {
		if !strings.Contains(script, host.hostport) {
			t.Errorf("probe script does not check %s", host.hostport)
		}
	}
	// Only the registry itself serves /v2/; the token service and the layer
	// CDN answer 404/403 there, which would read as a problem when it is not.
	if got := strings.Count(script, `curl -sS -o /dev/null`); got != 1 {
		t.Errorf("want exactly one /v2/ probe (the registry host), found %d", got)
	}
}

// A published host port can only belong to one container, so a redeploy of a
// role that publishes one collides with its own previous version. That is not
// a network fault and should not send anyone to `qifa doctor`.
func TestClassifyPortAlreadyAllocated(t *testing.T) {
	out := `docker: Error response from daemon: failed to set up container networking: driver failed programming external connectivity on endpoint app-web-1: Bind for :::6001 failed: port is already allocated`
	cause, ok := Classify(out)
	if !ok || cause.Category != CategoryPortInUse {
		t.Fatalf("got (%q, %v), want host-port-in-use", cause.Category, ok)
	}
	if cause.Network {
		t.Error("a port conflict is local; probing the registry adds nothing")
	}
	if cause.Retryable {
		t.Error("retrying cannot free the port")
	}
}
