package ssh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gokamal/gocart/internal/config"
	ksshconfig "github.com/kevinburke/ssh_config"
	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

type Client struct {
	cfg     config.SSH
	timeout time.Duration
}

type resolvedConfig struct {
	alias                 string
	hostname              string
	port                  string
	user                  string
	identityFiles         []string
	strictHostKeyChecking bool
	userKnownHostsFiles   []string
}

func New(cfg config.SSH) *Client {
	return &Client{cfg: cfg, timeout: 30 * time.Second}
}

// LocalHost is the reserved host name that means "the machine qifa is running
// on, without SSH". Deploying to yourself over SSH to 127.0.0.1 works, but it
// demands sshd, an authorized key and a known_hosts entry for a machine you
// are already standing on. `hosts: [local]` skips all of that and runs the
// same commands through /bin/sh.
//
// It is opt-in by exact name only. 127.0.0.1 and localhost keep meaning SSH,
// because targeting a local SSH daemon (a VM, a container, the e2e harness on
// 127.0.0.1:2222) is a legitimate and different thing.
const LocalHost = "local"

// IsLocal reports whether host is the reserved local sentinel.
func IsLocal(host string) bool { return host == LocalHost }

func (c *Client) Run(ctx context.Context, host, command string) (string, error) {
	if IsLocal(host) {
		return c.runLocal(ctx, command)
	}
	conn, err := c.dial(host)
	if err != nil {
		return "", err
	}
	defer conn.Close()

	sess, err := conn.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	sess.Stdout = &stdout
	sess.Stderr = &stderr

	done := make(chan error, 1)
	go func() { done <- sess.Run(command) }()

	select {
	case <-ctx.Done():
		conn.Close() // force TCP teardown; unblocks sess.Run() goroutine and deferred sess.Close()
		return "", ctx.Err()
	case err := <-done:
		if err != nil {
			// Fall back to stdout when the command said nothing on stderr —
			// shell pipelines and `docker ... || echo` idioms often report the
			// real problem there.
			output := stderr.String()
			if strings.TrimSpace(output) == "" {
				output = stdout.String()
			}
			return "", formatRemoteError("remote command", host, command, err, output)
		}
		return strings.TrimSpace(stdout.String()), nil
	}
}

// Stream runs command on host and pipes its stdout and stderr live to out
// instead of buffering. Use for long-running commands (docker build, push,
// pull) where the user benefits from seeing progress.
func (c *Client) Stream(ctx context.Context, host, command string, out io.Writer) error {
	if IsLocal(host) {
		return c.streamLocal(ctx, command, out)
	}
	conn, err := c.dial(host)
	if err != nil {
		return err
	}
	defer conn.Close()

	sess, err := conn.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()

	// Keep a bounded tail of everything the command printed, not just stderr:
	// docker reports some failures on stdout, and when a long pull dies the
	// last few lines are what explains it. The tail is what gets attached to
	// the error and, from there, classified by internal/dockererr.
	combined := newTailBuffer(tailBytes)
	// stdout and stderr are copied by separate goroutines, so both must go
	// through one lock-protected writer — two independent MultiWriters over
	// the same destination race, and interleaved writes lose output.
	merged := newSyncWriter(io.MultiWriter(out, combined))
	sess.Stdout = merged
	sess.Stderr = merged

	done := make(chan error, 1)
	go func() { done <- sess.Run(command) }()

	select {
	case <-ctx.Done():
		conn.Close() // force TCP teardown; unblocks sess.Run() goroutine and deferred sess.Close()
		return ctx.Err()
	case err := <-done:
		if err != nil {
			return formatRemoteError("remote command", host, command, err, combined.String())
		}
		return nil
	}
}

