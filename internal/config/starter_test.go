package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every shape qifa init can write loads without warnings and says what was
// asked: where it deploys, whether it builds, and whether it is proxied.
func TestStarterShapesLoad(t *testing.T) {
	for _, host := range []string{"", "10.0.0.11", "deploy@10.0.0.11:2222"} {
		for _, domain := range []string{"", "app.example.com"} {
			for _, build := range []bool{false, true} {
				s := Starter{Service: "shop", Host: host, Domain: domain, Build: build}
				if build {
					s.AppPort = 3000
				}
				name := strings.Join([]string{"host=" + host, "domain=" + domain, map[bool]string{true: "build", false: "pull"}[build]}, ",")
				t.Run(name, func(t *testing.T) {
					text := s.YAML()
					cfg, err := loadYAML(t, text)
					if err != nil {
						t.Fatalf("%v\n%s", err, text)
					}
					if len(cfg.Warnings) > 0 {
						t.Errorf("warns: %v", cfg.Warnings)
					}
					web := cfg.Servers["web"]
					wantHost := host
					if wantHost == "" {
						wantHost = "local"
					}
					if len(web.Hosts) != 1 || web.Hosts[0] != wantHost {
						t.Errorf("hosts = %v, want [%s]", web.Hosts, wantHost)
					}
					if build != cfg.Builder.IsPerTarget() {
						t.Errorf("builder = %+v, want per_target %v", cfg.Builder, build)
					}
					if (domain != "") != (web.Proxy != nil && *web.Proxy) || cfg.Proxy.Host != domain {
						t.Errorf("proxy = %v host %q, want domain %q", web.Proxy, cfg.Proxy.Host, domain)
					}
					if domain != "" && web.Port != 0 {
						t.Errorf("a proxied role publishes port %d", web.Port)
					}
					if build && web.AppPort != 3000 {
						t.Errorf("app_port = %d, want 3000", web.AppPort)
					}
					// The hints name the machine, not the SSH user or port.
					if strings.Contains(text, "# http://deploy@") || strings.Contains(text, ":2222:8765") || strings.Contains(text, "# deploy@") {
						t.Errorf("a hint carries the SSH user or port:\n%s", text)
					}
				})
			}
		}
	}
}

// The starter keeps a build context with odd characters a single YAML
// string.
func TestStarterQuotesAnOddContext(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "my app: v2")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadYAML(t, Starter{Build: true, Context: dir}.YAML())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Builder.Context != dir {
		t.Errorf("context = %q, want %q", cfg.Builder.Context, dir)
	}
}

func TestDetectStarter(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "My_Shop.API")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	s := DetectStarter(dir)
	if s.Service != "my-shop-api" || s.Build {
		t.Errorf("no Dockerfile: %+v", s)
	}
	dockerfile := "FROM golang AS build\nEXPOSE 9000\nFROM alpine\nexpose 3000/tcp 3001\nCMD [\"/app\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	s = DetectStarter(dir)
	if !s.Build || s.AppPort != 3000 || s.Context != dir {
		t.Errorf("with a Dockerfile: %+v", s)
	}
}

func TestExposedPort(t *testing.T) {
	for text, want := range map[string]int{
		"FROM x\n":                       0,
		"FROM x\nEXPOSE 8080\n":          8080,
		"EXPOSE 80\nFROM y\nEXPOSE 81\n": 81,
		"EXPOSE $PORT\n":                 0,
		"EXPOSE 80\nEXPOSE ${PORT}\n":    0, // the last one wins, and is unknown
		"EXPOSE 70000\n":                 0,
	} {
		if got := exposedPort([]byte(text)); got != want {
			t.Errorf("%q: got %d, want %d", text, got, want)
		}
	}
}

func TestServiceNameFrom(t *testing.T) {
	for in, want := range map[string]string{
		"shop":      "shop",
		"My Shop":   "my-shop",
		"--api--v2": "api-v2",
		"2048":      "", // must start with a letter
		"日本":        "",
	} {
		if got := serviceNameFrom(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

func TestStarterCheck(t *testing.T) {
	for _, s := range []Starter{
		{Host: "a b"},
		{Host: "a,b"},
		{Host: "[a]"},
		{Host: "a:b"},
		{Domain: "https://app.example.com"},
		{Domain: "app.example.com/x"},
		{Domain: "app example"},
	} {
		if err := s.Check(); err == nil {
			t.Errorf("%+v passed", s)
		}
	}
	for _, s := range []Starter{{Host: "10.0.0.11"}, {Host: "deploy@box:2222"}, {Domain: "app.example.com"}} {
		if err := s.Check(); err != nil {
			t.Errorf("%+v: %v", s, err)
		}
	}
}
