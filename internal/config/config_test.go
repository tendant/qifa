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
	path := filepath.Join(dir, "qifa.yaml")
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
	if cfg.Retain() != 5 {
		t.Fatalf("unexpected retain default: %d", cfg.Retain())
	}
	if cfg.Health().Interval != 2*time.Second {
		t.Fatalf("unexpected healthcheck interval default: %v", cfg.Health().Interval)
	}
	if cfg.SSH.StrictHostKeyChecking != nil {
		t.Fatalf("expected nil strict host key setting by default, got %#v", cfg.SSH.StrictHostKeyChecking)
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	cases := map[string]string{
		"top level": "service: myapp\nimage: myapp\nservers:\n  web:\n    hosts: [10.0.0.11]\nregistery:\n  server: r.example.com\n",
		"nested":    "service: myapp\nimage: myapp\nservers:\n  web:\n    hosts: [10.0.0.11]\n    app_prot: 3000\n",
		"deep":      "service: myapp\nimage: myapp\nservers:\n  web:\n    hosts: [10.0.0.11]\nproxy:\n  host: app.example.com\n  healthcheck:\n    pth: /up\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "qifa.yaml")
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), "not found in type") {
				t.Fatalf("expected unknown-field error, got %v", err)
			}
		})
	}
}

func TestLoadEmptyFileFailsValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "qifa.yaml")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "config.service is required") {
		t.Fatalf("expected validation error for empty file, got %v", err)
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
			path := filepath.Join(t.TempDir(), "qifa.yaml")
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

func TestValidateSource(t *testing.T) {
	// externalLocal returns a valid config for an external image sourced
	// locally: no builder, no registry, tagged image.
	externalLocal := func() Config {
		c := validConfig()
		c.Builder = nil
		c.Registry = Registry{}
		c.Image = "app:1.2.3"
		c.Source = SourceLocal
		return c
	}
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{
			name:   "source local, external image, no registry",
			mutate: func(c *Config) { *c = externalLocal() },
		},
		{
			name: "source registry explicit with registry",
			mutate: func(c *Config) {
				c.Builder = nil
				c.Image = "registry.example.com/app:1"
				c.Source = SourceRegistry
			},
		},
		{
			name: "empty source defaults to registry",
			mutate: func(c *Config) {
				c.Builder = nil
				c.Image = "registry.example.com/app:1"
				c.Source = ""
			},
		},
		{
			name: "source local forbids registry",
			mutate: func(c *Config) {
				c.Builder = nil
				c.Image = "app:1"
				c.Source = SourceLocal // registry left set by validConfig
			},
			wantErr: "must not set config.registry",
		},
		{
			name: "source with builder is rejected",
			mutate: func(c *Config) {
				c.Source = SourceRegistry // builder still set
			},
			wantErr: "applies only to external images",
		},
		{
			name: "invalid source value",
			mutate: func(c *Config) {
				c.Builder = nil
				c.Registry = Registry{}
				c.Image = "app:1"
				c.Source = "always"
			},
			wantErr: "is not valid",
		},
		{
			name: "accessory source local",
			mutate: func(c *Config) {
				*c = externalLocal()
				c.Accessories = map[string]Accessory{
					"db": {Image: "postgres:16", Host: "10.0.0.11", Source: SourceLocal},
				}
			},
		},
		{
			name: "accessory invalid source",
			mutate: func(c *Config) {
				*c = externalLocal()
				c.Accessories = map[string]Accessory{
					"db": {Image: "postgres:16", Host: "10.0.0.11", Source: "never"},
				}
			},
			wantErr: "accessories.db.source",
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

func TestImageSourceDefaults(t *testing.T) {
	c := Config{}
	if c.ImageSource() != SourceRegistry {
		t.Fatalf("empty ImageSource = %q, want %q", c.ImageSource(), SourceRegistry)
	}
	if c.IsLocalSource() {
		t.Fatal("empty config should not be local source")
	}
	c.Source = SourceLocal
	if !c.IsLocalSource() {
		t.Fatal("source: local with no builder should be local source")
	}
	c.Builder = &Builder{}
	if c.IsLocalSource() {
		t.Fatal("source: local is inert when a builder is set")
	}

	a := Accessory{}
	if a.ImageSource() != SourceRegistry || a.IsLocalSource() {
		t.Fatal("empty accessory should default to registry source")
	}
	a.Source = SourceLocal
	if !a.IsLocalSource() {
		t.Fatal("accessory source: local should be local source")
	}
}

func TestWriteSampleCreatesParents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "qifa.yaml")
	if err := WriteSample(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

// TestWriteSampleParses guards against schema drift: whatever `qifa init`
// writes must Load cleanly, so new users don't hit a validation error on
// day one.
func TestWriteSampleParses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "qifa.yaml")
	if err := WriteSample(path); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("sample config from `qifa init` does not parse: %v", err)
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

func TestValidatePortPublishSinglePorts(t *testing.T) {
	cases := []struct {
		in      string
		wantErr bool
	}{
		{"8080:8080", false},
		{"8080:8080/tcp", false},
		{"8080:8080/udp", false},
		{"8080:8080/sctp", false},
		{"8080:8080/quic", true},
		{"0:8080", true},
		{"8080:70000", true},
		{"8080", true},
		{"8080:8080:8080", true},
	}
	for _, c := range cases {
		err := validatePortPublish(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("validatePortPublish(%q) error = %v, wantErr %v", c.in, err, c.wantErr)
		}
	}
}

func TestValidatePortPublishBindIP(t *testing.T) {
	cases := []struct {
		in      string
		wantErr bool
	}{
		{"127.0.0.1:64602:6002", false},  // loopback only — off the public internet
		{"0.0.0.0:8080:80", false},       // explicit all-interfaces
		{"10.0.0.11:8080:80/udp", false}, // with a protocol
		{"[::1]:8080:80", false},         // IPv6 must be bracketed
		{"[::]:8080:80/tcp", false},
		{"127.0.0.1:50000-50100:50000-50100", false}, // ranges still apply
		{"not-an-ip:8080:80", true},
		{":8080:80", true},             // empty IP
		{"::1:8080:80", true},          // unbracketed IPv6, as docker rejects
		{"[::1:8080:80", true},         // unclosed bracket
		{"[::1]8080:80", true},         // missing colon after the bracket
		{"127.0.0.1:8080:80:90", true}, // one field too many
		{"127.0.0.1:0:80", true},       // invalid port with a valid IP
	}
	for _, c := range cases {
		err := validatePortPublish(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("validatePortPublish(%q) error = %v, wantErr %v", c.in, err, c.wantErr)
		}
	}
}

func TestValidatePortPublishRanges(t *testing.T) {
	cases := []struct {
		in      string
		wantErr bool
	}{
		{"50000-50100:50000-50100/udp", false},
		{"50000-50000:50000-50000/udp", false}, // single-port "range" is fine
		{"50000-50100:60000-60100/udp", false}, // different numbers, same width
		{"50000-50100:50000-50050/udp", true},  // width mismatch
		{"50100-50000:50100-50000/udp", true},  // end before start
		{"50000-50100:8080/udp", true},         // range on one side only
		{"8080:50000-50100/udp", true},
	}
	for _, c := range cases {
		err := validatePortPublish(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("validatePortPublish(%q) error = %v, wantErr %v", c.in, err, c.wantErr)
		}
	}
}

func TestValidateGPUs(t *testing.T) {
	for _, ok := range []string{"", "all", "1", "2", "device=0", "device=0,1", "device=GPU-3a23c669-1f69-c64e-cf85-44e9b07e7a2a", "device=MIG-GPU-3a23c669/1/0"} {
		if err := ValidateGPUs(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"0", "-1", "yes", "ALL", "device=", "device=0,", "device=0;rm -rf /", "device= 0", "count=1", "1 "} {
		if err := ValidateGPUs(bad); err == nil {
			t.Errorf("%q: accepted", bad)
		}
	}
}

// loadYAML loads a config from text, as Load does from a file.
func loadYAML(t *testing.T, text string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "qifa.yaml")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

// A bad gpus value fails the config at load, naming the field, for a role
// and for an accessory; a native role rejects gpus outright.
func TestGPUsValidatedOnLoad(t *testing.T) {
	base := `service: app
image: app:1.0
servers:
  web:
    hosts: [h1]
`
	cfg, err := loadYAML(t, base+"    gpus: \"device=0,1\"\naccessories:\n  llm:\n    image: vllm/vllm-openai:v0.8.0\n    host: h1\n    gpus: all\n")
	if err != nil {
		t.Fatalf("valid gpus rejected: %v", err)
	}
	if cfg.Servers["web"].GPUs != "device=0,1" || cfg.Accessories["llm"].GPUs != "all" {
		t.Fatalf("gpus not parsed: %+v %+v", cfg.Servers["web"], cfg.Accessories["llm"])
	}
	if _, err := loadYAML(t, base+"    gpus: yes\n"); err == nil || !strings.Contains(err.Error(), "servers.web.gpus") {
		t.Errorf("bad role gpus: %v", err)
	}
	if _, err := loadYAML(t, base+"accessories:\n  llm:\n    image: vllm/vllm-openai:v0.8.0\n    host: h1\n    gpus: \"device=\"\n"); err == nil || !strings.Contains(err.Error(), "accessories.llm.gpus") {
		t.Errorf("bad accessory gpus: %v", err)
	}
}

// networks names existing user-defined networks; built-ins, the estate
// network, repeats and bad names fail at load, naming the field.
func TestNetworksValidatedOnLoad(t *testing.T) {
	base := `service: app
image: app:1.0
servers:
  web:
    hosts: [h1]
`
	cfg, err := loadYAML(t, base+"    networks: [shop_default]\naccessories:\n  db:\n    image: postgres:16\n    host: h1\n    networks: [shop_backend, shop_default]\n")
	if err != nil {
		t.Fatalf("valid networks rejected: %v", err)
	}
	if got := cfg.Servers["web"].Networks; len(got) != 1 || got[0] != "shop_default" {
		t.Fatalf("role networks not parsed: %v", got)
	}
	if got := cfg.Accessories["db"].Networks; len(got) != 2 {
		t.Fatalf("accessory networks not parsed: %v", got)
	}
	for value, want := range map[string]string{
		"[bridge]":     "built-in",
		"[host]":       "built-in",
		"[kamal]":      "estate network",
		"[a, a]":       "twice",
		"[\"my net\"]": "not a docker network name",
	} {
		if _, err := loadYAML(t, base+"    networks: "+value+"\n"); err == nil || !strings.Contains(err.Error(), "servers.web.networks") || !strings.Contains(err.Error(), want) {
			t.Errorf("networks %s: got %v, want an error containing %q", value, err, want)
		}
	}
	if _, err := loadYAML(t, base+"accessories:\n  db:\n    image: postgres:16\n    host: h1\n    networks: [none]\n"); err == nil || !strings.Contains(err.Error(), "accessories.db.networks") {
		t.Errorf("bad accessory networks: %v", err)
	}
}