// Download streams the contents of a remote file to dst. Uses `cat <path>` so
// it works with any user that can read the file (no sftp subsystem needed).
// Designed for binary blobs (backup artifacts, etc.) — runs in constant
// memory regardless of file size since both ends are streaming pipes.
func (c *Client) Download(ctx context.Context, host, path string, dst io.Writer) error {
	if IsLocal(host) {
		file, err := os.Open(path)
		if err != nil {
			return &RemoteError{Op: "download", Host: LocalHost, Command: "open " + path, ExitStatus: -1, Err: err}
		}
		defer file.Close()
		_, err = io.Copy(dst, file)
		return err
	}
	conn, err := c.dial(host)
	if err != nil {
		return err
	}
	defer conn.Close()

	sess, err := conn.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()

	var stderr bytes.Buffer
	sess.Stdout = dst
	sess.Stderr = &stderr

	command := "cat " + shellQuote(path)
	done := make(chan error, 1)
	go func() { done <- sess.Run(command) }()

	select {
	case <-ctx.Done():
		_ = sess.Signal(gossh.SIGKILL)
		return ctx.Err()
	case err := <-done:
		if err != nil {
			return formatRemoteError("download", host, command, err, stderr.String())
		}
		return nil
	}
}

func (c *Client) Upload(ctx context.Context, host, path string, contents []byte, mode os.FileMode) error {
	if IsLocal(host) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return &RemoteError{Op: "upload", Host: LocalHost, Command: "mkdir -p " + filepath.Dir(path), ExitStatus: -1, Err: err}
		}
		if err := os.WriteFile(path, contents, mode.Perm()); err != nil {
			return &RemoteError{Op: "upload", Host: LocalHost, Command: "write " + path, ExitStatus: -1, Err: err}
		}
		// WriteFile only applies mode when it creates the file; an existing
		// one keeps its old permissions, which matters for 0600 env files.
		return os.Chmod(path, mode.Perm())
	}
	command := fmt.Sprintf("umask 077 && mkdir -p %s && cat > %s && chmod %o %s", shellDir(path), shellQuote(path), mode.Perm(), shellQuote(path))
	conn, err := c.dial(host)
	if err != nil {
		return err
	}
	defer conn.Close()

	sess, err := conn.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()

	sess.Stdin = bytes.NewReader(contents)
	done := make(chan error, 1)
	go func() { done <- sess.Run(command) }()

	select {
	case <-ctx.Done():
		_ = sess.Signal(gossh.SIGKILL)
		return ctx.Err()
	case err := <-done:
		if err != nil {
			return formatRemoteError("upload", host, command, err, "")
		}
		return nil
	}
}

func (c *Client) dial(host string) (*gossh.Client, error) {
	resolved, err := c.resolve(host)
	if err != nil {
		return nil, err
	}
	authMethods, err := c.authMethods(resolved)
	if err != nil {
		return nil, err
	}
	hostKeyCallback := gossh.InsecureIgnoreHostKey()
	if resolved.strictHostKeyChecking {
		hostKeyCallback, err = knownHostsCallback(resolved.userKnownHostsFiles)
		if err != nil {
			return nil, err
		}
	}
	clientCfg := &gossh.ClientConfig{
		User:            resolved.user,
		Auth:            authMethods,
		HostKeyCallback: hostKeyCallback,
		Timeout:         c.timeout,
	}
	addr := net.JoinHostPort(resolved.hostname, resolved.port)
	conn, err := gossh.Dial("tcp", addr, clientCfg)
	if err != nil {
		return nil, dialError(resolved, addr, err)
	}
	return conn, nil
}

