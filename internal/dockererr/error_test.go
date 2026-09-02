package dockererr

import (
	"context"
	"strings"
	"testing"
)

// fakeRunner stands in for an SSH client during the host probe.
type fakeRunner struct {
	out     string
	command string
}

func (f *fakeRunner) Run(ctx context.Context, host, command string) (string, error) {
	f.command = command
	return f.out, nil
}

func TestWrapRendersCauseHintAndDiagnostics(t *testing.T) {
	runner := &fakeRunner{out: strings.Join([]string{
		"docker-cli|ok|Docker version 27.1.1",
		"docker-daemon|ok|server 27.1.1",
		"dns ghcr.io|FAIL|no address (check /etc/resolv.conf and the host's DNS server)",
		"disk|ok|/var/lib/docker: 40G free of 80G",
	}, "\n")}

	inner := outputErr{out: `Error response from daemon: Get "https://ghcr.io/v2/": dial tcp: lookup ghcr.io: no such host`}
	err := Wrap(context.Background(), runner, "pull image", "10.0.0.11", "ghcr.io/acme/app:v1", 3, inner)

	msg := err.Error()
	for _, want := range []string{
		"pull image ghcr.io/acme/app:v1 on 10.0.0.11 failed after 3 attempts",
		"cannot resolve the registry hostname",
		"docker said:",
		"lookup ghcr.io: no such host",
		"what to check:",
		"/etc/resolv.conf",
		"host diagnostics (10.0.0.11)",
		"dns ghcr.io",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message is missing %q:\n%s", want, msg)
		}
	}
	// The hint's <registry> placeholder should be filled in from the image.
	if strings.Contains(msg, "<registry>") {
		t.Errorf("unsubstituted placeholder in message:\n%s", msg)
	}
}

func TestWrapSkipsTheProbeForNonNetworkFailures(t *testing.T) {
	runner := &fakeRunner{out: "docker-cli|ok|x"}
	inner := outputErr{out: "Error response from daemon: manifest unknown"}
	err := Wrap(context.Background(), runner, "pull image", "10.0.0.11", "ghcr.io/acme/app:v9", 1, inner)

	if runner.command != "" {
		t.Fatal("a missing tag is not a connectivity problem; the host probe should be skipped")
	}
	if msg := err.Error(); strings.Contains(msg, "host diagnostics") {
		t.Fatalf("unexpected diagnostics block:\n%s", msg)
	}
}

func TestWrapExplainsUnrecognisedFailures(t *testing.T) {
	err := Wrap(context.Background(), nil, "pull image", "10.0.0.11", "app:v1", 1, outputErr{out: "kaboom"})
	msg := err.Error()
	if !strings.Contains(msg, "did not recognise this failure") {
		t.Fatalf("expected a fallback explanation:\n%s", msg)
	}
	if !strings.Contains(msg, "kaboom") {
		t.Fatalf("expected the raw output to be preserved:\n%s", msg)
	}
}

func TestWrapNilIsNil(t *testing.T) {
	if err := Wrap(context.Background(), nil, "pull image", "h", "i", 1, nil); err != nil {
		t.Fatalf("want nil, got %v", err)
	}
}

func TestErrorKeepsTheTailOfLongOutput(t *testing.T) {
	var lines []string
	for i := 0; i < 200; i++ {
		lines = append(lines, "progress line")
	}
	lines = append(lines, "the actual failure")
	err := Wrap(context.Background(), nil, "pull image", "h", "app:v1", 1, outputErr{out: strings.Join(lines, "\n")})
	msg := err.Error()
	if !strings.Contains(msg, "the actual failure") {
		t.Fatalf("the last line must survive truncation:\n%s", msg)
	}
	if strings.Count(msg, "progress line") > 12 {
		t.Fatalf("output was not truncated:\n%s", msg)
	}
}

func TestDiagnoseParsesChecksAndClockSkew(t *testing.T) {
	runner := &fakeRunner{out: strings.Join([]string{
		"docker-cli|ok|Docker version 27.1.1",
		"clock|info|100", // 1970 — a huge skew
		"malformed line without separators",
	}, "\n")}
	report := Diagnose(context.Background(), runner, "10.0.0.11", "ghcr.io/acme/app:v1")

	if len(report.Checks) != 2 {
		t.Fatalf("want 2 parsed checks, got %d: %+v", len(report.Checks), report.Checks)
	}
	clock := report.Checks[1]
	if clock.Status != "FAIL" || !strings.Contains(clock.Detail, "skew") {
		t.Fatalf("a 1970 clock should fail with a skew note, got %+v", clock)
	}
	if !report.Failed() {
		t.Fatal("report with a FAIL check should report Failed()")
	}
}

func TestWrapKnownNetworkProbesOnlyRecognisedNetworkFaults(t *testing.T) {
	// A build that failed on the user's own Dockerfile: no probe.
	runner := &fakeRunner{}
	err := WrapKnownNetwork(context.Background(), runner, "build image", "10.0.0.11", "app:v1", 1,
		outputErr{out: "COPY failed: file not found in build context"})
	if runner.command != "" {
		t.Fatal("an unrecognised build failure should not trigger a host probe")
	}
	if !strings.Contains(err.Error(), "qifa doctor") {
		t.Fatalf("unrecognised failures should point at doctor:\n%s", err.Error())
	}

	// A build that failed pulling its base image: probe.
	runner = &fakeRunner{out: "docker-cli|ok|x"}
	WrapKnownNetwork(context.Background(), runner, "build image", "10.0.0.11", "app:v1", 1,
		outputErr{out: `failed to resolve source: dial tcp: lookup docker.io: no such host`})
	if runner.command == "" {
		t.Fatal("a DNS failure during a build should still be diagnosed")
	}
}
