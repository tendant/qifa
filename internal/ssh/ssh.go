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
	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

type Client struct {
	cfg     config.SSH
	timeout time.Duration
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
			return "", fmt.Errorf("ssh %s: %w: %s", host, err, strings.TrimSpace(stderr.String()))
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
			return fmt.Errorf("upload %s:%s: %w", host, path, err)
		}
		return nil
	}
}

func (c *Client) dial(host string) (*gossh.Client, error) {
	signer, err := privateKey(c.cfg.Key)
	if err != nil {
		return nil, err
	}
	hostKeyCallback, err := knownHostsCallback()
	if err != nil {
		return nil, err
	}
	clientCfg := &gossh.ClientConfig{
		User:            c.cfg.User,
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(signer)},
		HostKeyCallback: hostKeyCallback,
		Timeout:         c.timeout,
	}
	addr := host
	if !strings.Contains(host, ":") {
		addr = net.JoinHostPort(host, "22")
	}
	return gossh.Dial("tcp", addr, clientCfg)
}

func privateKey(path string) (gossh.Signer, error) {
	data, err := os.ReadFile(expandHome(path))
	if err != nil {
		return nil, fmt.Errorf("read ssh key: %w", err)
	}
	return gossh.ParsePrivateKey(data)
}

func knownHostsCallback() (gossh.HostKeyCallback, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return knownhosts.New(filepath.Join(home, ".ssh", "known_hosts"))
}

func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	return path
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func shellDir(path string) string {
	return shellQuote(filepath.Dir(path))
}
