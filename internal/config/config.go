package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
	Restore     *Restore             `yaml:"restore"`
	Files       []FileMapping        `yaml:"files"`
}

// FileMapping pushes a local file to a host path. Used to manage app config
// files (glance.yml, gatus's config.yaml, nginx.conf, etc.) — local file is
// the source of truth, qifa scp's it to the host on `qifa sync`,
// `qifa deploy`, and `qifa restart`. Hand-edits on the host are clobbered
// by the next sync (acceptable for single-operator homelab; not for teams).
type FileMapping struct {
	// Src is a local path, typically relative to the qifa.yaml directory.
	Src string `yaml:"src"`
	// Dest is the absolute path on the target host. The parent directory is
	// auto-created via mkdir -p.
	Dest string `yaml:"dest"`
	// Mode is the file mode (0644 default). Octal literal in YAML: 0o644 or 420.
	Mode int `yaml:"mode"`
}

// Restore is the symmetric inverse of Backup. qifa stages the user-provided
// local file on the target host, then runs Command — either inside the
// container (mode: container) or on the host directly (mode: host). The
// command receives ${ARTIFACT} (the staged file's path).
//
// Intentionally minimal: no auto-stop/start, no snapshot-swap. If your
// restore needs those (like gitea's SQLite restore), wrap qifa restore in a
// shell script that handles the safety steps. Most volume-only apps just
// need an untar and a separate `qifa restart`.
type Restore struct {
	// Mode mirrors Backup.Mode: "container" (default) runs Command via
	// docker exec sh -c in the running container; "host" runs Command
	// directly on the target host via SSH.
	Mode string `yaml:"mode"`
	// Command consumes the staged file. ${ARTIFACT} is expanded to its path
	// (inside the container in container mode, on the host in host mode).
	// ${SERVICE} is also available.
	Command string `yaml:"command"`
	// User and Workdir map to docker exec --user / --workdir. Container mode only.
	User    string `yaml:"user"`
	Workdir string `yaml:"workdir"`
	// Artifact is where qifa stages the uploaded file (inside the container
	// for container mode, on the host for host mode). Defaults to a temp
	// path under /tmp.
	Artifact string `yaml:"artifact"`
}

// Backup describes a per-service snapshot recipe. `qifa backup` runs the
// command, copies the produced artifact back to ./backups/<service>/, and
// applies optional retention. App-specific dump tools (gitea dump, postgres
// pg_dump, mysql dump, restic, plain tar, etc.) plug in via Command — qifa
// doesn't bake in any backend.
type Backup struct {
	// Mode picks where Command runs:
	//   "container" (default) — docker exec sh -c <command> in the running
	//     app container. Requires the container image to ship a shell.
	//     Artifact is a path inside the container; qifa docker-cp's it out.
	//   "host" — run the command directly on the target host via SSH (no
	//     docker exec). Use for distroless apps where the container has no
	//     shell but state lives in a bind-mount on the host. Artifact is a
	//     path on the host.
	Mode string `yaml:"mode"`
	// Command is the snapshot recipe; must produce Artifact.
	Command string `yaml:"command"`
	// User and Workdir map to docker exec --user / --workdir. Container mode only.
	User    string `yaml:"user"`
	Workdir string `yaml:"workdir"`
	// Artifact is the path where Command writes its output (inside the
	// container in container mode, on the host in host mode).
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
	// Privileged runs the container with `--privileged`. Needed for workloads
	// that manage their own container/cgroup namespaces (e.g. Concourse
	// workers using Garden runC, dind, …). Off by default.
	Privileged bool `yaml:"privileged"`
	// ExtraPorts publishes additional host:container ports beyond the
	// AppPort one used by the proxy/non-proxy path. Each entry is a string
	// in docker `-p` form: "hostport:containerport" or
	// "hostport:containerport/proto". Useful when a container exposes
	// more than one port that needs to be reachable from outside the
	// docker network (e.g. Concourse TSA on 2222 while ATC stays on 8080
	// behind kamal-proxy).
	ExtraPorts []string `yaml:"extra_ports"`
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
	SSL             bool          `yaml:"ssl"`
	TLSRedirect     *bool         `yaml:"tls_redirect"`
	TLS             *TLS          `yaml:"tls"`
	ForwardHeaders  *bool         `yaml:"forward_headers"`
	PathPrefixes    []string      `yaml:"path_prefixes"`
	StripPathPrefix *bool         `yaml:"strip_path_prefix"`
}

