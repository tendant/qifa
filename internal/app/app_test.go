package app

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunNoArgsPrintsUsage(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	if err := Run(context.Background(), nil, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	// Assert the shape, not the exact wording: the usage line has already
	// gained a "-c config.yaml" since this test was written, and a cosmetic
	// tweak should not fail the suite. What matters is that no-args prints
	// usage and lists the verbs.
	out := stdout.String()
	if !strings.HasPrefix(out, "usage: qifa") {
		t.Fatalf("expected a usage line, got: %q", out)
	}
	for _, verb := range []string{"deploy", "rollback", "doctor", "proxy"} {
		if !strings.Contains(out, "\n  "+verb) {
			t.Errorf("usage does not list %q:\n%s", verb, out)
		}
	}
}

func TestRunInitWritesSample(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "qifa.yaml")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := Run(context.Background(), []string{"init", path}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "wrote starter config") {
		t.Fatalf("unexpected stdout: %q", stdout.String())
	}
}

func TestRunUnknownCommand(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	err := Run(context.Background(), []string{"nope"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(stdout.String(), "commands:") {
		t.Fatalf("unexpected stdout: %q", stdout.String())
	}
}
