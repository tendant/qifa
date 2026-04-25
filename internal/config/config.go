package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Service     string               `yaml:"service"`
	Image       string               `yaml:"image"`
	Servers     map[string]Server    `yaml:"servers"`
	Proxy       Proxy                `yaml:"proxy"`
	Registry    Registry             `yaml:"registry"`
	Env         Env                  `yaml:"env"`
	Builder     *Builder             `yaml:"builder"`
	SSH         SSH                  `yaml:"ssh"`
	Hooks       Hooks                `yaml:"hooks"`
	Accessories map[string]Accessory `yaml:"accessories"`
	Prune       Prune                `yaml:"prune"`
	Rollout     Rollout              `yaml:"rollout"`
}

type Prune struct {
	RetainContainers int `yaml:"retain_containers"`
}

type Rollout struct {
	// BatchSize controls how many hosts in a role deploy in parallel per batch.
	// Defaults to 1 (strict rolling) when omitted. Set to 0 to deploy all
	// hosts in one batch (fully parallel).
	BatchSize *int          `yaml:"batch_size"`
	BatchWait time.Duration `yaml:"batch_wait"`
}

type Server struct {
	Hosts   []string `yaml:"hosts"`
	Port    int      `yaml:"port"`
	AppPort int      `yaml:"app_port"`
	Cmd     string   `yaml:"cmd"`
	Proxy   *bool    `yaml:"proxy"`
}

type Proxy struct {
	Host            string        `yaml:"host"`
	Hosts           []string      `yaml:"hosts"`
	AppPort         int           `yaml:"app_port"`
	HTTPPort        int           `yaml:"http_port"`
	HTTPSPort       int           `yaml:"https_port"`
	Image           string        `yaml:"image"`
	Version         string        `yaml:"version"`
	Network         string        `yaml:"network"`
	StateVolume     string        `yaml:"state_volume"`
	AppsConfigDir   string        `yaml:"apps_config_dir"`
	Healthcheck     Healthcheck   `yaml:"healthcheck"`
	DeployTimeout   time.Duration `yaml:"deploy_timeout"`
	DrainTimeout    time.Duration `yaml:"drain_timeout"`
	TargetTimeout   time.Duration `yaml:"target_timeout"`
	TLS             bool          `yaml:"tls"`
	TLSRedirect     *bool         `yaml:"tls_redirect"`
	TLSStaging      bool          `yaml:"tls_staging"`
	ForwardHeaders  *bool         `yaml:"forward_headers"`
	PathPrefixes    []string      `yaml:"path_prefixes"`
	StripPathPrefix *bool         `yaml:"strip_path_prefix"`
}

type Healthcheck struct {
	Path     string        `yaml:"path"`
	Interval time.Duration `yaml:"interval"`
	Timeout  time.Duration `yaml:"timeout"`
}

type Registry struct {
	Server      string `yaml:"server"`
	Username    string `yaml:"username"`
	PasswordEnv string `yaml:"password_env"`
}

type Env struct {
	Clear  map[string]string `yaml:"clear"`
	Secret []string          `yaml:"secret"`
}

// Builder describes how to produce the image. When nil, the image is treated
// as externally produced and qifa just pulls it.
//
// Host:
//   ""           build locally (where qifa runs); requires registry.
//   "per_target" build on each deployment target; forbids registry.
//   anything else  treated as an SSH-reachable host to build on; requires registry.
//
// Source is inferred from the presence of Repo:
//   Repo set     git source; requires Ref; forbids Context.
//   Repo unset   local source; requires Context (defaults to ".").
type Builder struct {
	Host       string `yaml:"host"`
	Context    string `yaml:"context"`
	Repo       string `yaml:"repo"`
	Ref        string `yaml:"ref"`
	Subdir     string `yaml:"subdir"`
	Dockerfile string `yaml:"dockerfile"`
	Platform   string `yaml:"platform"`
}

const BuilderHostPerTarget = "per_target"

