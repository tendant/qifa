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
	if cfg.Builder.Mode != "local" {
		t.Fatalf("unexpected builder mode: %s", cfg.Builder.Mode)
	}
	if cfg.Builder.Source != "local" {
		t.Fatalf("unexpected builder source: %s", cfg.Builder.Source)
	}
	if cfg.SSH.StrictHostKeyChecking != nil {
		t.Fatalf("expected nil strict host key setting by default, got %#v", cfg.SSH.StrictHostKeyChecking)
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

func TestValidateBuilderModes(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{
			name: "local with registry",
			cfg:  validConfig(),
		},
		{
			name: "ssh user and key optional",
			cfg: func() Config {
				cfg := validConfig()
				cfg.SSH = SSH{}
				return cfg
			}(),
		},
		{
			name: "ssh strict host key setting optional",
			cfg: func() Config {
				cfg := validConfig()
				value := false
				cfg.SSH.StrictHostKeyChecking = &value
				return cfg
			}(),
		},
		{
			name: "remote with registry",
			cfg: func() Config {
				cfg := validConfig()
				cfg.Builder.Mode = "remote"
				cfg.Builder.Host = "10.0.0.99"
				return cfg
			}(),
		},
		{
			name: "git source with registry",
			cfg: func() Config {
				cfg := validConfig()
				cfg.Builder.Source = "git"
				cfg.Builder.Context = ""
				cfg.Builder.Repo = "git@example.com:org/app.git"
				cfg.Builder.Ref = "v1.2.3"
				cfg.Builder.Subdir = "."
				return cfg
			}(),
		},
		{
			name: "per target without registry",
			cfg: func() Config {
				cfg := validConfig()
				cfg.Builder.Mode = "per_target"
				cfg.Registry = Registry{}
				cfg.Image = "myapp"
				return cfg
			}(),
		},
		{
			name: "local requires registry",
			cfg: func() Config {
				cfg := validConfig()
				cfg.Registry = Registry{}
				return cfg
			}(),
			wantErr: "config.registry is required when config.builder.mode=local",
		},
		{
			name: "remote requires host",
			cfg: func() Config {
				cfg := validConfig()
				cfg.Builder.Mode = "remote"
				return cfg
			}(),
			wantErr: "config.builder.host is required when config.builder.mode=remote",
		},
		{
			name: "per target forbids registry",
			cfg: func() Config {
				cfg := validConfig()
				cfg.Builder.Mode = "per_target"
				return cfg
			}(),
			wantErr: "config.registry must not be set when config.builder.mode=per_target",
		},
		{
			name: "per target forbids host",
			cfg: func() Config {
				cfg := validConfig()
				cfg.Builder.Mode = "per_target"
				cfg.Registry = Registry{}
				cfg.Builder.Host = "10.0.0.99"
				cfg.Image = "myapp"
				return cfg
			}(),
			wantErr: "config.builder.host must not be set when config.builder.mode=per_target",
		},
		{
			name: "local source requires context",
			cfg: func() Config {
				cfg := validConfig()
				cfg.Builder.Context = ""
				return cfg
			}(),
			wantErr: "config.builder.context is required when config.builder.source=local",
		},
		{
			name: "git source requires repo",
			cfg: func() Config {
				cfg := validConfig()
				cfg.Builder.Source = "git"
				cfg.Builder.Context = ""
				cfg.Builder.Ref = "main"
				return cfg
			}(),
			wantErr: "config.builder.repo is required when config.builder.source=git",
		},
		{
			name: "git source requires ref",
			cfg: func() Config {
				cfg := validConfig()
				cfg.Builder.Source = "git"
				cfg.Builder.Context = ""
				cfg.Builder.Repo = "git@example.com:org/app.git"
				return cfg
			}(),
			wantErr: "config.builder.ref is required when config.builder.source=git",
		},
		{
			name: "git source forbids context",
			cfg: func() Config {
				cfg := validConfig()
				cfg.Builder.Source = "git"
				cfg.Builder.Repo = "git@example.com:org/app.git"
				cfg.Builder.Ref = "main"
				return cfg
			}(),
			wantErr: "config.builder.context must not be set when config.builder.source=git",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
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
		Builder: Builder{
			Mode:       "local",
			Source:     "local",
			Context:    ".",
			Dockerfile: "Dockerfile",
		},
		SSH: SSH{
			User: "ubuntu",
			Key:  "~/.ssh/id_ed25519",
		},
	}
}
