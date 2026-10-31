package config

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
)

// Native deploys the app as a systemd service running a release directory on
// each host, instead of as a container. Its presence selects native mode.
// See docs/qifa-native-deployment-design.md.
type Native struct {
	// Build runs on the deployer, in the config's directory, before upload.
	// Optional: the artifact may be produced by CI or a hook instead.
	Build string `yaml:"build"`
	// Artifact is the file or directory to ship, relative to the config.
	Artifact string `yaml:"artifact"`
	// Exec is the argv of the service. Exec[0] is resolved under the release
	// (current/) when relative. Never passed through a shell.
	// Deprecated: use servers.<role>.cmd.
	Exec []string `yaml:"exec,omitempty"`
	// User is the account the service runs as, created by `qifa native setup`.
	User string `yaml:"user"`
	// Arch, when set (amd64, arm64, ...), is checked against the artifact and
	// each host before anything is uploaded.
	Arch string `yaml:"arch"`
	// Port the app listens on. Needed only for the HTTP health check.
	// Deprecated: use app_port on the role (servers.<role>.app_port).
	Port int `yaml:"port,omitempty"`
	// Deprecated: use the top-level healthcheck.
	Healthcheck Healthcheck `yaml:"healthcheck,omitempty"`
	// Dir is the install root on the host. Default /opt/qifa/<service>.
	Dir string `yaml:"dir"`
	// Retain is how many releases to keep on each host.
	// Deprecated: use prune.retain.
	Retain int `yaml:"retain,omitempty"`
	// Restart is the systemd Restart= policy.
	Restart string `yaml:"restart"`
	// SharedDirs are directories under shared/ linked into every release, for
	// data that must outlive a release (uploads, sqlite files, logs).
	SharedDirs []string `yaml:"shared_dirs"`
}

// IsNative reports whether the config deploys native processes.
func (c *Config) IsNative() bool { return c.Native != nil }

const (
	NativeDefaultRetain  = 5
	NativeDefaultRestart = "on-failure"
)

