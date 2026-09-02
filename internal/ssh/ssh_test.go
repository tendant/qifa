package ssh

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gokamal/gocart/internal/config"
	gossh "golang.org/x/crypto/ssh"
)

func TestFormatRemoteErrorUsesRemoteCommandLabel(t *testing.T) {
	err := formatRemoteError("remote command", "example-host", "echo test", errors.New("exit status 1"), "boom")
	message := err.Error()

	if !strings.Contains(message, "remote command example-host failed") {
		t.Fatalf("expected remote command label, got %q", message)
	}
	if strings.Contains(message, "ssh example-host failed") {
		t.Fatalf("did not expect ssh label, got %q", message)
	}
}

func TestRemoteErrorCarriesOutputForClassification(t *testing.T) {
	err := formatRemoteError("remote command", "10.0.0.11", "docker pull app:v1",
		&gossh.ExitError{Waitmsg: gossh.Waitmsg{}},
		`Error response from daemon: dial tcp: lookup ghcr.io: no such host`)

	var remote *RemoteError
	if !errors.As(err, &remote) {
		t.Fatalf("expected a *RemoteError, got %T", err)
	}
	if remote.RemoteOutput() == "" {
		t.Fatal("captured output must be reachable for classification")
	}
	// dockererr matches on this accessor; keep them in sync.
	var carrier interface{ RemoteOutput() string } = remote
	if !strings.Contains(carrier.RemoteOutput(), "no such host") {
		t.Fatalf("unexpected output: %q", carrier.RemoteOutput())
	}
	if !strings.Contains(err.Error(), "exit status 0") {
		t.Fatalf("expected the exit status in the message, got %q", err.Error())
	}
}

func TestRemoteErrorTruncatesLongCommandAndKeepsOutputTail(t *testing.T) {
	longCommand := strings.Repeat("x", 1000)
	longOutput := strings.Repeat("progress\n", 500) + "the actual failure"
	err := formatRemoteError("remote command", "host", longCommand, errors.New("boom"), longOutput)

	message := err.Error()
	if len(message) > 4000 {
		t.Fatalf("message should stay bounded, got %d bytes", len(message))
	}
	if !strings.Contains(message, "the actual failure") {
		t.Fatalf("the tail of the output must survive:\n%s", message)
	}
}

func TestTailBufferKeepsOnlyTheTail(t *testing.T) {
	buf := newTailBuffer(10)
	if _, err := buf.Write([]byte("0123456789abcdef")); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "6789abcdef" {
		t.Fatalf("got %q, want %q", got, "6789abcdef")
	}
}

func TestDialErrorAddsHostContextAndHint(t *testing.T) {
	resolved := resolvedConfig{
		alias:               "web1",
		hostname:            "10.0.0.11",
		port:                "22",
		user:                "deploy",
		userKnownHostsFiles: []string{"/home/me/.ssh/known_hosts"},
	}
	err := dialError(resolved, "10.0.0.11:22", errors.New("ssh: handshake failed: knownhosts: key mismatch"))
	message := err.Error()
	for _, want := range []string{"deploy@10.0.0.11:22", `alias "web1"`, "ssh-keygen -R 10.0.0.11", "/home/me/.ssh/known_hosts"} {
		if !strings.Contains(message, want) {
			t.Errorf("message is missing %q:\n%s", want, message)
		}
	}
}

// The suggested probe has to be a command that actually runs: user@host:port
// is qifa's notation for a target, but ssh wants -p for the port.
func TestDialErrorSuggestsAValidSSHCommand(t *testing.T) {
	resolved := resolvedConfig{hostname: "10.0.0.11", port: "2222", user: "deploy"}
	err := dialError(resolved, "10.0.0.11:2222", errors.New("ssh: unable to authenticate, no supported methods remain"))
	message := err.Error()
	if !strings.Contains(message, "ssh -v deploy@10.0.0.11 -p 2222") {
		t.Fatalf("expected a runnable ssh command:\n%s", message)
	}
	if strings.Contains(message, "ssh -v deploy@10.0.0.11:2222") {
		t.Fatalf("user@host:port is not valid ssh syntax:\n%s", message)
	}
}

