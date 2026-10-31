package native

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tendant/qifa/internal/config"
)

func TestExecStartQuotesArgumentsAndNeutralisesExpansion(t *testing.T) {
	got := ExecStart("/opt/qifa/app", []string{"bin/app", "--name", `a "b" \c`, "100%", "$HOME"})
	want := `/opt/qifa/app/current/bin/app "--name" "a \"b\" \\c" "100%%" "$$HOME"`
	if got != want {
		t.Fatalf("ExecStart:\n got %s\nwant %s", got, want)
	}
	if got := ExecStart("/opt/qifa/app", []string{"./app"}); got != "/opt/qifa/app/current/app" {
		t.Fatalf("relative ./ path: %s", got)
	}
	if got := ExecStart("/opt/qifa/app", []string{"/usr/bin/java", "-jar", "app.jar"}); got != `/usr/bin/java "-jar" "app.jar"` {
		t.Fatalf("absolute path: %s", got)
	}
}

func TestRenderEnvEscapesForSystemd(t *testing.T) {
	out, err := RenderEnv(map[string]string{
		"B": `say "hi" \ $HOME ` + "`x`",
		"A": "line1\nline2",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "# Managed by qifa deploy. Do not edit: the next deploy overwrites it.\n" +
		"A=\"line1\nline2\"\n" +
		"B=\"say \\\"hi\\\" \\\\ \\$HOME \\`x\\`\"\n"
	if string(out) != want {
		t.Fatalf("RenderEnv:\n got %q\nwant %q", out, want)
	}
	if _, err := RenderEnv(map[string]string{"BAD-KEY": "x"}); err == nil {
		t.Fatal("expected an invalid key to be rejected")
	}
}

func TestRenderSudoersIsExactAndWildcardFree(t *testing.T) {
	got := RenderSudoers("deploy", "/usr/bin/systemctl", []string{"qifa-app.service"})
	if strings.Contains(got, "*") {
		t.Fatalf("sudoers must not contain wildcards:\n%s", got)
	}
	for _, verb := range []string{"start", "stop", "restart"} {
		if !strings.Contains(got, "/usr/bin/systemctl "+verb+" qifa-app.service") {
			t.Fatalf("missing %s rule:\n%s", verb, got)
		}
	}
	if strings.Contains(got, "daemon-reload") || strings.Contains(got, "install") {
		t.Fatalf("sudoers must not grant unit writes:\n%s", got)
	}
}

func TestUnitUsesCurrentAndHardening(t *testing.T) {
	unit := RenderUnit(UnitParams{Service: "app", Role: "web", User: "app", Dir: "/opt/qifa/app", Exec: []string{"app"}, Restart: "on-failure"})
	for _, want := range []string{
		"User=app\n",
		"WorkingDirectory=/opt/qifa/app/current\n",
		"ExecStart=/opt/qifa/app/current/app\n",
		"EnvironmentFile=-/opt/qifa/app/etc/env\n",
		"NoNewPrivileges=true\n",
		"ReadWritePaths=/opt/qifa/app/shared\n",
	} {
		if !strings.Contains(unit, want) {
			t.Fatalf("unit lacks %q:\n%s", want, unit)
		}
	}
}

func TestPackIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "bin", "app"), "#!/bin/sh\necho hi\n", 0o755)
	mustWrite(t, filepath.Join(dir, "config.yml"), "a: 1\n", 0o644)
	if err := os.Symlink("bin/app", filepath.Join(dir, "run")); err != nil {
		t.Fatal(err)
	}
	a, err := Pack(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Touching a file changes its mtime, not its contents: same checksum.
	if err := os.Chtimes(filepath.Join(dir, "config.yml"), timeNow(), timeNow()); err != nil {
		t.Fatal(err)
	}
	b, err := Pack(dir)
	if err != nil {
		t.Fatal(err)
	}
	if a.SHA256 != b.SHA256 {
		t.Fatal("same contents packed to different checksums")
	}
	mustWrite(t, filepath.Join(dir, "config.yml"), "a: 2\n", 0o644)
	c, _ := Pack(dir)
	if c.SHA256 == a.SHA256 {
		t.Fatal("different contents packed to the same checksum")
	}
	if !a.IsDir {
		t.Fatal("directory artifact not marked IsDir")
	}

	// The archive extracts with the system tar to the same tree.
	out := t.TempDir()
	tgz := filepath.Join(t.TempDir(), "a.tgz")
	if err := os.WriteFile(tgz, a.Data, 0o600); err != nil {
		t.Fatal(err)
	}
	if msg, err := exec.Command("tar", "-xzf", tgz, "-C", out).CombinedOutput(); err != nil {
		t.Fatalf("tar: %v %s", err, msg)
	}
	fi, err := os.Stat(filepath.Join(out, "bin", "app"))
	if err != nil || fi.Mode().Perm()&0o100 == 0 {
		t.Fatalf("bin/app missing or not executable after extract: %v", err)
	}
	if target, _ := os.Readlink(filepath.Join(out, "run")); target != "bin/app" {
		t.Fatalf("symlink not preserved: %q", target)
	}
}