// TLS controls who issues, renews, and serves the cert when proxy.ssl
// is true. Three sources, picked per-app:
//
//   kamal:  kamal-proxy autocert (HTTP-01). Needs port 80 publicly
//           reachable; kamal-proxy renews silently. Lowest overhead
//           for public hosts.
//   qifa:   qifa-issued via lego. DNS-01 by default (works for
//           tailscale-private hosts and wildcards). Renewed by
//           `qifa cert renew`; affected apps must be redeployed for
//           kamal-proxy to pick up the new cert.
//   static: BYO. User owns acquisition and renewal entirely; qifa
//           just plumbs paths to kamal-proxy.
type TLS struct {
	Source string `yaml:"source"`

	// Staging requests Let's Encrypt's staging environment (avoids LE
	// rate limits while testing). Honored when source: kamal | qifa.
	Staging bool `yaml:"staging"`

	// Challenge selects ACME challenge type when source: qifa. One of
	// "dns-01" (default) or "http-01". Ignored otherwise.
	Challenge string `yaml:"challenge"`

	// Provider is the lego DNS provider name (e.g. "cloudflare",
	// "route53"). Required when source: qifa and challenge: dns-01.
	// Full list: https://go-acme.github.io/lego/dns/
	Provider string `yaml:"provider"`

	// CertPath / KeyPath are the absolute paths inside the kamal-proxy
	// container. Required when source: static. For source: qifa, qifa
	// computes them automatically from the proxy host.
	CertPath string `yaml:"cert_path"`
	KeyPath  string `yaml:"key_path"`
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
		for i, p := range server.ExtraPorts {
			if err := validatePortPublish(p); err != nil {
				return fmt.Errorf("config.servers.%s.extra_ports[%d]: %w", role, i, err)
			}
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
		switch c.Backup.Mode {
		case "", "container", "host":
		default:
			return fmt.Errorf("config.backup.mode must be \"container\" or \"host\", got %q", c.Backup.Mode)
		}
		if strings.TrimSpace(c.Backup.Command) == "" {
			return errors.New("config.backup.command is required when config.backup is set")
		}
		if strings.TrimSpace(c.Backup.Artifact) == "" {
			return errors.New("config.backup.artifact is required when config.backup is set (path where the command writes its output)")
		}
	}
	if c.Restore != nil {
		switch c.Restore.Mode {
		case "", "container", "host":
		default:
			return fmt.Errorf("config.restore.mode must be \"container\" or \"host\", got %q", c.Restore.Mode)
		}
		if strings.TrimSpace(c.Restore.Command) == "" {
			return errors.New("config.restore.command is required when config.restore is set")
		}
	}
	for i, f := range c.Files {
		if strings.TrimSpace(f.Src) == "" {
			return fmt.Errorf("config.files[%d].src is required", i)
		}
		if strings.TrimSpace(f.Dest) == "" {
			return fmt.Errorf("config.files[%d].dest is required", i)
		}
	}
	if err := c.validateProxyTLS(); err != nil {
		return err
	}
	return c.validateBuilder()
}

func (c *Config) validateProxyTLS() error {
	if !c.Proxy.SSL {
		// SSL disabled — TLS block ignored.
		return nil
	}
	if c.Proxy.Host == "" && len(c.Proxy.Hosts) == 0 {
		return errors.New("config.proxy.ssl: true requires config.proxy.host (or .hosts)")
	}
	if c.Proxy.TLS == nil {
		return errors.New("config.proxy.ssl: true requires config.proxy.tls.source (one of: kamal, qifa, static)")
	}
	switch c.Proxy.TLS.Source {
	case "kamal":
		// Anything goes; kamal-proxy autocert handles it.
	case "qifa":
		switch c.Proxy.TLS.Challenge {
		case "", "dns-01":
			if c.Proxy.TLS.Provider == "" {
				return errors.New("config.proxy.tls.provider is required when source: qifa and challenge: dns-01")
			}
		case "http-01":
			// no provider needed
		default:
			return fmt.Errorf("config.proxy.tls.challenge must be \"dns-01\" or \"http-01\", got %q", c.Proxy.TLS.Challenge)
		}
	case "static":
		if c.Proxy.TLS.CertPath == "" || c.Proxy.TLS.KeyPath == "" {
			return errors.New("config.proxy.tls.cert_path and config.proxy.tls.key_path are both required when source: static")
		}
	case "":
		return errors.New("config.proxy.tls.source is required when proxy.ssl: true (one of: kamal, qifa, static)")
	default:
		return fmt.Errorf("config.proxy.tls.source must be \"kamal\", \"qifa\", or \"static\", got %q", c.Proxy.TLS.Source)
	}
	return nil
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

// validatePortPublish accepts a docker `-p` value of the form
// "hostport:containerport" or "hostport:containerport/proto". The full
// docker `-p` grammar also supports an optional "host:port:port" prefix,
// but qifa keeps the surface narrow for now — apps that need bind-IP
// control can use proxy.bind_ips at the proxy layer.
func validatePortPublish(s string) error {
	core := s
	if i := strings.Index(core, "/"); i >= 0 {
		proto := core[i+1:]
		switch proto {
		case "tcp", "udp", "sctp":
		default:
			return fmt.Errorf("invalid protocol %q (want tcp|udp|sctp)", proto)
		}
		core = core[:i]
	}
	parts := strings.Split(core, ":")
	if len(parts) != 2 {
		return fmt.Errorf("want \"hostport:containerport[/proto]\", got %q", s)
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n <= 0 || n > 65535 {
			label := "hostport"
			if i == 1 {
				label = "containerport"
			}
			return fmt.Errorf("%s %q is not a valid port", label, p)
		}
	}
	return nil
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
