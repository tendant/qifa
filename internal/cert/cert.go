// Package cert manages TLS certs that are issued, renewed, and served
// out of kamal-proxy's named state volume — the certs that a per-app
// `proxy.tls.source: qifa` route consumes.
//
// All cert work runs as one-shot containers on the proxy host (lego or
// alpine), mounting kamal-proxy-config:/state. Lego writes to
// <volume>/qifa/certificates/<host>.{crt,key}; kamal-proxy reads the
// same file at /home/kamal-proxy/.config/kamal-proxy/qifa/certificates/...
// (see proxy.QifaCertPaths).
//
// Provider credentials never land on the host disk: they're pushed to
// /dev/shm (tmpfs) with mode 0600, fed to lego via --env-file, and
// removed on exit.
package cert

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/gokamal/gocart/internal/ssh"
)

const (
	// LegoImage is the lego container image used for cert acquisition
	// and renewal. Pinned to :latest by default; override per-Manager
	// for reproducibility.
	LegoImage = "goacme/lego:latest"

	// AlpineImage is the helper image used for filesystem operations
	// inside the proxy volume (listing certs, removing files).
	AlpineImage = "alpine:latest"

	// ProxyVolume is the named docker volume kamal-proxy mounts as its
	// state directory. Must match what `qifa proxy boot` creates.
	ProxyVolume = "kamal-proxy-config"

	// LegoMountPoint is where ProxyVolume is mounted inside the lego
	// (and alpine helper) container.
	LegoMountPoint = "/state"

	// LegoSubdir is the path under LegoMountPoint that qifa hands to
	// lego's --path. Keeps qifa-managed certs in a separate subtree
	// from kamal-proxy's autocert cache (which lives in <volume>/certs).
	LegoSubdir = "qifa"
)

// Manager runs cert ops against a kamal-proxy host over SSH.
type Manager struct {
	ssh        *ssh.Client
	proxyHost  string
	legoImage  string
	alpineImg  string
	volumeName string
	subdir     string
	out        io.Writer
}

// Options configure a Manager. Empty fields fall back to package defaults.
type Options struct {
	LegoImage   string
	AlpineImage string
	VolumeName  string
	Subdir      string
}

// New creates a Manager bound to the given SSH client and proxy host.
func New(client *ssh.Client, proxyHost string, out io.Writer, opts Options) *Manager {
	m := &Manager{
		ssh:        client,
		proxyHost:  proxyHost,
		legoImage:  LegoImage,
		alpineImg:  AlpineImage,
		volumeName: ProxyVolume,
		subdir:     LegoSubdir,
		out:        out,
	}
	if opts.LegoImage != "" {
		m.legoImage = opts.LegoImage
	}
	if opts.AlpineImage != "" {
		m.alpineImg = opts.AlpineImage
	}
	if opts.VolumeName != "" {
		m.volumeName = opts.VolumeName
	}
	if opts.Subdir != "" {
		m.subdir = opts.Subdir
	}
	return m
}

// IssueOptions controls a single cert acquisition.
type IssueOptions struct {
	// Host is the FQDN to issue the cert for (also the cert filename).
	Host string

	// Email is registered with the ACME directory.
	Email string

	// Provider is the lego DNS provider name (cloudflare, route53, …).
	// Required when Challenge is dns-01. Full list:
	// https://go-acme.github.io/lego/dns/
	Provider string

	// Challenge selects the ACME challenge type. "dns-01" (default) or
	// "http-01". For now only dns-01 is implemented for qifa-managed
	// certs — http-01 conflicts with kamal-proxy's port 80 binding,
	// so use source: kamal for that case instead.
	Challenge string

	// Staging requests Let's Encrypt's staging environment (avoids
	// production rate limits).
	Staging bool

	// EnvVars are passed through to the lego container (for the DNS
	// provider's API credentials). Pushed to /dev/shm on the proxy
	// host with mode 0600 and removed on exit — never persisted.
	EnvVars map[string]string
}

// Issue acquires a fresh cert and writes it into the proxy volume.
func (m *Manager) Issue(ctx context.Context, opts IssueOptions) error {
	return m.runLego(ctx, "run", opts, nil)
}

// Renew refreshes a cert if it's within `days` of expiring. Returns
// without error if the cert isn't due for renewal yet.
func (m *Manager) Renew(ctx context.Context, opts IssueOptions, days int) error {
	extra := []string{}
	if days > 0 {
		extra = append(extra, "--days", fmt.Sprintf("%d", days))
	}
	return m.runLego(ctx, "renew", opts, extra)
}

// RenewAll iterates every cert currently in the proxy volume and runs
// Renew on each. Returns the first error but continues past failures
// so one expired-account cert can't block the others.
func (m *Manager) RenewAll(ctx context.Context, opts IssueOptions, days int) error {
	hosts, err := m.List(ctx)
	if err != nil {
		return err
	}
	if len(hosts) == 0 {
		fmt.Fprintln(m.out, "no certs to renew")
		return nil
	}
	var firstErr error
	for _, h := range hosts {
		fmt.Fprintf(m.out, "==> %s\n", h)
		hostOpts := opts
		hostOpts.Host = h
		if err := m.Renew(ctx, hostOpts, days); err != nil && firstErr == nil {
			firstErr = err
			fmt.Fprintf(m.out, "  (renew failed for %s: %v — continuing)\n", h, err)
		}
	}
	return firstErr
}