var (
	// Names that end up in unit names, paths, sudoers lines and shell
	// commands. Deliberately narrow: nothing here needs quoting anywhere.
	nativeNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	nativeUserRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]*$`)
	nativeDirRe  = regexp.MustCompile(`^/[A-Za-z0-9_./-]+$`)
)

// NativeUnitName is the systemd unit for a role. A single-role service gets
// the short name; several roles get one unit each.
func (c *Config) NativeUnitName(role string) string {
	if len(c.Servers) == 1 {
		return "qifa-" + c.Service + ".service"
	}
	return "qifa-" + c.Service + "-" + role + ".service"
}

// NativeExec returns the argv for a role: its own exec, or native.exec.
func (c *Config) NativeExec(role string) []string {
	s := c.Servers[role]
	if args, err := s.Cmd.Args(); err == nil && len(args) > 0 {
		return args
	}
	if len(s.Exec) > 0 {
		return s.Exec
	}
	return c.Native.Exec
}

func applyNativeDefaults(cfg *Config) {
	n := cfg.Native
	if n == nil {
		return
	}
	if n.Dir == "" && cfg.Service != "" {
		n.Dir = "/opt/qifa/" + cfg.Service
	}
	n.Dir = strings.TrimRight(n.Dir, "/")
	if n.Restart == "" {
		n.Restart = NativeDefaultRestart
	}
	if cfg.ProxyBoot.Version == "" {
		cfg.ProxyBoot.Version = NativeProxyVersion
	}
}

// DefaultProxyVersion is the kamal-proxy qifa runs by default, as a
// container or (built from source, since kamal-proxy publishes no binaries)
// as a native service. Native needs v0.10+ (KAMAL_PROXY_SOCKET, list --json).
const DefaultProxyVersion = "v0.10.2"

// NativeProxyVersion is the native proxy's default; the same as the container's.
const NativeProxyVersion = DefaultProxyVersion

func (c *Config) validateNative() error {
	n := c.Native
	if !nativeNameRe.MatchString(c.Service) {
		return fmt.Errorf("config.service %q: native mode needs letters, digits, '.', '_' or '-' (it names the unit and install directory)", c.Service)
	}
	// Container settings are not ignored silently: a config that sets them
	// was written for the other mode, and would deploy something different
	// from what it says.
	forbidden := []struct {
		set  bool
		name string
	}{
		{c.Image != "", "image"},
		{c.Source != "", "source"},
		{c.PullPolicy == PullAlways, "pull_policy"},
		{c.Builder != nil, "builder (use native.build)"},
		{c.Registry.Server != "", "registry"},
		{c.ProxyBoot.Image != "" || c.ProxyBoot.Network != "" || c.ProxyBoot.StateVolume != "" || c.ProxyBoot.AppsConfigDir != "", "proxy_boot image, network, state_volume and apps_config_dir (a native proxy is not a container)"},
		{len(c.ProxyBoot.BindIPs) > 0, "proxy_boot.bind_ips (a native proxy listens on all interfaces)"},
		{c.Backup != nil, "backup"},
		{c.Restore != nil, "restore"},
		// Accessories are containers; a native config cannot manage them, so
		// they belong in a separate Docker config for the same host.
		{len(c.Accessories) > 0, "accessories (run them from a separate Docker config)"},
	}
	for _, f := range forbidden {
		if f.set {
			return fmt.Errorf("config.%s is not used with config.native; remove it", f.name)
		}
	}
	if len(c.Servers) == 0 {
		return errors.New("config.servers is required")
	}
	for role, s := range c.Servers {
		if !nativeNameRe.MatchString(role) {
			return fmt.Errorf("config.servers.%s: role names must be letters, digits, '.', '_' or '-'", role)
		}
		if len(s.Hosts) == 0 {
			return fmt.Errorf("config.servers.%s.hosts is required", role)
		}
		switch {
		case s.Port != 0 || len(s.ExtraPorts) > 0:
			return fmt.Errorf("config.servers.%s: port and extra_ports publish container ports; a native app listens on its app_port directly", role)
		case s.AppPort < 0 || s.AppPort > 65535:
			return fmt.Errorf("config.servers.%s.app_port %d is out of range", role, s.AppPort)
		case len(s.Volumes) > 0:
			return fmt.Errorf("config.servers.%s.volumes is for containers; use native.shared_dirs", role)
		case s.Privileged:
			return fmt.Errorf("config.servers.%s.privileged is for containers", role)
		case s.Proxy != nil && *s.Proxy:
			return fmt.Errorf("config.servers.%s.proxy: native mode does not register with kamal-proxy", role)
		}
		field := fmt.Sprintf("config.servers.%s.cmd", role)
		args, err := s.Cmd.Args()
		if err != nil {
			return fmt.Errorf("%s: %w", field, err)
		}
		if len(args) == 0 {
			args = c.NativeExec(role)
		}
		if err := validateExec(field, args, false); err != nil {
			return err
		}
	}
	if strings.TrimSpace(n.Artifact) == "" {
		return errors.New("config.native.artifact is required (the file or directory to ship)")
	}
	if !nativeUserRe.MatchString(n.User) {
		return fmt.Errorf("config.native.user %q: a service account name is required (lowercase, e.g. %q)", n.User, c.Service)
	}
	if n.User == "root" {
		return errors.New("config.native.user must not be root")
	}
	if n.Arch != "" && !knownArch(n.Arch) {
		return fmt.Errorf("config.native.arch %q: use one of amd64, arm64, arm, 386, riscv64", n.Arch)
	}
	if hc := c.Health(); hc.Path != "" {
		hasPort := hc.Port != 0
		for role := range c.Servers {
			hasPort = hasPort || c.AppPort(role) > 0
		}
		if !hasPort {
			return errors.New("config.healthcheck.path needs a port to probe: set app_port on the role (or healthcheck.port)")
		}
		if !strings.HasPrefix(hc.Path, "/") || strings.ContainsAny(hc.Path, " '\"\\`$") {
			return fmt.Errorf("config.healthcheck.path %q must start with / and contain no quotes or spaces", hc.Path)
		}
	}
	if !nativeDirRe.MatchString(n.Dir) || strings.Contains(n.Dir, "..") || n.Dir == "/" {
		return fmt.Errorf("config.native.dir %q must be an absolute path without spaces or '..'", n.Dir)
	}
	if c.Retain() < 2 {
		return errors.New("config.prune.retain must be at least 2 in native mode (the live release and one to roll back to)")
	}
	switch n.Restart {
	case "no", "always", "on-success", "on-failure", "on-abnormal", "on-abort", "on-watchdog":
	default:
		return fmt.Errorf("config.native.restart %q is not a systemd Restart= value", n.Restart)
	}
	for i, d := range n.SharedDirs {
		clean := path.Clean(d)
		if d == "" || clean != d || path.IsAbs(d) || strings.HasPrefix(clean, "..") || !nativeDirRe.MatchString("/"+d) {
			return fmt.Errorf("config.native.shared_dirs[%d] %q must be a plain relative path", i, d)
		}
	}
	if err := c.validateNativeProxy(); err != nil {
		return err
	}
	for i, f := range c.Files {
		if strings.TrimSpace(f.Src) == "" || strings.TrimSpace(f.Dest) == "" {
			return fmt.Errorf("config.files[%d]: src and dest are required", i)
		}
	}
	return nil
}