// dialError turns golang.org/x/crypto/ssh's terse dial failures into something
// that names the host, user and port qifa actually used, plus the usual fix.
// "ssh: handshake failed: knownhosts: key mismatch" with no mention of which
// host or which key file is the single most-reported qifa confusion.
func dialError(resolved resolvedConfig, addr string, err error) error {
	target := fmt.Sprintf("%s@%s", resolved.user, addr)
	if resolved.alias != "" && resolved.alias != resolved.hostname {
		target += fmt.Sprintf(" (from ssh_config alias %q)", resolved.alias)
	}
	// A command the operator can paste. `user@host:port` is qifa's notation,
	// not ssh's — ssh takes the port as a separate -p flag.
	probe := fmt.Sprintf("ssh -v %s@%s", resolved.user, resolved.hostname)
	if resolved.port != "" && resolved.port != "22" {
		probe += " -p " + resolved.port
	}
	msg := strings.ToLower(err.Error())
	hint := ""
	switch {
	case strings.Contains(msg, "knownhosts: key mismatch"):
		hint = fmt.Sprintf("the host key changed or a stale entry exists in %s\n"+
			"  fix: ssh-keygen -R %s && ssh-keyscan -H %s >> %s\n"+
			"  (or set ssh.strict_host_key_checking: false for trusted local hosts)",
			strings.Join(resolved.userKnownHostsFiles, ", "), resolved.hostname, resolved.hostname,
			firstOr(resolved.userKnownHostsFiles, "~/.ssh/known_hosts"))
	case strings.Contains(msg, "knownhosts: key is unknown") || strings.Contains(msg, "key is unknown"):
		hint = fmt.Sprintf("this host is not in %s yet\n"+
			"  fix: ssh-keyscan -H %s >> %s",
			strings.Join(resolved.userKnownHostsFiles, ", "), resolved.hostname,
			firstOr(resolved.userKnownHostsFiles, "~/.ssh/known_hosts"))
	case strings.Contains(msg, "unable to authenticate"), strings.Contains(msg, "no supported methods remain"):
		hint = "no accepted key. Check ssh.user, ssh.key, and that the public key is in\n" +
			"  the remote user's ~/.ssh/authorized_keys (try: " + probe + ")"
	case strings.Contains(msg, "connection refused"):
		hint = "nothing is listening on that port — is sshd running, and is the port right?"
	case strings.Contains(msg, "i/o timeout"), strings.Contains(msg, "deadline exceeded"):
		hint = "no response within 30s — firewall/security group, or the host is down"
	case strings.Contains(msg, "no such host"):
		hint = "the hostname does not resolve from this machine"
	}
	if hint == "" {
		return fmt.Errorf("ssh connect to %s failed: %w", target, err)
	}
	return fmt.Errorf("ssh connect to %s failed: %w\n  %s", target, err, hint)
}

func firstOr(values []string, fallback string) string {
	if len(values) > 0 {
		return values[0]
	}
	return fallback
}

func (c *Client) authMethods(resolved resolvedConfig) ([]gossh.AuthMethod, error) {
	if c.cfg.Key != "" {
		signer, err := privateKey(c.cfg.Key)
		if err != nil {
			return nil, err
		}
		return []gossh.AuthMethod{gossh.PublicKeys(signer)}, nil
	}

	var methods []gossh.AuthMethod
	if agentMethod, err := agentAuthMethod(); err == nil {
		methods = append(methods, agentMethod)
	}
	if len(resolved.identityFiles) > 0 {
		signers, err := signersFromFiles(resolved.identityFiles)
		if err != nil {
			return nil, err
		}
		if len(signers) > 0 {
			methods = append(methods, gossh.PublicKeys(signers...))
		}
	}
	signers, err := defaultKeySigners()
	if err != nil {
		return nil, err
	}
	if len(signers) > 0 {
		methods = append(methods, gossh.PublicKeys(signers...))
	}
	if len(methods) == 0 {
		return nil, fmt.Errorf("no SSH auth available: set config.ssh.key, configure SSH agent, or provide a default identity file")
	}
	return methods, nil
}

func (c *Client) resolve(host string) (resolvedConfig, error) {
	alias, portFromHost := splitHostPort(host)
	hostname, err := sshConfigValue(alias, "HostName")
	if err != nil {
		return resolvedConfig{}, err
	}
	if hostname == "" {
		hostname = alias
	}
	port := portFromHost
	if port == "" {
		port, err = sshConfigValue(alias, "Port")
		if err != nil {
			return resolvedConfig{}, err
		}
	}
	if port == "" {
		port = "22"
	}
	user := c.cfg.User
	if user == "" {
		user, err = sshConfigValue(alias, "User")
		if err != nil {
			return resolvedConfig{}, err
		}
	}
	if user == "" {
		user = currentUser()
	}
	identityFiles := []string{}
	if c.cfg.Key == "" {
		identityFiles, err = sshConfigValues(alias, "IdentityFile")
		if err != nil {
			return resolvedConfig{}, err
		}
		for i, value := range identityFiles {
			identityFiles[i] = expandHome(value)
		}
	}
	strictHostKeyChecking := true
	if c.cfg.StrictHostKeyChecking != nil {
		strictHostKeyChecking = *c.cfg.StrictHostKeyChecking
	} else {
		value, err := sshConfigValue(alias, "StrictHostKeyChecking")
		if err != nil {
			return resolvedConfig{}, err
		}
		if value != "" {
			strictHostKeyChecking = !strings.EqualFold(value, "no")
		}
	}
	userKnownHostsFiles, err := sshConfigValues(alias, "UserKnownHostsFile")
	if err != nil {
		return resolvedConfig{}, err
	}
	var expandedKnownHosts []string
	for _, value := range userKnownHostsFiles {
		for _, part := range strings.Fields(value) {
			expandedKnownHosts = append(expandedKnownHosts, expandHome(part))
		}
	}
	userKnownHostsFiles = expandedKnownHosts
	if len(userKnownHostsFiles) == 0 {
		home, err := os.UserHomeDir()
		if err != nil {
			return resolvedConfig{}, err
		}
		userKnownHostsFiles = []string{filepath.Join(home, ".ssh", "known_hosts")}
	}
	return resolvedConfig{
		alias:                 alias,
		hostname:              hostname,
		port:                  port,
		user:                  user,
		identityFiles:         identityFiles,
		strictHostKeyChecking: strictHostKeyChecking,
		userKnownHostsFiles:   userKnownHostsFiles,
	}, nil
}

