package ssh

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
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

func (c *Client) Run(ctx context.Context, host, command string) (string, error) {
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
		_ = sess.Signal(gossh.SIGKILL)
		return "", ctx.Err()
	case err := <-done:
		if err != nil {
			return "", formatRemoteError("remote command", host, command, err, stderr.String())
		}
		return strings.TrimSpace(stdout.String()), nil
	}
}

func (c *Client) Upload(ctx context.Context, host, path string, contents []byte, mode os.FileMode) error {
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
	return gossh.Dial("tcp", addr, clientCfg)
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
	if len(resolved.identityFiles) > 0 {
		signers, err := signersFromFiles(resolved.identityFiles)
		if err != nil {
			return nil, err
		}
		if len(signers) > 0 {
			methods = append(methods, gossh.PublicKeys(signers...))
		}
	}
	if agentMethod, err := agentAuthMethod(); err == nil {
		methods = append(methods, agentMethod)
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
			if os.IsNotExist(err) || strings.Contains(err.Error(), "no such file") {
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
			if os.IsNotExist(err) || strings.Contains(err.Error(), "no such file") {
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

func formatRemoteError(op, host, command string, err error, stderr string) error {
	stderr = strings.TrimSpace(stderr)
	command = strings.TrimSpace(command)
	if stderr != "" {
		return fmt.Errorf("%s %s failed\ncommand: %s\nerror: %v\nstderr: %s", op, host, command, err, stderr)
	}
	return fmt.Errorf("%s %s failed\ncommand: %s\nerror: %v", op, host, command, err)
}
