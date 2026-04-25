package secrets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRender(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://db\nline2")
	out, err := Render(map[string]string{"APP_ENV": "production"}, []string{"DATABASE_URL"}, "")
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if got != "APP_ENV=production\nDATABASE_URL=postgres://db\\nline2\n" {
		t.Fatalf("unexpected env file: %q", got)
	}
}

func TestRenderSecretCommand(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-sops.sh")
	body := `#!/bin/sh
cat <<EOF
# a comment
DATABASE_URL=postgres://from-sops
API_KEY="quoted-value"

REDIS_URL=redis://from-sops
EOF
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := Render(
		map[string]string{"APP_ENV": "production"},
		nil,
		script,
	)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, want := range []string{
		"APP_ENV=production\n",
		"API_KEY=quoted-value\n",
		"DATABASE_URL=postgres://from-sops\n",
		"REDIS_URL=redis://from-sops\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected env file to contain %q, got %q", want, got)
		}
	}
}

func TestRenderSecretCommandPrecedence(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 'APP_ENV=from-command'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := Render(map[string]string{"APP_ENV": "from-clear"}, nil, script)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "APP_ENV=from-command\n" {
		t.Fatalf("expected secret_command to override clear, got %q", string(out))
	}
}

func TestRenderSecretCommandFailure(t *testing.T) {
	_, err := Render(nil, nil, "exit 1")
	if err == nil {
		t.Fatal("expected error for failing secret_command")
	}
}

func TestRenderSecretCommandMalformedOutput(t *testing.T) {
	_, err := Render(nil, nil, "echo 'no equals here'")
	if err == nil {
		t.Fatal("expected error for malformed output")
	}
}
