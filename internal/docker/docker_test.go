package docker

import (
	"strings"
	"testing"
)

func TestDockerfileIn(t *testing.T) {
	cases := []struct {
		name       string
		contextDir string
		dockerfile string
		want       string
	}{
		{
			name:       "relative joins the context",
			contextDir: "/src/app",
			dockerfile: "Dockerfile",
			want:       "/src/app/Dockerfile",
		},
		{
			name:       "relative subdirectory",
			contextDir: "/src/app",
			dockerfile: "docker/Dockerfile.prod",
			want:       "/src/app/docker/Dockerfile.prod",
		},
		{
			// `docker build -f /abs/Dockerfile ctx` is valid and people write
			// it; joining it to the context produced "<context>/private/tmp/…"
			// and the opaque "unable to evaluate symlinks in Dockerfile path".
			name:       "absolute is honoured as written",
			contextDir: "/src/app",
			dockerfile: "/src/app/Dockerfile",
			want:       "/src/app/Dockerfile",
		},
		{
			name:       "absolute outside the context is still honoured",
			contextDir: "/src/app",
			dockerfile: "/etc/build/Dockerfile",
			want:       "/etc/build/Dockerfile",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dockerfileIn(tc.contextDir, tc.dockerfile); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRemoteDockerfileIn(t *testing.T) {
	const remote = "/tmp/qifa-build-1/context"

	cases := []struct {
		name         string
		localContext string
		dockerfile   string
		want         string
	}{
		{
			name:         "relative joins the uploaded context",
			localContext: "/src/app",
			dockerfile:   "Dockerfile",
			want:         remote + "/Dockerfile",
		},
		{
			// The local absolute path does not exist on the host, but its
			// position inside the context does.
			name:         "absolute inside the context keeps its position",
			localContext: "/src/app",
			dockerfile:   "/src/app/docker/Dockerfile",
			want:         remote + "/docker/Dockerfile",
		},
		{
			name:         "absolute outside the context falls back to the base name",
			localContext: "/src/app",
			dockerfile:   "/etc/build/Dockerfile.prod",
			want:         remote + "/Dockerfile.prod",
		},
		{
			// Git builds clone on the host: there is no local context.
			name:         "no local context falls back to the base name",
			localContext: "",
			dockerfile:   "/src/app/Dockerfile",
			want:         remote + "/Dockerfile",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := remoteDockerfileIn(tc.localContext, remote, tc.dockerfile); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseContainerIP(t *testing.T) {
	ip, err := parseContainerIP("172.18.0.5\n", "app-web-1", "10.0.0.11")
	if err != nil || ip != "172.18.0.5" {
		t.Fatalf("got (%q, %v), want the address", ip, err)
	}

	// docker 28 renders an unset netip.Addr like this. Passed through, it
	// became http://invalid IP:8090/readyz and the healthcheck failed with
	// "Could not resolve host: invalid" instead of "the container is down".
	for _, out := range []string{"invalid IP", "", "  ", "<no value>"} {
		if _, err := parseContainerIP(out, "app-web-1", "10.0.0.11"); err == nil {
			t.Errorf("parseContainerIP(%q) should refuse to produce an address", out)
		} else {
			for _, want := range []string{"app-web-1", "10.0.0.11", "not running"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error for %q is missing %q: %v", out, want, err)
				}
			}
		}
	}
}
