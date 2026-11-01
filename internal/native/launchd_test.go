package native

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/tendant/qifa/internal/config"
)

func launchdConfig() *config.Config {
	return &config.Config{
		Service: "slow-town",
		Servers: map[string]config.Server{"web": {Hosts: []string{"mini"}}},
		Native: &config.Native{
			Init:    config.InitLaunchd,
			Exec:    []string{"/Users/mini/go/bin/agent-runner"},
			User:    "mini",
			Dir:     "/Users/mini/qifa/slow-town",
			Workdir: "/Users/mini/workspace/agent-slow-town",
			Restart: "on-failure",
		},
	}
}

// The plist is valid, and what launchd would run gets the env file's values
// and the argv exactly as configured: no shell parses either.
func TestRenderPlistRunsArgvVerbatimWithTheEnv(t *testing.T) {
	plutil, err := exec.LookPath("plutil")
	if err != nil {
		t.Skip("plutil not available (macOS only)")
	}
	dir := t.TempDir()
	args := []string{"a & b", "<x>", "$HOME", `say "hi" \ ` + "`x`", "100%"}
	bin := filepath.Join(dir, "current", "bin", "app")
	mustWrite(t, bin, "#!/bin/sh\nprintf '%s\\n' \"$@\" \"SECRET=$SECRET\"\n", 0o755)
	env, err := RenderEnv(map[string]string{"SECRET": "p$w \"d\"\nline2"})
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dir, "etc", "env"), string(env), 0o600)

	plist := RenderPlist(PlistParams{
		Label: "qifa.app", Dir: dir, Exec: append([]string{"bin/app"}, args...),
		Restart: "on-failure", Workdir: dir, LogFile: dir + "/log/qifa.app.log",
	})
	file := filepath.Join(dir, "qifa.app.plist")
	mustWrite(t, file, plist, 0o644)
	if out, err := exec.Command(plutil, "-lint", file).CombinedOutput(); err != nil {
		t.Fatalf("plutil -lint: %v\n%s\n%s", err, out, plist)
	}
	out, err := exec.Command(plutil, "-convert", "json", "-o", "-", file).Output()
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Label            string
		ProgramArguments []string
		KeepAlive        map[string]bool
		WorkingDirectory string
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Label != "qifa.app" || parsed.WorkingDirectory != dir || parsed.KeepAlive["SuccessfulExit"] {
		t.Fatalf("plist: %+v", parsed)
	}

	got, err := exec.Command(parsed.ProgramArguments[0], parsed.ProgramArguments[1:]...).Output()
	if err != nil {
		t.Fatalf("running ProgramArguments: %v", err)
	}
	want := strings.Join(args, "\n") + "\nSECRET=p$w \"d\"\nline2\n"
	if string(got) != want {
		t.Fatalf("the app saw:\n%q\nwant\n%q", got, want)
	}
}

func TestKeepAliveFollowsRestart(t *testing.T) {
	for restart, want := range map[string]string{
		"always":     "<true/>",
		"no":         "<false/>",
		"on-success": "<key>SuccessfulExit</key>\n\t\t<true/>",
		"on-failure": "<key>SuccessfulExit</key>\n\t\t<false/>",
	} {
		if got := keepAlive(restart); !strings.Contains(got, want) {
			t.Errorf("%s: %q, want %q", restart, got, want)
		}
	}
}

