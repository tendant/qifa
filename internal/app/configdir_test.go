package app

import (
	"os"
	"path/filepath"
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
