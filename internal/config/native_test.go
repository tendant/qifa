package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const nativeBase = `service: app
servers:
  web:
    hosts: [10.0.0.1]
    cmd: [app, --port, "8080"]
    app_port: 8080
ssh:
  user: deploy
healthcheck:
  path: /up
native:
  build: make build
  artifact: dist/app
  user: app
`

func loadString(t *testing.T, yaml string) (*Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "qifa.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

func TestNativeConfigLoadsWithDefaults(t *testing.T) {
	cfg, err := loadString(t, nativeBase)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.IsNative() {
		t.Fatal("native block not detected")
	}
	n := cfg.Native
	if n.Dir != "/opt/qifa/app" || cfg.Retain() != 5 || n.Restart != "on-failure" || cfg.Health().DeployTimeout.Seconds() != 35 || cfg.Health().Timeout.Seconds() != 5 {
		t.Fatalf("defaults not applied: %+v %+v", n, cfg.Health())
	}
	if len(cfg.Warnings) != 0 {
		t.Fatalf("the unified keys raise no warnings: %v", cfg.Warnings)
	}
	if cfg.NativeUnitName("web") != "qifa-app.service" {
		t.Fatalf("single-role unit name = %s", cfg.NativeUnitName("web"))
	}
}

func TestNativeConfigRejects(t *testing.T) {
	cases := map[string]struct{ edit, want string }{
		"image":            {"image: x/app\n", "config.image is not used"},
		"builder":          {"builder:\n  context: .\n", "config.builder"},
		"two healthchecks": {"proxy:\n  host: a.example.com\n  healthcheck:\n    path: /x\n", "both set"},
		"ssl without host": {"proxy:\n  ssl: true\n  tls:\n    source: kamal\n", "needs config.proxy.host"},
		"proxy_boot image": {"proxy_boot:\n  image: x/proxy\n", "proxy_boot image"},
		"proxy_boot bind":  {"proxy_boot:\n  bind_ips: [10.0.0.1]\n", "bind_ips"},
		"registry":         {"registry:\n  server: r\n  username: u\n  password_env: P\n", "config.registry"},
		"accessory":        {"accessories:\n  redis:\n    image: redis:7\n    host: h\n", "config.accessories"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := loadString(t, nativeBase+c.edit)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want error containing %q", err, c.want)
			}
		})
	}

	replace := map[string]struct{ old, new, want string }{
		"published port": {"    app_port: 8080\n", "    port: 80\n", "publish container ports"},
		"missing cmd":    {"    cmd: [app, --port, \"8080\"]\n", "", "cmd is required"},
		"escaping cmd":   {"cmd: [app,", "cmd: [../app,", "clean path"},
		"shell cmd":      {"cmd: [app, --port, \"8080\"]", "cmd: app --port $PORT", "needs a shell"},
		"root user":      {"user: app\n", "user: root\n", "must not be root"},
		"no artifact":    {"  artifact: dist/app\n", "", "native.artifact is required"},
		"health no port": {"    app_port: 8080\n", "", "needs a port to probe"},
		"bad arch":       {"  user: app\n", "  user: app\n  arch: x86\n", "native.arch"},
		"bad shared dir": {"  user: app\n", "  user: app\n  shared_dirs: [../etc]\n", "shared_dirs[0]"},
		"bad dir":        {"  user: app\n", "  user: app\n  dir: relative/path\n", "native.dir"},
		"retain 1":       {"ssh:\n", "prune:\n  retain: 1\nssh:\n", "retain must be at least 2"},
		"bad restart":    {"  user: app\n", "  user: app\n  restart: sometimes\n", "Restart="},
		"bad service":    {"service: app\n", "service: \"my app\"\n", "config.service"},
		"newline in cmd": {"cmd: [app, --port, \"8080\"]", "cmd: [app, \"a\\nb\"]", "newline"},
	}
	for name, c := range replace {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(nativeBase, c.old) {
				t.Fatalf("test bug: %q not in base", c.old)
			}
			_, err := loadString(t, strings.Replace(nativeBase, c.old, c.new, 1))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want error containing %q", err, c.want)
			}
		})
	}
}

const installedBase = `service: agent
servers:
  web:
    hosts: [10.0.0.1]
    cmd: [/usr/local/bin/agent-runner]
    app_port: 9084
ssh:
  user: ubuntu
healthcheck:
  path: /health
native:
  user: ubuntu
  workdir: /data/agents/a/
  hardening: false
`