func TestDialErrorOmitsThePortFlagForPort22(t *testing.T) {
	resolved := resolvedConfig{hostname: "10.0.0.11", port: "22", user: "deploy"}
	err := dialError(resolved, "10.0.0.11:22", errors.New("ssh: unable to authenticate, no supported methods remain"))
	if !strings.Contains(err.Error(), "ssh -v deploy@10.0.0.11)") {
		t.Fatalf("expected a bare ssh command for the default port:\n%s", err.Error())
	}
}

func TestDialErrorWithoutAKnownHintStillNamesTheTarget(t *testing.T) {
	resolved := resolvedConfig{hostname: "10.0.0.11", port: "22", user: "deploy"}
	err := dialError(resolved, "10.0.0.11:22", errors.New("something odd"))
	if !strings.Contains(err.Error(), "ssh connect to deploy@10.0.0.11:22 failed") {
		t.Fatalf("unexpected message: %s", err.Error())
	}
}

func TestLocalTransportRunsCommandsWithoutSSH(t *testing.T) {
	client := New(config.SSH{})

	out, err := client.Run(context.Background(), LocalHost, "echo hello")
	if err != nil {
		t.Fatalf("local run failed: %v", err)
	}
	if out != "hello" {
		t.Fatalf("got %q, want %q", out, "hello")
	}
}

func TestLocalTransportCapturesFailureOutputAndExitStatus(t *testing.T) {
	client := New(config.SSH{})

	_, err := client.Run(context.Background(), LocalHost, "echo boom >&2; exit 3")
	if err == nil {
		t.Fatal("expected an error")
	}
	var remote *RemoteError
	if !errors.As(err, &remote) {
		t.Fatalf("expected *RemoteError, got %T", err)
	}
	if remote.ExitStatus != 3 {
		t.Fatalf("got exit status %d, want 3", remote.ExitStatus)
	}
	if remote.RemoteOutput() != "boom" {
		t.Fatalf("got output %q, want %q", remote.RemoteOutput(), "boom")
	}
	// The host is the sentinel, so the message must not read "... local failed".
	if strings.Contains(remote.Error(), "local command local failed") {
		t.Fatalf("awkward message: %s", remote.Error())
	}
}

func TestLocalTransportStreamsOutputLive(t *testing.T) {
	client := New(config.SSH{})

	var out bytes.Buffer
	if err := client.Stream(context.Background(), LocalHost, "echo one; echo two >&2", &out); err != nil {
		t.Fatalf("local stream failed: %v", err)
	}
	for _, want := range []string{"one", "two"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stream output missing %q: %q", want, out.String())
		}
	}
}

func TestLocalTransportStreamFailureCarriesTheTail(t *testing.T) {
	client := New(config.SSH{})

	var out bytes.Buffer
	err := client.Stream(context.Background(), LocalHost, "echo progress; echo 'no such host' >&2; exit 1", &out)
	if err == nil {
		t.Fatal("expected an error")
	}
	var remote *RemoteError
	if !errors.As(err, &remote) || !strings.Contains(remote.RemoteOutput(), "no such host") {
		t.Fatalf("stream failures must carry their output for classification: %v", err)
	}
}

func TestLocalTransportUploadAndDownload(t *testing.T) {
	client := New(config.SSH{})
	path := filepath.Join(t.TempDir(), "nested", "app.env")

	if err := client.Upload(context.Background(), LocalHost, path, []byte("SECRET=1\n"), 0o600); err != nil {
		t.Fatalf("upload failed: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("got mode %v, want 0600", info.Mode().Perm())
	}

	// Re-uploading must reapply the mode: os.WriteFile only sets it on create,
	// and env files carrying secrets must not stay world-readable.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := client.Upload(context.Background(), LocalHost, path, []byte("SECRET=2\n"), 0o600); err != nil {
		t.Fatalf("re-upload failed: %v", err)
	}
	if info, err = os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("got mode %v, want 0600", info.Mode().Perm())
	}

	var buf bytes.Buffer
	if err := client.Download(context.Background(), LocalHost, path, &buf); err != nil {
		t.Fatalf("download failed: %v", err)
	}
	if buf.String() != "SECRET=2\n" {
		t.Fatalf("got %q", buf.String())
	}
}

func TestLocalTransportIsOptInByExactName(t *testing.T) {
	for _, host := range []string{"localhost", "127.0.0.1", "127.0.0.1:2222", "local.example.com", ""} {
		if IsLocal(host) {
			t.Errorf("%q must not be treated as the local transport", host)
		}
	}
	if !IsLocal("local") {
		t.Error(`"local" should be the local transport`)
	}
}
