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
	Builder     Builder              `yaml:"builder"`
	SSH         SSH                  `yaml:"ssh"`
	Hooks       Hooks                `yaml:"hooks"`
	Accessories map[string]Accessory `yaml:"accessories"`
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

type Builder struct {
	Mode       string `yaml:"mode"`
	Host       string `yaml:"host"`
	Source     string `yaml:"source"`
	Context    string `yaml:"context"`
	Repo       string `yaml:"repo"`
	Ref        string `yaml:"ref"`
	Subdir     string `yaml:"subdir"`
	Dockerfile string `yaml:"dockerfile"`
	Platform   string `yaml:"platform"`
}

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
	if err := c.validateBuilder(); err != nil {
		return err
	}
	return nil
}

func applyDefaults(cfg *Config) {
	if cfg.Builder.Mode == "" {
		cfg.Builder.Mode = "local"
	}
	if cfg.Builder.Source == "" {
		cfg.Builder.Source = "local"
	}
	if cfg.Builder.Source == "local" && cfg.Builder.Context == "" {
		cfg.Builder.Context = "."
	}
	if cfg.Builder.Source == "git" && cfg.Builder.Subdir == "" {
		cfg.Builder.Subdir = "."
	}
	if cfg.Builder.Dockerfile == "" {
		cfg.Builder.Dockerfile = "Dockerfile"
	}
	if cfg.Proxy.Healthcheck.Path == "" {
		cfg.Proxy.Healthcheck.Path = "/up"
	}
	if cfg.Proxy.Healthcheck.Interval == 0 {
		cfg.Proxy.Healthcheck.Interval = 2 * time.Second
	}
	if cfg.Proxy.Healthcheck.Timeout == 0 {
		cfg.Proxy.Healthcheck.Timeout = 5 * time.Second
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
  deploy_timeout: 30s
  drain_timeout: 30s
  target_timeout: 30s
  tls: true
  tls_redirect: true
  path_prefixes:
    - /
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
    LOG_LEVEL: info
  secret:
    - DATABASE_URL
    - REDIS_URL

builder:
  mode: local
  source: local
  context: .
  dockerfile: Dockerfile
  platform: linux/amd64

ssh:
  user: ubuntu
  key: ~/.ssh/id_ed25519

hooks:
  pre_build: ./scripts/pre_build.sh
  post_deploy: ./scripts/post_deploy.sh
  pre_rollback: ./scripts/pre_rollback.sh

accessories:
  redis:
    image: redis:7
    host: 10.0.0.13
`

func (c *Config) validateBuilder() error {
	switch c.Builder.Mode {
	case "local":
		if c.Builder.Host != "" {
			return errors.New("config.builder.host must not be set when config.builder.mode=local")
		}
		if !c.Registry.Enabled() {
			return errors.New("config.registry is required when config.builder.mode=local")
		}
	case "remote":
		if c.Builder.Host == "" {
			return errors.New("config.builder.host is required when config.builder.mode=remote")
		}
		if !c.Registry.Enabled() {
			return errors.New("config.registry is required when config.builder.mode=remote")
		}
	case "per_target":
		if c.Builder.Host != "" {
			return errors.New("config.builder.host must not be set when config.builder.mode=per_target")
		}
		if c.Registry.Enabled() {
			return errors.New("config.registry must not be set when config.builder.mode=per_target")
		}
	default:
		return fmt.Errorf("config.builder.mode must be one of local, remote, per_target, got %q", c.Builder.Mode)
	}
	if c.Registry.Server != "" && (c.Registry.Username == "" || c.Registry.PasswordEnv == "") {
		return errors.New("config.registry.username and config.registry.password_env are required when config.registry.server is set")
	}
	if err := c.validateBuilderSource(); err != nil {
		return err
	}
	return nil
}

func (r Registry) Enabled() bool {
	return strings.TrimSpace(r.Server) != ""
}

func (c *Config) validateBuilderSource() error {
	switch c.Builder.Source {
	case "local":
		if strings.TrimSpace(c.Builder.Context) == "" {
			return errors.New("config.builder.context is required when config.builder.source=local")
		}
		if c.Builder.Repo != "" || c.Builder.Ref != "" || c.Builder.Subdir != "" {
			return errors.New("config.builder.repo, config.builder.ref, and config.builder.subdir must not be set when config.builder.source=local")
		}
	case "git":
		if strings.TrimSpace(c.Builder.Repo) == "" {
			return errors.New("config.builder.repo is required when config.builder.source=git")
		}
		if strings.TrimSpace(c.Builder.Ref) == "" {
			return errors.New("config.builder.ref is required when config.builder.source=git")
		}
		if c.Builder.Context != "" {
			return errors.New("config.builder.context must not be set when config.builder.source=git")
		}
	default:
		return fmt.Errorf("config.builder.source must be one of local, git, got %q", c.Builder.Source)
	}
	return nil
}