// No artifact: an absolute cmd runs a program installed on the server.
func TestNativeInstalledProgram(t *testing.T) {
	cfg, err := loadString(t, installedBase)
	if err != nil {
		t.Fatal(err)
	}
	n := cfg.Native
	if !n.Installed() || n.HardeningOn() || n.Workdir != "/data/agents/a" {
		t.Fatalf("installed=%v hardening=%v workdir=%q", n.Installed(), n.HardeningOn(), n.Workdir)
	}
	cfg, err = loadString(t, nativeBase)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Native.Installed() || !cfg.Native.HardeningOn() {
		t.Fatal("an artifact config ships releases, hardened by default")
	}
	for name, c := range map[string]struct{ old, new, want string }{
		"relative cmd":   {"/usr/local/bin/agent-runner", "bin/agent-runner", "native.artifact is required"},
		"build alone":    {"  hardening: false\n", "  hardening: false\n  build: make\n", "nothing would be shipped"},
		"bad workdir":    {"/data/agents/a/", "data/agents", "native.workdir"},
		"dotdot workdir": {"/data/agents/a/", "/data/../etc", "native.workdir"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadString(t, strings.Replace(installedBase, c.old, c.new, 1))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want error containing %q", err, c.want)
			}
		})
	}
}

// cmd means the same in both modes: a string, or a list. A list given to a
// container is quoted for the shell docker run hands it to.
func TestCmdListInDockerMode(t *testing.T) {
	cfg, err := loadString(t, "service: app\nimage: x/app:1\nservers:\n  web:\n    hosts: [h]\n    cmd: [app, serve, \"it's\"]\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Servers["web"].Cmd.ShellString(); got != `'app' 'serve' 'it'"'"'s'` {
		t.Fatalf("shell form = %s", got)
	}
}

func TestNativeMultiRoleUnitNames(t *testing.T) {
	cfg, err := loadString(t, strings.Replace(nativeBase, "ssh:\n", "  worker:\n    hosts: [10.0.0.1]\n    cmd: app work\nssh:\n", 1))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.NativeUnitName("worker") != "qifa-app-worker.service" || cfg.NativeExec("worker")[1] != "work" || cfg.NativeExec("web")[1] != "--port" {
		t.Fatalf("multi-role: %s %v %v", cfg.NativeUnitName("worker"), cfg.NativeExec("worker"), cfg.NativeExec("web"))
	}
}

func TestNativeProxiedConfig(t *testing.T) {
	cfg, err := loadString(t, nativeBase+"proxy:\n  host: app.example.com\n  ssl: true\n  tls:\n    source: kamal\nproxy_boot:\n  hosts: [10.0.0.1]\n")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.NativeProxied() || cfg.NativeProxyRole() != "web" {
		t.Fatalf("proxied=%v role=%q", cfg.NativeProxied(), cfg.NativeProxyRole())
	}
	if cfg.ProxyBoot.Version != NativeProxyVersion || cfg.ProxyBoot.Image != "" || cfg.ProxyBoot.Network != "" {
		t.Fatalf("native proxy_boot defaults: %+v", cfg.ProxyBoot)
	}
	// Routing needs a port and a health path to give kamal-proxy.
	noHealth := strings.Replace(nativeBase, "healthcheck:\n  path: /up\n", "", 1)
	if noHealth == nativeBase {
		t.Fatal("test bug: healthcheck not removed")
	}
	if _, err := loadString(t, noHealth+"proxy:\n  host: app.example.com\n"); err == nil || !strings.Contains(err.Error(), "healthcheck.path") {
		t.Fatalf("got %v", err)
	}
}

func TestProxyBootBinaryOnlyWithNative(t *testing.T) {
	_, err := loadString(t, "service: app\nimage: x/app:1\nservers:\n  web:\n    hosts: [h]\nproxy_boot:\n  binary: ./kamal-proxy\n")
	if err == nil || !strings.Contains(err.Error(), "proxy_boot.binary") {
		t.Fatalf("got %v", err)
	}
}

func TestNativeAcceptsQifaTLS(t *testing.T) {
	cfg, err := loadString(t, nativeBase+"proxy:\n  host: app.example.com\n  ssl: true\n  tls:\n    source: qifa\n    provider: cloudflare\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Proxy.TLS.Source != "qifa" {
		t.Fatalf("tls source = %q", cfg.Proxy.TLS.Source)
	}
}