func (b *Builder) IsPerTarget() bool { return b != nil && b.Host == BuilderHostPerTarget }
func (b *Builder) IsRemote() bool {
	return b != nil && b.Host != "" && b.Host != BuilderHostPerTarget
}
func (b *Builder) IsLocal() bool { return b != nil && b.Host == "" }
func (b *Builder) IsGit() bool   { return b != nil && b.Repo != "" }

type SSH struct {
	User                  string `yaml:"user"`
	Key                   string `yaml:"key"`
	StrictHostKeyChecking *bool  `yaml:"strict_host_key_checking"`
}

type Hooks struct {
	PreBuild    string `yaml:"pre_build"`
	PostDeploy  string `yaml:"post_deploy"`
	PreRollback string `yaml:"pre_rollback"`
}

type Accessory struct {
	Image string `yaml:"image"`
	Host  string `yaml:"host"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	applyDefaults(&cfg)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) Validate() error {
	if c.Service == "" {
		return errors.New("config.service is required")
	}
	if c.Image == "" {
		return errors.New("config.image is required")
	}
	if len(c.Servers) == 0 {
		return errors.New("config.servers is required")
	}
	for role, server := range c.Servers {
		if len(server.Hosts) == 0 {
			return fmt.Errorf("config.servers.%s.hosts is required", role)
		}
	}
	if c.Proxy.HTTPPort < 0 {
		return errors.New("config.proxy.http_port must not be negative")
	}
	if c.Proxy.HTTPSPort < 0 {
		return errors.New("config.proxy.https_port must not be negative")
	}
	if c.Registry.Server != "" && (c.Registry.Username == "" || c.Registry.PasswordEnv == "") {
		return errors.New("config.registry.username and config.registry.password_env are required when config.registry.server is set")
	}
	return c.validateBuilder()
}

func applyDefaults(cfg *Config) {
	if cfg.Builder != nil {
		if cfg.Builder.Dockerfile == "" {
			cfg.Builder.Dockerfile = "Dockerfile"
		}
		if cfg.Builder.IsGit() {
			if cfg.Builder.Subdir == "" {
				cfg.Builder.Subdir = "."
			}
		} else if cfg.Builder.Context == "" {
			cfg.Builder.Context = "."
		}
	}
	if cfg.Proxy.Healthcheck.Interval == 0 {
		cfg.Proxy.Healthcheck.Interval = 2 * time.Second
	}
	if cfg.Proxy.Healthcheck.Timeout == 0 {
		cfg.Proxy.Healthcheck.Timeout = 5 * time.Second
	}
	if cfg.Proxy.HTTPPort == 0 {
		cfg.Proxy.HTTPPort = 80
	}
	if cfg.Proxy.HTTPSPort == 0 {
		cfg.Proxy.HTTPSPort = 443
	}
	if cfg.Proxy.Image == "" {
		cfg.Proxy.Image = "basecamp/kamal-proxy"
	}
	if cfg.Proxy.Version == "" {
		cfg.Proxy.Version = "v0.9.2"
	}
	if cfg.Proxy.Network == "" {
		cfg.Proxy.Network = "kamal"
	}
	if cfg.Proxy.StateVolume == "" {
		cfg.Proxy.StateVolume = "kamal-proxy-config"
	}
	if cfg.Proxy.AppsConfigDir == "" {
		cfg.Proxy.AppsConfigDir = ".kamal/proxy/apps-config"
	}
	if cfg.Prune.RetainContainers == 0 {
		cfg.Prune.RetainContainers = 5
	}
	if cfg.Rollout.BatchSize == nil {
		one := 1
		cfg.Rollout.BatchSize = &one
	}
	if cfg.Proxy.DeployTimeout == 0 {
		cfg.Proxy.DeployTimeout = 30 * time.Second
	}
	if cfg.Proxy.DrainTimeout == 0 {
		cfg.Proxy.DrainTimeout = 30 * time.Second
	}
	if cfg.Proxy.TargetTimeout == 0 {
		cfg.Proxy.TargetTimeout = 30 * time.Second
	}
}

// Marshal returns the config rendered as YAML, including all applied defaults.
// Useful for `qifa config` to show what qifa actually sees after loading.
func (c *Config) Marshal() ([]byte, error) {
	return yaml.Marshal(c)
}

func WriteSample(path string) error {
	dir := filepath.Dir(path)
	if dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, []byte(sampleConfig), 0o644)
}

const sampleConfig = `service: myapp
image: registry.example.com/myapp

servers:
  web:
    hosts:
      - 10.0.0.11
      - 10.0.0.12
    port: 3000
  worker:
    hosts:
      - 10.0.0.13
    cmd: ./worker

proxy:
  host: app.example.com
  app_port: 3000
  healthcheck:
    path: /up
    interval: 2s
    timeout: 5s

registry:
  server: registry.example.com
  username: reg
  password_env: REGISTRY_PASSWORD

env:
  clear:
    APP_ENV: production
  secret:
    - DATABASE_URL

# Omit the builder block to deploy an externally built image (image must
# include a :tag or @digest). Set host: per_target to build on each target,
# or host: <ip> to build on a remote host.
builder:
  context: .
  dockerfile: Dockerfile
  platform: linux/amd64

ssh:
  user: ubuntu
  key: ~/.ssh/id_ed25519
`

func (c *Config) validateBuilder() error {
	if c.Builder == nil {
		if _, err := ParseImageVersion(c.Image); err != nil {
			return fmt.Errorf("config.image: %w (set config.builder to build the image, or pin a tag like image:1.2.3)", err)
		}
		return nil
	}
	switch {
	case c.Builder.IsPerTarget():
		if c.Registry.Enabled() {
			return errors.New("config.registry must not be set when config.builder.host=per_target")
		}
	case c.Builder.IsLocal(), c.Builder.IsRemote():
		if !c.Registry.Enabled() {
			return errors.New("config.registry is required when building locally or remotely (omit config.builder to deploy an external image)")
		}
	}
	if c.Builder.IsGit() {
		if strings.TrimSpace(c.Builder.Ref) == "" {
			return errors.New("config.builder.ref is required when config.builder.repo is set")
		}
		if c.Builder.Context != "" {
			return errors.New("config.builder.context must not be set when config.builder.repo is set")
		}
	} else {
		if strings.TrimSpace(c.Builder.Context) == "" {
			return errors.New("config.builder.context is required when config.builder.repo is not set")
		}
		if c.Builder.Ref != "" || c.Builder.Subdir != "" {
			return errors.New("config.builder.ref and config.builder.subdir must not be set when config.builder.repo is not set")
		}
	}
	return nil
}

func (r Registry) Enabled() bool {
	return strings.TrimSpace(r.Server) != ""
}

// ParseImageVersion extracts the tag or digest from an image reference.
// Returns an error only if the image has no tag and no digest. The actual
// version label used at deploy time is the registry digest (resolved at
// deploy), not the tag — so :latest is accepted here.
func ParseImageVersion(image string) (string, error) {
	image = strings.TrimSpace(image)
	if image == "" {
		return "", errors.New("image is empty")
	}
	if at := strings.LastIndex(image, "@"); at > 0 {
		digest := image[at+1:]
		if digest == "" {
			return "", errors.New("image digest is empty")
		}
		return digest, nil
	}
	colon := strings.LastIndex(image, ":")
	if colon < 0 || strings.Contains(image[colon:], "/") {
		return "", errors.New("image must include a :tag or @digest")
	}
	tag := image[colon+1:]
	if tag == "" {
		return "", errors.New("image tag is empty")
	}
	return tag, nil
}

// ImageRepo returns the registry/repository portion of an image reference,
// stripping any :tag or @digest suffix.
func ImageRepo(image string) string {
	if at := strings.LastIndex(image, "@"); at > 0 {
		return image[:at]
	}
	colon := strings.LastIndex(image, ":")
	if colon < 0 || strings.Contains(image[colon:], "/") {
		return image
	}
	return image[:colon]
}
