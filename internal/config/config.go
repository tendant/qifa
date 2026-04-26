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
	ProxyBoot   ProxyBoot            `yaml:"proxy_boot"`
	Backup      *Backup              `yaml:"backup"`
}

// Backup describes a per-service snapshot recipe. `qifa backup` runs the
// command inside the running app container, copies the produced artifact
// back to ./backups/<service>/, and applies optional retention. App-specific
// dump tools (gitea dump, postgres pg_dump, mysql dump, restic, plain tar,
// etc.) plug in via Command — qifa doesn't bake in any backend.
type Backup struct {
	// Command is run inside the container via `docker exec sh -c`. It must
	// produce the artifact at the path given by Artifact.
	Command string `yaml:"command"`
	// User and Workdir map to docker exec --user / --workdir. Optional.
	User    string `yaml:"user"`
	Workdir string `yaml:"workdir"`
	// Artifact is the path inside the container where Command writes its
	// output. qifa docker-cp's it out, scp's it back, then removes both
	// the in-container and host-side temp copies.
	Artifact string `yaml:"artifact"`
	// ArtifactName templates the local filename. Supports ${SERVICE},
	// ${VERSION}, ${STAMP}. Defaults to "${SERVICE}-${VERSION}-${STAMP}<ext>"
	// where <ext> is taken from the Artifact path.
	ArtifactName string `yaml:"artifact_name"`
	// Retain is how many recent local backups to keep. 0 (default) = unlimited.
	// Older snapshots beyond this count are deleted after a successful backup.
	Retain int `yaml:"retain"`
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
	// Volumes are docker -v mounts in the form host_path:container_path[:options].
	// Host paths are created with mkdir -p before docker run.
	Volumes []string `yaml:"volumes"`
}

// Proxy is the per-app routing config registered with kamal-proxy at deploy
// time. It does NOT control how the proxy container itself runs — that's
// owned by ProxyBoot and the `qifa proxy` verbs.
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

// ProxyBoot describes how to boot the shared kamal-proxy container itself.
// One proxy per host, shared across every app on that host. Owned by the
// operator via `qifa proxy boot` / `qifa proxy upgrade`; app deploys never
// touch it.
type ProxyBoot struct {
	Hosts         []string `yaml:"hosts"` // hosts to boot the proxy on
	Image         string   `yaml:"image"`
	Version       string   `yaml:"version"`
	Network       string   `yaml:"network"`
	StateVolume   string   `yaml:"state_volume"`
	AppsConfigDir string   `yaml:"apps_config_dir"`
	HTTPPort      int      `yaml:"http_port"`
	HTTPSPort     int      `yaml:"https_port"`
	// BindIPs restricts which host IPs the proxy listens on. Empty (default)
	// = listen on all interfaces (0.0.0.0). Use to separate public and
	// internal NICs, or to bind to specific IPs on a multi-IP host.
	// Each entry produces one set of -p <ip>:<port>:<port> flags.
	BindIPs []string `yaml:"bind_ips"`
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
	// SecretCommand is a shell command run on the deployer at deploy time.
	// Its stdout is parsed as KEY=VALUE lines (dotenv format) and merged
	// into the env file passed to containers. Use this to integrate with
	// SOPS, Vault, 1Password, AWS Secrets Manager, etc. without qifa
	// depending on any of them.
	SecretCommand string `yaml:"secret_command"`
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
	Image   string            `yaml:"image"`
	Host    string            `yaml:"host"`
	Volumes []string          `yaml:"volumes"`
	Env     map[string]string `yaml:"env"`
	Port    int               `yaml:"port"`     // host port to publish
	AppPort int               `yaml:"app_port"` // container port to publish
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
	if c.ProxyBoot.HTTPPort < 0 {
		return errors.New("config.proxy_boot.http_port must not be negative")
	}
	if c.ProxyBoot.HTTPSPort < 0 {
		return errors.New("config.proxy_boot.https_port must not be negative")
	}
	if c.Registry.Server != "" && (c.Registry.Username == "" || c.Registry.PasswordEnv == "") {
		return errors.New("config.registry.username and config.registry.password_env are required when config.registry.server is set")
	}
	if c.Backup != nil {
		if strings.TrimSpace(c.Backup.Command) == "" {
			return errors.New("config.backup.command is required when config.backup is set")
		}
		if strings.TrimSpace(c.Backup.Artifact) == "" {
			return errors.New("config.backup.artifact is required when config.backup is set (path inside the container where the command writes its output)")
		}
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
	if cfg.ProxyBoot.HTTPPort == 0 {
		cfg.ProxyBoot.HTTPPort = 80
	}
	if cfg.ProxyBoot.HTTPSPort == 0 {
		cfg.ProxyBoot.HTTPSPort = 443
	}
	if cfg.ProxyBoot.Image == "" {
		cfg.ProxyBoot.Image = "basecamp/kamal-proxy"
	}
	if cfg.ProxyBoot.Version == "" {
		cfg.ProxyBoot.Version = "v0.9.2"
	}
	if cfg.ProxyBoot.Network == "" {
		cfg.ProxyBoot.Network = "kamal"
	}
	if cfg.ProxyBoot.StateVolume == "" {
		cfg.ProxyBoot.StateVolume = "kamal-proxy-config"
	}
	if cfg.ProxyBoot.AppsConfigDir == "" {
		cfg.ProxyBoot.AppsConfigDir = ".kamal/proxy/apps-config"
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

# proxy_boot describes how to launch the shared kamal-proxy container itself.
# Owned by the operator via "qifa proxy boot"; app deploys do not touch it.
# A single proxy is shared by every app on each listed host.
proxy_boot:
  hosts:
    - 10.0.0.11
    - 10.0.0.12
  # http_port: 80
  # https_port: 443

# proxy describes how THIS app's routes are registered with the running proxy.
# Set on every "qifa deploy".
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
	if strings.Contains(c.Builder.Platform, ",") {
		if c.Builder.IsPerTarget() {
			return errors.New("config.builder.platform must be a single platform when config.builder.host=per_target")
		}
		if !c.Registry.Enabled() {
			return errors.New("config.builder.platform with multiple platforms requires a registry (buildx --push targets the registry directly)")
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