// A config written before the keys were unified loads unchanged in meaning,
// with a warning naming each key to move.
func TestLegacyNativeKeysMigrate(t *testing.T) {
	legacy := `service: app
servers:
  web:
    hosts: [10.0.0.1]
  worker:
    hosts: [10.0.0.1]
    exec: [app, work]
native:
  artifact: dist/app
  exec: [app, --port, "8080"]
  user: app
  port: 8080
  retain: 3
  healthcheck:
    path: /up
    timeout: 45s
proxy:
  host: app.example.com
`
	cfg, err := loadString(t, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.NativeExec("web"); strings.Join(got, " ") != "app --port 8080" {
		t.Fatalf("web cmd = %v", got)
	}
	if got := cfg.NativeExec("worker"); strings.Join(got, " ") != "app work" {
		t.Fatalf("worker cmd = %v", got)
	}
	if cfg.AppPort("web") != 8080 || cfg.AppPort("worker") != 0 {
		t.Fatalf("app ports: web %d worker %d", cfg.AppPort("web"), cfg.AppPort("worker"))
	}
	h := cfg.Health()
	if h.Path != "/up" || h.DeployTimeout.Seconds() != 45 || h.Timeout.Seconds() != 5 {
		t.Fatalf("native.healthcheck.timeout was the whole wait: %+v", h)
	}
	if cfg.Retain() != 3 {
		t.Fatalf("retain = %d", cfg.Retain())
	}
	joined := strings.Join(cfg.Warnings, "\n")
	for _, key := range []string{"native.port", "native.retain", "native.healthcheck", "native.exec", "servers.worker.exec"} {
		if !strings.Contains(joined, key) {
			t.Errorf("no warning for %s:\n%s", key, joined)
		}
	}
	// Migrated keys are gone from the loaded config: `qifa config` shows
	// the unified form.
	out, _ := cfg.Marshal()
	for _, gone := range []string{"exec:", "retain: 3\n    healthcheck"} {
		if strings.Contains(string(out), gone) {
			t.Errorf("qifa config still shows %q:\n%s", gone, out)
		}
	}
}

func TestLegacyDockerKeysMigrate(t *testing.T) {
	cfg, err := loadString(t, `service: app
image: x/app:1
servers:
  web:
    hosts: [h]
proxy:
  host: a.example.com
  healthcheck:
    path: /ready
    timeout: 3s
prune:
  retain_containers: 7
`)
	if err != nil {
		t.Fatal(err)
	}
	if h := cfg.Health(); h.Path != "/ready" || h.Timeout.Seconds() != 3 || h.DeployTimeout.Seconds() != 25 {
		t.Fatalf("health = %+v", h)
	}
	if cfg.Retain() != 7 {
		t.Fatalf("retain = %d", cfg.Retain())
	}
	if p := cfg.ProxyRoute(); p.Healthcheck.Path != "/ready" || p.DeployTimeout.Seconds() != 25 {
		t.Fatalf("proxy route = %+v", p)
	}
	if len(cfg.Warnings) != 2 {
		t.Fatalf("warnings = %v", cfg.Warnings)
	}
	if _, err := loadString(t, "service: app\nimage: x/app:1\nservers:\n  web:\n    hosts: [h]\nprune:\n  retain: 2\n  retain_containers: 3\n"); err == nil || !strings.Contains(err.Error(), "both set") {
		t.Fatalf("both retention keys: %v", err)
	}
}

func TestSplitWords(t *testing.T) {
	for in, want := range map[string]string{
		`bin/app serve --port 8080`: "bin/app|serve|--port|8080",
		`app "two words" 'it''s'`:   "app|two words|its",
		`app a\ b "q\"q"`:           `app|a b|q"q`,
	} {
		got, err := SplitWords(in)
		if err != nil || strings.Join(got, "|") != want {
			t.Errorf("SplitWords(%q) = %q, %v; want %q", in, strings.Join(got, "|"), err, want)
		}
	}
	for _, bad := range []string{"app $HOME", "a | b", "a; b", "a > f", "a *", `a "$X"`, "a 'unterminated"} {
		if _, err := SplitWords(bad); err == nil {
			t.Errorf("SplitWords(%q) accepted something that needs a shell", bad)
		}
	}
}

// Docker mode always allowed five attempts. A config with a longer timeout
// or interval must keep that time when deploy_timeout is not set: it went
// from 75s to 30s for a 10s/5s check in the first cut of the unified key.
func TestUnsetDeployTimeoutKeepsFiveAttempts(t *testing.T) {
	cfg, err := loadString(t, "service: app\nimage: x/app:1\nservers:\n  web:\n    hosts: [h]\nproxy:\n  host: a.example.com\n  healthcheck:\n    path: /health\n    interval: 5s\n    timeout: 10s\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Health().DeployTimeout.Seconds(); got != 75 {
		t.Fatalf("deploy_timeout = %vs, want 75s (5 x (10s + 5s))", got)
	}
	// An explicit deploy_timeout is taken as given.
	cfg, err = loadString(t, "service: app\nimage: x/app:1\nservers:\n  web:\n    hosts: [h]\nhealthcheck:\n  path: /health\n  timeout: 10s\n  deploy_timeout: 20s\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Health().DeployTimeout.Seconds(); got != 20 {
		t.Fatalf("explicit deploy_timeout = %vs", got)
	}
}

// A native app can route through the server's Docker kamal-proxy instead of
// a native one: both kinds of app behind one proxy on a mixed server.
func TestNativeContainerProxyMode(t *testing.T) {
	base := nativeBase + "proxy:\n  host: app.example.com\n"
	cfg, err := loadString(t, base+"proxy_boot:\n  mode: container\n  network: edge\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProxyMode() != ProxyModeContainer || cfg.ProxyBoot.Network != "edge" {
		t.Fatalf("mode=%s network=%s", cfg.ProxyMode(), cfg.ProxyBoot.Network)
	}
	if cfg, _ := loadString(t, base); cfg.ProxyMode() != ProxyModeNative {
		t.Fatal("native configs default to the native proxy")
	}
	for name, c := range map[string]struct{ yaml, want string }{
		"bad mode":              {base + "proxy_boot:\n  mode: sidecar\n", "proxy_boot.mode"},
		"network without mode":  {base + "proxy_boot:\n  network: edge\n", "proxy_boot.network"},
		"binary with container": {base + "proxy_boot:\n  mode: container\n  binary: ./kp\n", "proxy_boot.binary"},
		"docker config native":  {"service: app\nimage: x/app:1\nservers:\n  web:\n    hosts: [h]\nproxy_boot:\n  mode: native\n", "always uses the container proxy"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadString(t, c.yaml); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want %q", err, c.want)
			}
		})
	}
}

