package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "godeploy.yml")
	if err := os.WriteFile(path, []byte(sampleConfig), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Service != "myapp" {
		t.Fatalf("unexpected service: %s", cfg.Service)
	}
	if cfg.Proxy.Healthcheck.Path != "/up" {
		t.Fatalf("unexpected path: %s", cfg.Proxy.Healthcheck.Path)
	}
	if !cfg.Proxy.TLS {
		t.Fatal("expected tls to be enabled in sample config")
	}
	if cfg.Proxy.TLSRedirect == nil || !*cfg.Proxy.TLSRedirect {
		t.Fatalf("unexpected tls redirect setting: %#v", cfg.Proxy.TLSRedirect)
	}
	if len(cfg.Proxy.PathPrefixes) != 1 || cfg.Proxy.PathPrefixes[0] != "/" {
		t.Fatalf("unexpected path prefixes: %#v", cfg.Proxy.PathPrefixes)
	}
	if cfg.Proxy.DeployTimeout != 30*time.Second || cfg.Proxy.DrainTimeout != 30*time.Second || cfg.Proxy.TargetTimeout != 30*time.Second {
		t.Fatalf("unexpected proxy timeouts: %#v", cfg.Proxy)
	}
}

func TestWriteSampleCreatesParents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "godeploy.yml")
	if err := WriteSample(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}