// NativeProxied reports whether a native app is routed through kamal-proxy:
// its config names the host(s) or path(s) to serve it on.
func (c *Config) NativeProxied() bool {
	return c.Native != nil && (c.Proxy.Host != "" || len(c.Proxy.Hosts) > 0 || len(c.Proxy.PathPrefixes) > 0)
}

// NativeProxyRole is the role kamal-proxy routes to: web, or the only role.
func (c *Config) NativeProxyRole() string {
	if _, ok := c.Servers["web"]; ok {
		return "web"
	}
	if len(c.Servers) == 1 {
		for role := range c.Servers {
			return role
		}
	}
	return ""
}

func (c *Config) validateNativeProxy() error {
	if !c.NativeProxied() {
		if c.Proxy.SSL || c.Proxy.TLS != nil {
			return errors.New("config.proxy.ssl needs config.proxy.host (or hosts) to route a native app")
		}
		return nil
	}
	if c.NativeProxyRole() == "" {
		return errors.New("config.proxy: a native app with several roles is routed to its web role; name one role web")
	}
	if c.AppPort(c.NativeProxyRole()) == 0 {
		return fmt.Errorf("config.proxy needs the %s role's app_port: kamal-proxy forwards to 127.0.0.1:<app_port>", c.NativeProxyRole())
	}
	if c.Health().Path == "" {
		return errors.New("config.proxy needs config.healthcheck.path: kamal-proxy checks it before routing")
	}
	return c.validateProxyTLS()
}

func validateExec(field string, argv []string, optional bool) error {
	if len(argv) == 0 {
		if optional {
			return nil
		}
		return fmt.Errorf("%s is required: the command to run, as a list (e.g. [bin/myapp, --port, \"8080\"])", field)
	}
	for i, a := range argv {
		if strings.ContainsAny(a, "\n\r\x00") {
			return fmt.Errorf("%s[%d] contains a newline or NUL", field, i)
		}
	}
	bin := argv[0]
	if bin == "" {
		return fmt.Errorf("%s[0] is empty", field)
	}
	if !path.IsAbs(bin) {
		rel := strings.TrimPrefix(bin, "./")
		if c := path.Clean(rel); c != rel || c == ".." || strings.HasPrefix(c, "../") {
			return fmt.Errorf("%s[0] %q must be a clean path inside the release", field, bin)
		}
	}
	if strings.ContainsAny(bin, " '\"\\`$%") {
		return fmt.Errorf("%s[0] %q must not contain spaces, quotes or %%", field, bin)
	}
	return nil
}

func knownArch(a string) bool {
	switch a {
	case "amd64", "arm64", "arm", "386", "riscv64":
		return true
	}
	return false
}