func TestLaunchdScriptsParse(t *testing.T) {
	d := &Deployer{cfg: launchdConfig()}
	unit := d.cfg.NativeUnitName("web")
	scripts := map[string]string{
		"start":       launchctlScript("start", unit, d.logFile(unit)),
		"stop":        launchctlScript("stop", unit, d.logFile(unit)),
		"restart":     launchctlScript("restart", unit, d.logFile(unit)),
		"preflight":   launchdPreflight(d.cfg.Native.Dir, []string{unit}),
		"setup":       d.launchdSetupScript("mini"),
		"teardown":    d.launchdTeardownScript("mini", true),
		"exec":        launchdExecScript(d.cfg.Native.Dir, d.workdir(), "ls -la"),
		"diagnostics": d.diagnostics(unit),
		"health":      healthScript(unit, d.isActive(unit), d.unitState(unit), "http://127.0.0.1:8080/health", 30, 2, 5),
		"inventory":   inventoryScript(d.cfg.Native.Dir, []string{unit}, d.unitState, ""),
	}
	for name, s := range scripts {
		if out, err := exec.Command("sh", "-n", "-c", s).CombinedOutput(); err != nil {
			t.Errorf("%s does not parse: %v\n%s\n%s", name, err, out, s)
		}
		// Each goes through /bin/sh on the host, whatever its login shell.
		if got := d.sh(s); !strings.HasPrefix(got, "/bin/sh -c '") {
			t.Errorf("%s is not run through /bin/sh: %.40s", name, got)
		}
	}
	if (&Deployer{cfg: testConfig()}).sh("true") != "true" {
		t.Error("systemd hosts' scripts must run as they always have")
	}
}

// The setup script, run for real against a temp home, lays out the install
// directory and the plist, and a second run changes nothing.
func TestLaunchdSetupLocally(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS only")
	}
	home := t.TempDir()
	cfg := launchdConfig()
	user := os.Getenv("USER")
	cfg.Native.User = user
	cfg.Native.Dir = filepath.Join(home, "qifa", "app")
	cfg.Native.Workdir = filepath.Join(home, "work")
	d := &Deployer{cfg: cfg}
	run := func() string {
		cmd := exec.Command("sh", "-c", d.launchdSetupScript("mini"))
		cmd.Env = append(os.Environ(), "HOME="+home)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("setup: %v\n%s", err, out)
		}
		return string(out)
	}
	if out := run(); !strings.Contains(out, "installed qifa.slow-town") || !strings.Contains(out, "created "+cfg.Native.Workdir) {
		t.Fatalf("first setup said: %s", out)
	}
	if out := run(); strings.TrimSpace(out) != "" {
		t.Fatalf("second setup changed something: %s", out)
	}
	plist, err := os.ReadFile(filepath.Join(home, "Library", "LaunchAgents", "qifa.slow-town.plist"))
	if err != nil || sha256Hex(string(plist)) != d.launchdUnitSum("web") {
		t.Fatalf("installed plist differs from the rendered one: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(cfg.Native.Dir, "etc", "env")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("etc/env: %v %v", fi, err)
	}
}

func TestMachOArchOfThisTestBinary(t *testing.T) {
	arch, err := MachOArch(os.Args[0])
	if runtime.GOOS != "darwin" {
		if err != nil || arch != "" {
			t.Fatalf("non-Mach-O: got %q, %v", arch, err)
		}
		return
	}
	if err != nil || arch != runtime.GOARCH {
		t.Fatalf("got %q, %v; want %s", arch, err, runtime.GOARCH)
	}
}

// A Linux build shipped to a Mac (or the reverse) is refused before upload.
func TestArtifactArchChecksTheHostsOS(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("needs an ELF or Mach-O test binary")
	}
	for _, init := range []string{config.InitSystemd, config.InitLaunchd} {
		cfg := launchdConfig()
		cfg.Native.Init = init
		cfg.Native.Artifact = os.Args[0]
		cfg.Native.Exec = []string{filepath.Base(os.Args[0])}
		d := &Deployer{cfg: cfg}
		arch, err := d.artifactArch(&Package{})
		matches := (init == config.InitLaunchd) == (runtime.GOOS == "darwin")
		switch {
		case matches && (err != nil || arch != runtime.GOARCH):
			t.Errorf("%s: got %q, %v; want %s", init, arch, err, runtime.GOARCH)
		case !matches && (err == nil || !strings.Contains(err.Error(), "build with GOOS=")):
			t.Errorf("%s: a %s binary was accepted (%v)", init, runtime.GOOS, err)
		}
	}
}