func privateKey(path string) (gossh.Signer, error) {
	data, err := os.ReadFile(expandHome(path))
	if err != nil {
		return nil, fmt.Errorf("read ssh key: %w", err)
	}
	return gossh.ParsePrivateKey(data)
}

func signersFromFiles(paths []string) ([]gossh.Signer, error) {
	var signers []gossh.Signer
	for _, path := range paths {
		signer, err := privateKey(path)
		if err != nil {
			var pmErr *gossh.PassphraseMissingError
			if os.IsNotExist(err) || strings.Contains(err.Error(), "no such file") || errors.As(err, &pmErr) {
				continue
			}
			return nil, err
		}
		signers = append(signers, signer)
	}
	return signers, nil
}

func agentAuthMethod() (gossh.AuthMethod, error) {
	socket := os.Getenv("SSH_AUTH_SOCK")
	if socket == "" {
		return nil, fmt.Errorf("SSH_AUTH_SOCK is not set")
	}
	conn, err := net.Dial("unix", socket)
	if err != nil {
		return nil, err
	}
	return gossh.PublicKeysCallback(agent.NewClient(conn).Signers), nil
}

func defaultKeySigners() ([]gossh.Signer, error) {
	var signers []gossh.Signer
	for _, path := range []string{
		"~/.ssh/id_ed25519",
		"~/.ssh/id_rsa",
		"~/.ssh/id_ecdsa",
		"~/.ssh/id_dsa",
	} {
		signer, err := privateKey(path)
		if err != nil {
			var pmErr *gossh.PassphraseMissingError
			if os.IsNotExist(err) || strings.Contains(err.Error(), "no such file") || errors.As(err, &pmErr) {
				continue
			}
			return nil, err
		}
		signers = append(signers, signer)
	}
	return signers, nil
}

