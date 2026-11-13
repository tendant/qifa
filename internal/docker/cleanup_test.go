package docker

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A build's working directory is removed after it, whether it succeeded or
// failed; a failure still fails, with its status, and stops the steps after
// it.
func TestCleanupAfter(t *testing.T) {
	run := func(t *testing.T, steps ...string) (string, int) {
		t.Helper()
		root := filepath.Join(t.TempDir(), "qifa-build-1")
		if err := os.MkdirAll(filepath.Join(root, "repo"), 0o755); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(t.TempDir(), "after-failure")
		for i, s := range steps {
			if s == "MARK" {
				steps[i] = "touch " + shellQuote(marker)
			}
		}
		err := exec.Command("sh", "-c", cleanupAfter(root, steps)).Run()
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(root); !os.IsNotExist(err) {
			t.Errorf("%v: the build directory is still there", steps)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Errorf("%v: a step ran after a failed one", steps)
		}
		return root, code
	}
	if _, code := run(t, "true", "echo built > /dev/null"); code != 0 {
		t.Errorf("success: exit %d", code)
	}
	if _, code := run(t, "true", "exit 3", "MARK"); code != 3 {
		t.Errorf("failure: exit %d, want 3", code)
	}
}