func TestELFArchOfThisTestBinary(t *testing.T) {
	if runtime.GOOS != "linux" {
		// A Mach-O or PE binary is not ELF: no architecture, no error.
		arch, err := ELFArch(os.Args[0])
		if err != nil || arch != "" {
			t.Fatalf("non-ELF: got %q, %v", arch, err)
		}
		return
	}
	arch, err := ELFArch(os.Args[0])
	if err != nil || arch != runtime.GOARCH {
		t.Fatalf("got %q, %v; want %s", arch, err, runtime.GOARCH)
	}
}

func TestUnameArch(t *testing.T) {
	for in, want := range map[string]string{"x86_64": "amd64", "aarch64": "arm64", "armv7l": "arm", "i686": "386", "riscv64": "riscv64"} {
		if got := UnameArch(in); got != want {
			t.Errorf("UnameArch(%q) = %q, want %q", in, got, want)
		}
	}
}

// Every remote script must at least parse as POSIX sh.
func TestScriptsParse(t *testing.T) {
	scripts := map[string]string{
		"stage":       stageScript("/opt/qifa/app", "abc123", "deadbeef", []string{"storage", "var/log"}, []string{"bin/app", "/usr/bin/java"}),
		"activate":    activateScript("/opt/qifa/app", "abc123"),
		"revert":      revertScript("/opt/qifa/app", "releases/old"),
		"markok":      markOKScript("/opt/qifa/app", "abc123"),
		"health-http": healthScript("qifa-app.service", "http://127.0.0.1:8080/up", 30, 2),
		"health":      healthScript("qifa-app.service", "", 30, 2),
		"preflight":   preflightScript("/opt/qifa/app", []string{"qifa-app.service"}),
		"diagnostics": diagnosticsScript("qifa-app.service"),
	}
	cfg := testConfig()
	d := &Deployer{cfg: cfg}
	scripts["setup"] = d.setupScript("h1", "deploy", "/usr/bin/systemctl")
	for name, s := range scripts {
		if out, err := exec.Command("sh", "-n", "-c", s).CombinedOutput(); err != nil {
			t.Errorf("%s does not parse: %v\n%s\n%s", name, err, out, s)
		}
	}
}