// List returns the FQDNs that currently have a cert+key pair in the
// proxy volume.
func (m *Manager) List(ctx context.Context) ([]string, error) {
	cmd := fmt.Sprintf(
		"docker run --rm -v %s:%s %s sh -c %s",
		shellQuote(m.volumeName),
		shellQuote(m.mountPoint()),
		shellQuote(m.alpineImg),
		shellQuote(fmt.Sprintf(
			"ls %s/certificates/*.crt 2>/dev/null | sed -e 's|.*/||' -e 's|\\.crt$||' || true",
			m.subdirPath(),
		)),
	)
	out, err := m.ssh.Run(ctx, m.proxyHost, cmd)
	if err != nil {
		return nil, err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, nil
	}
	hosts := strings.Split(out, "\n")
	for i, h := range hosts {
		hosts[i] = strings.TrimSpace(h)
	}
	sort.Strings(hosts)
	return hosts, nil
}

// Remove deletes <host>.crt and <host>.key from the proxy volume.
// Does not redeploy or update kamal-proxy state — the user is
// responsible for redeploying any app that referenced this cert.
func (m *Manager) Remove(ctx context.Context, host string) error {
	if host == "" {
		return fmt.Errorf("host is required")
	}
	cmd := fmt.Sprintf(
		"docker run --rm -v %s:%s %s sh -c %s",
		shellQuote(m.volumeName),
		shellQuote(m.mountPoint()),
		shellQuote(m.alpineImg),
		shellQuote(fmt.Sprintf(
			"rm -f %s/certificates/%s.crt %s/certificates/%s.key",
			m.subdirPath(), host, m.subdirPath(), host,
		)),
	)
	_, err := m.ssh.Run(ctx, m.proxyHost, cmd)
	return err
}

// runLego pushes credentials (if any) to /dev/shm on the proxy host,
// runs lego in a one-shot container, and cleans up on exit.
func (m *Manager) runLego(ctx context.Context, action string, opts IssueOptions, extra []string) error {
	if opts.Host == "" {
		return fmt.Errorf("host is required")
	}
	if opts.Email == "" {
		return fmt.Errorf("email is required")
	}
	challenge := opts.Challenge
	if challenge == "" {
		challenge = "dns-01"
	}
	if challenge != "dns-01" {
		return fmt.Errorf("challenge %q is not supported for source: qifa (only dns-01); use proxy.tls.source: kamal for HTTP-01", challenge)
	}
	if opts.Provider == "" {
		return fmt.Errorf("provider is required for dns-01 challenge")
	}

	envFileFlag, cleanup, err := m.pushEnv(ctx, opts.EnvVars)
	if err != nil {
		return err
	}
	defer cleanup()

	args := []string{
		"docker run --rm",
		envFileFlag,
		"-v " + shellQuote(m.volumeName) + ":" + shellQuote(m.mountPoint()),
		shellQuote(m.legoImage),
		"--dns " + shellQuote(opts.Provider),
		"--email " + shellQuote(opts.Email),
		"--domains " + shellQuote(opts.Host),
		"--path " + shellQuote(m.subdirPath()),
		"--accept-tos",
	}
	if opts.Staging {
		args = append(args, "--server", shellQuote("https://acme-staging-v02.api.letsencrypt.org/directory"))
	}
	args = append(args, action)
	for _, e := range extra {
		args = append(args, shellQuote(e))
	}
	cmd := strings.Join(args, " ")
	return m.ssh.Stream(ctx, m.proxyHost, cmd, m.out)
}

// pushEnv writes opts.EnvVars to a tmpfs file on the proxy host with
// mode 0600 and returns the docker `--env-file` flag plus a cleanup
// function that removes the file. Returns empty flag if envVars is
// empty (so lego runs without --env-file).
func (m *Manager) pushEnv(ctx context.Context, envVars map[string]string) (string, func(), error) {
	noop := func() {}
	if len(envVars) == 0 {
		return "", noop, nil
	}
	nonce, err := randomNonce()
	if err != nil {
		return "", noop, err
	}
	remotePath := "/dev/shm/qifa-lego-env." + nonce

	var buf strings.Builder
	keys := make([]string, 0, len(envVars))
	for k := range envVars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		// docker --env-file is line-oriented KEY=VALUE; it doesn't do
		// quoting or expansion. Reject newlines defensively.
		v := envVars[k]
		if strings.ContainsAny(v, "\n\r") {
			return "", noop, fmt.Errorf("env var %s contains a newline; lego --env-file can't represent that", k)
		}
		fmt.Fprintf(&buf, "%s=%s\n", k, v)
	}
	if err := m.ssh.Upload(ctx, m.proxyHost, remotePath, []byte(buf.String()), 0o600); err != nil {
		return "", noop, fmt.Errorf("push lego env to %s: %w", remotePath, err)
	}
	cleanup := func() {
		_, _ = m.ssh.Run(context.Background(), m.proxyHost, "rm -f "+shellQuote(remotePath))
	}
	return "--env-file " + shellQuote(remotePath), cleanup, nil
}

func (m *Manager) mountPoint() string {
	return LegoMountPoint
}

func (m *Manager) subdirPath() string {
	return m.mountPoint() + "/" + m.subdir
}

func randomNonce() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// shellQuote single-quotes a value for safe interpolation into a
// remote shell command.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
