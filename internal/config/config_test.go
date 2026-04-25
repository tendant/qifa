package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "qifa.yml")
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
	if cfg.Builder == nil {
		t.Fatal("expected builder block to be loaded")
	}
	if cfg.Builder.Context != "." {
		t.Fatalf("unexpected context: %q", cfg.Builder.Context)
	}
	if cfg.Builder.Dockerfile != "Dockerfile" {
		t.Fatalf("unexpected dockerfile: %q", cfg.Builder.Dockerfile)
	}
	if cfg.Builder.IsRemote() || cfg.Builder.IsPerTarget() {
		t.Fatalf("expected local build, got host=%q", cfg.Builder.Host)
	}
	if cfg.Prune.RetainContainers != 5 {
		t.Fatalf("unexpected retain_containers default: %d", cfg.Prune.RetainContainers)
	}
	if cfg.Proxy.Healthcheck.Interval != 2*time.Second {
		t.Fatalf("unexpected healthcheck interval default: %v", cfg.Proxy.Healthcheck.Interval)
	}
	if cfg.SSH.StrictHostKeyChecking != nil {
		t.Fatalf("expected nil strict host key setting by default, got %#v", cfg.SSH.StrictHostKeyChecking)
	}
}

func TestRolloutBatchSizeDefaults(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want int
	}{
		{"unset defaults to 1 (strict rolling)", "", 1},
		{"explicit 1", "rollout:\n  batch_size: 1\n", 1},
		{"explicit 0 means all-at-once", "rollout:\n  batch_size: 0\n", 0},
		{"explicit N", "rollout:\n  batch_size: 3\n", 3},
	}
	base := `service: app
image: registry.example.com/app
servers:
  web:
    hosts: [10.0.0.11]
registry:
  server: registry.example.com
  username: reg
  password_env: REGISTRY_PASSWORD
builder:
  context: .
`
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "qifa.yml")
			if err := os.WriteFile(path, []byte(base+tt.yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Rollout.BatchSize == nil {
				t.Fatal("expected BatchSize to be defaulted to a non-nil pointer")
			}
			if got := *cfg.Rollout.BatchSize; got != tt.want {
				t.Fatalf("BatchSize = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestParseImageVersion(t *testing.T) {
	tests := []struct {
		image   string
		want    string
		wantErr bool
	}{
		{"nginx:alpine", "alpine", false},
		{"nginx:latest", "latest", false},
		{"ghcr.io/org/app:v1.2.3", "v1.2.3", false},
		{"ghcr.io:5000/org/app:v1.2.3", "v1.2.3", false},
		{"ghcr.io/org/app@sha256:abc", "sha256:abc", false},
		{"nginx", "", true},
		{"nginx:", "", true},
		{"ghcr.io/org/app", "", true},
		{"", "", true},
	}
	for _, tt := range tests {
		got, err := ParseImageVersion(tt.image)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ParseImageVersion(%q) = %q, want error", tt.image, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseImageVersion(%q) error: %v", tt.image, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseImageVersion(%q) = %q, want %q", tt.image, got, tt.want)
		}
	}
}

func TestValidateBuilderShapes(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{
			name:   "local build with registry",
			mutate: func(c *Config) {},
		},
		{
			name: "remote build with registry",
			mutate: func(c *Config) {
				c.Builder.Host = "10.0.0.99"
			},
		},
		{
			name: "git source",
			mutate: func(c *Config) {
				c.Builder.Context = ""
				c.Builder.Repo = "git@example.com:org/app.git"
				c.Builder.Ref = "v1.2.3"
				c.Builder.Subdir = "."
			},
		},
		{
			name: "per_target builds without registry",
			mutate: func(c *Config) {
				c.Builder.Host = BuilderHostPerTarget
				c.Registry = Registry{}
				c.Image = "myapp"
			},
		},
		{
			name: "external image (no builder) with tagged image",
			mutate: func(c *Config) {
				c.Builder = nil
				c.Image = "nginx:1.27"
				c.Registry = Registry{}
			},
		},
		{
			name: "local build requires registry",
			mutate: func(c *Config) {
				c.Registry = Registry{}
			},
			wantErr: "config.registry is required",
		},
		{
			name: "per_target forbids registry",
			mutate: func(c *Config) {
				c.Builder.Host = BuilderHostPerTarget
			},
			wantErr: "config.registry must not be set",
		},
		{
			name: "git requires ref",
			mutate: func(c *Config) {
				c.Builder.Context = ""
				c.Builder.Repo = "git@example.com:org/app.git"
			},
			wantErr: "config.builder.ref is required",
		},
		{
			name: "git forbids context",
			mutate: func(c *Config) {
				c.Builder.Repo = "git@example.com:org/app.git"
				c.Builder.Ref = "v1"
			},
			wantErr: "config.builder.context must not be set",
		},
		{
			name: "external image must be tagged",
			mutate: func(c *Config) {
				c.Builder = nil
				c.Image = "nginx"
				c.Registry = Registry{}
			},
			wantErr: "config.image",
		},
		{
			name: "external image accepts :latest (resolved to digest at deploy time)",
			mutate: func(c *Config) {
				c.Builder = nil
				c.Image = "nginx:latest"
				c.Registry = Registry{}
			},
		},
		{
			name: "single platform is fine",
			mutate: func(c *Config) {
				c.Builder.Platform = "linux/amd64"
			},
		},
		{
			name: "multi-platform with registry is fine",
			mutate: func(c *Config) {
				c.Builder.Platform = "linux/amd64,linux/arm64"
			},
		},
		{
			name: "multi-platform requires registry",
			mutate: func(c *Config) {
				c.Builder.Host = BuilderHostPerTarget
				c.Builder.Platform = "linux/amd64,linux/arm64"
				c.Registry = Registry{}
				c.Image = "myapp"
			},
			wantErr: "must be a single platform when config.builder.host=per_target",
		},
		{
			name: "multi-platform forbidden with per_target",
			mutate: func(c *Config) {
				c.Builder.Host = BuilderHostPerTarget
				c.Builder.Platform = "linux/amd64,linux/arm64"
				c.Registry = Registry{}
				c.Image = "myapp"
			},
			wantErr: "config.builder.host=per_target",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.mutate(&cfg)
			err := cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestWriteSampleCreatesParents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "qifa.yml")
	if err := WriteSample(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func validConfig() Config {
	return Config{
		Service: "app",
		Image:   "registry.example.com/app",
		Servers: map[string]Server{
			"web": {Hosts: []string{"10.0.0.11"}},
		},
		Registry: Registry{
			Server:      "registry.example.com",
			Username:    "reg",
			PasswordEnv: "REGISTRY_PASSWORD",
		},
		Builder: &Builder{
			Context:    ".",
			Dockerfile: "Dockerfile",
		},
		SSH: SSH{
			User: "ubuntu",
			Key:  "~/.ssh/id_ed25519",
		},
	}
}