// The stage, activate and revert scripts run for real against a temp dir
// (sha256sum and mv -T are GNU; skipped where they are missing).
func TestReleaseLifecycleLocally(t *testing.T) {
	for _, tool := range []string{"sha256sum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	if err := exec.Command("sh", "-c", "mv -T /dev/null /dev/null 2>&1 | grep -q 'invalid option\\|illegal option'").Run(); err == nil {
		t.Skip("mv -T not supported (BSD mv)")
	}
	base := t.TempDir()
	dir := filepath.Join(base, "app")
	for _, sub := range []string{"releases", "etc", "shared/storage"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	art := t.TempDir()
	mustWrite(t, filepath.Join(art, "bin", "app"), "#!/bin/sh\n", 0o755)

	stage := func(id string) string {
		pkg, err := Pack(art)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(uploadPath(dir, id), pkg.Data, 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command("sh", "-c", stageScript(dir, id, pkg.SHA256, []string{"storage"}, []string{"bin/app"})).CombinedOutput()
		if err != nil {
			t.Fatalf("stage %s: %v\n%s", id, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	activate := func(id string) string {
		mustWrite(t, filepath.Join(dir, "etc", "env.next"), "V="+id+"\n", 0o600)
		out, err := exec.Command("sh", "-c", activateScript(dir, id)).CombinedOutput()
		if err != nil {
			t.Fatalf("activate %s: %v\n%s", id, err, out)
		}
		return string(out)
	}

	if got := stage("v1"); got != "staged" {
		t.Fatalf("stage v1: %q", got)
	}
	if prev := activate("v1"); prev != "" {
		t.Fatalf("first activation reported previous %q", prev)
	}
	if got := stage("v1"); got != "reused" {
		t.Fatalf("restaging identical v1: %q", got)
	}
	if link, _ := os.Readlink(filepath.Join(dir, "releases", "v1", "storage")); link != filepath.Join(dir, "shared", "storage") {
		t.Fatalf("shared dir link: %q", link)
	}
	stage("v2")
	if prev := activate("v2"); prev != "releases/v1" {
		t.Fatalf("previous = %q, want releases/v1", prev)
	}
	if out, err := exec.Command("sh", "-c", revertScript(dir, "releases/v1")).CombinedOutput(); err != nil {
		t.Fatalf("revert: %v %s", err, out)
	}
	if link, _ := os.Readlink(filepath.Join(dir, "current")); link != "releases/v1" {
		t.Fatalf("after revert current = %q", link)
	}
	if env, _ := os.ReadFile(filepath.Join(dir, "etc", "env")); string(env) != "V=v1\n" {
		t.Fatalf("after revert env = %q, want v1's", env)
	}

	// Changing the live release's bytes under the same version is refused.
	mustWrite(t, filepath.Join(art, "bin", "app"), "#!/bin/sh\necho changed\n", 0o755)
	pkg, _ := Pack(art)
	_ = os.WriteFile(uploadPath(dir, "v1"), pkg.Data, 0o600)
	if out, err := exec.Command("sh", "-c", stageScript(dir, "v1", pkg.SHA256, nil, []string{"bin/app"})).CombinedOutput(); err == nil {
		t.Fatalf("restaging live v1 with new contents succeeded: %s", out)
	}

	// A release whose exec is missing is refused before it becomes a release.
	_ = os.WriteFile(uploadPath(dir, "v3"), pkg.Data, 0o600)
	if out, err := exec.Command("sh", "-c", stageScript(dir, "v3", pkg.SHA256, nil, []string{"bin/missing"})).CombinedOutput(); err == nil {
		t.Fatalf("stage with missing exec succeeded: %s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "releases", "v3")); err == nil {
		t.Fatal("failed stage left a release directory")
	}
}

func TestHostsAndUnits(t *testing.T) {
	cfg := testConfig()
	cfg.Servers["worker"] = config.Server{Hosts: []string{"h2", "h3"}, Exec: []string{"bin/worker"}}
	d := &Deployer{cfg: cfg}
	if got := strings.Join(d.hosts(), ","); got != "h1,h2,h3" {
		t.Fatalf("hosts = %s", got)
	}
	if got := strings.Join(d.unitsOn("h2"), ","); got != "qifa-app-web.service,qifa-app-worker.service" {
		t.Fatalf("units on h2 = %s", got)
	}
	if d.healthURL("worker") != "" {
		t.Fatal("worker role should not get the HTTP check")
	}
	if got := d.healthURL("web"); got != "http://127.0.0.1:8080/up" {
		t.Fatalf("web health URL = %s", got)
	}
}

func testConfig() *config.Config {
	return &config.Config{
		Service: "app",
		Servers: map[string]config.Server{"web": {Hosts: []string{"h1", "h2"}}},
		Native: &config.Native{
			Artifact:    "dist/app",
			Exec:        []string{"app"},
			User:        "app",
			Dir:         "/opt/qifa/app",
			Port:        8080,
			Healthcheck: config.Healthcheck{Path: "/up"},
			Restart:     "on-failure",
			SharedDirs:  []string{"storage"},
		},
	}
}

func mustWrite(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func timeNow() time.Time { return time.Now().Add(time.Hour) }
