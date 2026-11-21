package app

import (
	"bytes"
	"context"
	"os"
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

// init never replaces a config someone has edited unless told to.
func TestRunInitRefusesToOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "qifa.yaml")
	if err := os.WriteFile(path, []byte("service: mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err := Run(context.Background(), []string{"init", path}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("init over an existing file: %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != "service: mine\n" {
		t.Fatalf("the existing file was changed: %q", data)
	}
	if err := Run(context.Background(), []string{"init", "--force", path}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), "service: myapp") {
		t.Fatalf("--force did not write the starter: %q", data)
	}
}

// help, -h and --help print usage instead of failing, for everything or one
// command; an exec's own --help goes to the command it runs.
func TestRunHelp(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"-h"}, {"--help"}} {
		var stdout, stderr bytes.Buffer
		if err := Run(context.Background(), args, &stdout, &stderr); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if !strings.Contains(stdout.String(), "commands:") {
			t.Errorf("%v printed no command list:\n%s", args, stdout.String())
		}
	}
	for _, args := range [][]string{{"deploy", "--help"}, {"help", "deploy"}, {"proxy", "boot", "-h"}} {
		var stdout, stderr bytes.Buffer
		if err := Run(context.Background(), args, &stdout, &stderr); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if !strings.Contains(stdout.String(), "qifa "+args[0]+" ") && !strings.Contains(stdout.String(), "qifa "+args[1]+" ") {
			t.Errorf("%v did not print that command's usage:\n%s", args, stdout.String())
		}
		if strings.Contains(stdout.String(), "commands:") {
			t.Errorf("%v printed the whole list:\n%s", args, stdout.String())
		}
	}
	var stdout, stderr bytes.Buffer
	if err := Run(context.Background(), []string{"help", "nope"}, &stdout, &stderr); err == nil {
		t.Error("help for an unknown command should fail")
	}
	if wantsHelp([]string{"exec", "ls", "--help"}) || wantsHelp([]string{"exec", "redis", "redis-cli", "-h", "db"}) {
		t.Error("an exec's own flags were taken as a request for qifa's help")
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

// --host only picks which host's script --print writes; setup itself runs on
// every host, so --host without --print is refused before anything loads.
func TestSetupHostNeedsPrint(t *testing.T) {
	var out, errOut bytes.Buffer
	err := runSetup(context.Background(), []string{"--host", "h1"}, &out, &errOut, "missing.yaml")
	if err == nil || !strings.Contains(err.Error(), "--host goes with --print") {
		t.Fatalf("got %v", err)
	}
	err = runSetup(context.Background(), []string{"--print", "--bogus"}, &out, &errOut, "missing.yaml")
	if err == nil || !strings.Contains(err.Error(), `unknown argument "--bogus"`) {
		t.Fatalf("got %v", err)
	}
}
