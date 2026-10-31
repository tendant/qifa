package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A relative -c argument must still name the right file after the process
// moves, and the process must land in the config's directory so every
// relative path in the config resolves against it.
func TestUseConfigDirMovesToConfigDirectory(t *testing.T) {
	root := t.TempDir()
	deployDir := filepath.Join(root, "deploy")
	if err := os.MkdirAll(deployDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(deployDir, "qifa.yaml")
	if err := os.WriteFile(cfgPath, []byte("service: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Chdir(root)
	abs, err := useConfigDir("deploy/qifa.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("returned path is not readable: %v", err)
	}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// macOS hands out /var/folders paths that resolve through /private.
	got, _ := filepath.EvalSymlinks(wd)
	want, _ := filepath.EvalSymlinks(deployDir)
	if got != want {
		t.Fatalf("cwd is %q, want %q", got, want)
	}
}

func TestUseConfigDirRejectsMissingDirectory(t *testing.T) {
	t.Chdir(t.TempDir())
	if _, err := useConfigDir("nope/qifa.yaml"); err == nil {
		t.Fatal("expected an error for a directory that does not exist")
	}
}

// A relative -c path keeps working for commands that load the config more
// than once (proxy, remove): the second load must not resolve the path again
// from inside the config's directory.
func TestRelativeConfigPathSurvivesSecondLoad(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "deploy")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "service: app\nimage: x/app:1\nservers:\n  web:\n    hosts: [127.0.0.1]\n"
	if err := os.WriteFile(filepath.Join(dir, "qifa.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(wd) })
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	// An unknown proxy verb loads the config twice and then reports usage,
	// without touching any host.
	err := Run(context.Background(), []string{"-c", "deploy/qifa.yaml", "proxy", "no-such-verb"}, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "usage: qifa proxy") {
		t.Fatalf("got %v, want the proxy usage error (not a config path error)", err)
	}
}