func knownHostsCallback(paths []string) (gossh.HostKeyCallback, error) {
	if len(paths) == 0 {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		paths = []string{filepath.Join(home, ".ssh", "known_hosts")}
	}
	var existing []string
	for _, path := range paths {
		if _, err := os.Stat(path); err == nil {
			existing = append(existing, path)
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	if len(existing) == 0 {
		return nil, fmt.Errorf("no known_hosts files found in %v", paths)
	}
	return knownhosts.New(existing...)
}

func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	return path
}

func currentUser() string {
	if user := os.Getenv("USER"); user != "" {
		return user
	}
	return "root"
}

func splitHostPort(host string) (string, string) {
	name, port, err := net.SplitHostPort(host)
	if err == nil {
		return name, port
	}
	return host, ""
}

func sshConfigValue(alias, key string) (string, error) {
	value, err := ksshconfig.GetStrict(alias, key)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(value), nil
}

func sshConfigValues(alias, key string) ([]string, error) {
	values, err := ksshconfig.GetAllStrict(alias, key)
	if err != nil {
		return nil, err
	}
	filtered := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || value == "none" {
			continue
		}
		filtered = append(filtered, value)
	}
	return filtered, nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func shellDir(path string) string {
	return shellQuote(filepath.Dir(path))
}

// RemoteError is a command that failed on a remote host, with everything
// needed to explain why: the command, its exit status, and a bounded tail of
// what it printed. Callers (internal/dockererr) match on the captured output
// to classify the failure; RemoteOutput is that accessor.
type RemoteError struct {
	Op         string
	Host       string
	Command    string
	ExitStatus int // -1 when the command never produced one (signal, transport error)
	Output     string
	Err        error
}

func (e *RemoteError) Unwrap() error { return e.Err }

// RemoteOutput satisfies dockererr.OutputCarrier.
func (e *RemoteError) RemoteOutput() string { return e.Output }

func (e *RemoteError) Error() string {
	var b strings.Builder
	if e.Host == LocalHost {
		// "local command local failed" — the op already names the transport.
		fmt.Fprintf(&b, "%s failed", e.Op)
	} else {
		fmt.Fprintf(&b, "%s %s failed", e.Op, e.Host)
	}
	if e.ExitStatus >= 0 {
		fmt.Fprintf(&b, " (exit status %d)", e.ExitStatus)
	}
	fmt.Fprintf(&b, "\ncommand: %s", abbreviate(e.Command, 400))
	if e.ExitStatus < 0 {
		fmt.Fprintf(&b, "\nerror: %v", e.Err)
	}
	if out := strings.TrimSpace(e.Output); out != "" {
		fmt.Fprintf(&b, "\noutput: %s", abbreviateTail(out, 2000))
	}
	return b.String()
}

// localShell is the shell used for the local transport. qifa builds POSIX sh
// command lines (&& chains, for loops, $(...)), the same ones sshd would hand
// to the remote user's shell.
const localShell = "/bin/sh"

func (c *Client) runLocal(ctx context.Context, command string) (string, error) {
	cmd := exec.CommandContext(ctx, localShell, "-c", command)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		output := stderr.String()
		if strings.TrimSpace(output) == "" {
			output = stdout.String()
		}
		return "", formatLocalError("local command", command, err, output)
	}
	return strings.TrimSpace(stdout.String()), nil
}

func (c *Client) streamLocal(ctx context.Context, command string, out io.Writer) error {
	combined := newTailBuffer(tailBytes)
	merged := newSyncWriter(io.MultiWriter(out, combined))
	cmd := exec.CommandContext(ctx, localShell, "-c", command)
	cmd.Stdout = merged
	cmd.Stderr = merged
	if err := cmd.Run(); err != nil {
		return formatLocalError("local command", command, err, combined.String())
	}
	return nil
}

func formatLocalError(op, command string, err error, output string) error {
	exitStatus := -1
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		exitStatus = exitErr.ExitCode()
	}
	return &RemoteError{
		Op:         op,
		Host:       LocalHost,
		Command:    strings.TrimSpace(command),
		ExitStatus: exitStatus,
		Output:     strings.TrimSpace(output),
		Err:        err,
	}
}

func formatRemoteError(op, host, command string, err error, output string) error {
	exitStatus := -1
	var exitErr *gossh.ExitError
	if errors.As(err, &exitErr) {
		exitStatus = exitErr.ExitStatus()
	}
	return &RemoteError{
		Op:         op,
		Host:       host,
		Command:    strings.TrimSpace(command),
		ExitStatus: exitStatus,
		Output:     strings.TrimSpace(output),
		Err:        err,
	}
}

func abbreviate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("… (%d more characters)", len(s)-max)
}

// abbreviateTail keeps the END of a long output — the failure is at the
// bottom, the progress bars are at the top.
func abbreviateTail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return fmt.Sprintf("(%d earlier characters omitted)…", len(s)-max) + s[len(s)-max:]
}

// syncWriter serializes writes from the two goroutines that copy a command's
// stdout and stderr.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func newSyncWriter(w io.Writer) *syncWriter { return &syncWriter{w: w} }

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

const tailBytes = 64 << 10

// tailBuffer keeps at most the last n bytes written to it. Pull output can run
// to megabytes of progress lines; only the tail is worth carrying into an
// error.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func newTailBuffer(max int) *tailBuffer { return &tailBuffer{max: max} }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
