package deploy

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// gitRepo makes a temp git repo with one commit and moves the test into it.
func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
		{"commit", "-q", "--allow-empty", "-m", "first"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func TestResolveVersionExplicitWins(t *testing.T) {
	gitRepo(t)
	t.Setenv("QIFA_VERSION", "from-env")
	got, err := resolveVersion("v1.2.3", false)
	if err != nil {
		t.Fatal(err)
	}
	if got != "v1.2.3" {
		t.Fatalf("got %q, want v1.2.3", got)
	}
}

func TestResolveVersionFromEnv(t *testing.T) {
	gitRepo(t)
	t.Setenv("QIFA_VERSION", "from-env")
	got, err := resolveVersion("", false)
	if err != nil {
		t.Fatal(err)
	}
	if got != "from-env" {
		t.Fatalf("got %q, want from-env", got)
	}
}

func TestResolveVersionFromGit(t *testing.T) {
	sha := gitRepo(t)
	t.Setenv("QIFA_VERSION", "")
	got, err := resolveVersion("", false)
	if err != nil {
		t.Fatal(err)
	}
	if got != sha {
		t.Fatalf("got %q, want %q", got, sha)
	}
}

func TestResolveVersionRefusesDirtyCheckout(t *testing.T) {
	gitRepo(t)
	t.Setenv("QIFA_VERSION", "")
	if err := os.WriteFile("untracked.txt", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := resolveVersion("", false); err == nil {
		t.Fatal("expected a dirty checkout to be refused")
	} else if !strings.Contains(err.Error(), "uncommitted changes") {
		t.Fatalf("unexpected error: %v", err)
	}

	got, err := resolveVersion("", true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, "-dirty") {
		t.Fatalf("got %q, want a -dirty suffix", got)
	}
}

func TestResolveVersionWithoutGitFallsBackToTimestamp(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("QIFA_VERSION", "")
	// A temp dir is not necessarily outside a repo on every machine, so only
	// assert we get something non-empty and never an error.
	got, err := resolveVersion("", false)
	if err != nil && !strings.Contains(err.Error(), "uncommitted changes") {
		t.Fatal(err)
	}
	if err == nil && got == "" {
		t.Fatal("want a version, got empty")
	}
}
