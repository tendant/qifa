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
ssh:
  user: deploy
native:
  build: make build
  artifact: dist/app
  exec: [app, --port, "8080"]
  user: app
  port: 8080
  healthcheck:
    path: /up
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
	if n.Dir != "/opt/qifa/app" || n.Retain != 5 || n.Restart != "on-failure" || n.Healthcheck.Timeout.Seconds() != 30 {
		t.Fatalf("defaults not applied: %+v", n)
	}
	if cfg.NativeUnitName("web") != "qifa-app.service" {
		t.Fatalf("single-role unit name = %s", cfg.NativeUnitName("web"))
	}
}

func TestNativeConfigRejects(t *testing.T) {
	cases := map[string]struct{ edit, want string }{
		"image":     {"image: x/app\n", "config.image is not used"},
		"builder":   {"builder:\n  context: .\n", "config.builder"},
		"proxy":     {"proxy:\n  host: a.example.com\n", "config.proxy"},
		"registry":  {"registry:\n  server: r\n  username: u\n  password_env: P\n", "config.registry"},
		"accessory": {"accessories:\n  redis:\n    image: redis:7\n    host: h\n", "config.accessories"},
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
		"role cmd":        {"hosts: [10.0.0.1]\n", "hosts: [10.0.0.1]\n    cmd: ./x\n", "cmd is for containers"},
		"role port":       {"hosts: [10.0.0.1]\n", "hosts: [10.0.0.1]\n    app_port: 80\n", "native.port"},
		"missing exec":    {"  exec: [app, --port, \"8080\"]\n", "", "config.native.exec is required"},
		"escaping exec":   {"exec: [app,", "exec: [../app,", "clean path"},
		"root user":       {"user: app\n", "user: root\n", "must not be root"},
		"no artifact":     {"  artifact: dist/app\n", "", "native.artifact is required"},
		"health no port":  {"  port: 8080\n", "", "needs config.native.port"},
		"bad arch":        {"  user: app\n", "  user: app\n  arch: x86\n", "native.arch"},
		"bad shared dir":  {"  user: app\n", "  user: app\n  shared_dirs: [../etc]\n", "shared_dirs[0]"},
		"bad dir":         {"  user: app\n", "  user: app\n  dir: relative/path\n", "native.dir"},
		"retain 1":        {"  user: app\n", "  user: app\n  retain: 1\n", "retain must be at least 2"},
		"bad restart":     {"  user: app\n", "  user: app\n  restart: sometimes\n", "Restart="},
		"bad service":     {"service: app\n", "service: \"my app\"\n", "config.service"},
		"newline in exec": {"exec: [app, --port, \"8080\"]", "exec: [app, \"a\\nb\"]", "newline"},
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

func TestExecOnlyWithNative(t *testing.T) {
	_, err := loadString(t, "service: app\nimage: x/app:1\nservers:\n  web:\n    hosts: [h]\n    exec: [app]\n")
	if err == nil || !strings.Contains(err.Error(), "applies only with config.native") {
		t.Fatalf("got %v", err)
	}
}

func TestNativeMultiRoleUnitNames(t *testing.T) {
	cfg, err := loadString(t, strings.Replace(nativeBase, "    hosts: [10.0.0.1]\n", "    hosts: [10.0.0.1]\n  worker:\n    hosts: [10.0.0.1]\n    exec: [app, work]\n", 1))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.NativeUnitName("worker") != "qifa-app-worker.service" || cfg.NativeExec("worker")[1] != "work" || cfg.NativeExec("web")[1] != "--port" {
		t.Fatalf("multi-role: %s %v %v", cfg.NativeUnitName("worker"), cfg.NativeExec("worker"), cfg.NativeExec("web"))
	}
}