// agent-slow-town on a Mac: an installed program, run as a LaunchAgent of the
// deploy user.
const launchdBase = `service: slow-town
servers:
  web:
    hosts: [mini]
    cmd: [/Users/mini/go/bin/agent-runner]
    app_port: 8080
ssh:
  user: mini
healthcheck:
  path: /health
native:
  init: launchd
  user: mini
  dir: /Users/mini/qifa/slow-town
  workdir: /Users/mini/workspace/agent-slow-town
`

func TestNativeLaunchd(t *testing.T) {
	cfg, err := loadString(t, launchdBase)
	if err != nil {
		t.Fatal(err)
	}
	n := cfg.Native
	if !n.Launchd() || n.HardeningOn() || n.Dir != "/Users/mini/qifa/slow-town" {
		t.Fatalf("launchd=%v hardening=%v dir=%q", n.Launchd(), n.HardeningOn(), n.Dir)
	}
	if got := cfg.NativeUnitName("web"); got != "qifa.slow-town" {
		t.Fatalf("label = %s", got)
	}
	cfg, err = loadString(t, strings.Replace(launchdBase, "ssh:\n", "  worker:\n    hosts: [mini]\n    cmd: [/Users/mini/go/bin/agent-runner, work]\nssh:\n", 1))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.NativeUnitName("worker"); got != "qifa.slow-town.worker" {
		t.Fatalf("multi-role label = %s", got)
	}
}

func TestNativeLaunchdRejects(t *testing.T) {
	replace := map[string]struct{ old, new, want string }{
		"unknown init": {"init: launchd", "init: upstart", "use systemd (Linux) or launchd (macOS)"},
		"no dir":       {"  dir: /Users/mini/qifa/slow-town\n", "", "native.dir is required with init: launchd"},
		"hardening":    {"  init: launchd\n", "  init: launchd\n  hardening: true\n", "launchd has none"},
		"other user":   {"  init: launchd\n  user: mini\n", "  init: launchd\n  user: agent\n", "a LaunchAgent runs as the user who deploys it"},
		"on-watchdog":  {"  init: launchd\n", "  init: launchd\n  restart: on-watchdog\n", "no launchd equivalent"},
		"proxied":      {"ssh:\n", "proxy:\n  host: a.example.com\nssh:\n", "not supported with init: launchd"},
		"proxy_boot":   {"ssh:\n", "proxy_boot:\n  mode: native\nssh:\n", "not supported with init: launchd"},
	}
	for name, c := range replace {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(launchdBase, c.old) {
				t.Fatalf("test bug: %q not in base", c.old)
			}
			_, err := loadString(t, strings.Replace(launchdBase, c.old, c.new, 1))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want error containing %q", err, c.want)
			}
		})
	}
}
