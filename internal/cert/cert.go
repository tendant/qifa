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
	"bytes"
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/gokamal/gocart/internal/ssh"
)

const (
	// LegoImage is the lego container image used for cert acquisition
	// and renewal. Pinned to an exact version, NOT :latest — qifa's
	// command line is lego-v5-specific (`run` as get-or-renew,
	// --renew-days, --ari-disable), so a major bump upstream silently
	// breaks every issuance. That is exactly how 36 certs hard-expired
	// on 2026-08-13: a nightly `docker image prune` dropped the cached
	// image and the re-pull of :latest picked up a v5 that the
	// then-current qifa couldn't drive. Override with --lego-image or
	// QIFA_LEGO_IMAGE once you've verified a newer tag.
	LegoImage = "goacme/lego:v5.3.1"

	// AlpineImage is the helper image used for filesystem operations
	// inside the proxy volume (listing certs, reading them back,
	// removing files, fixing perms). Pinned for the same reason as
	// LegoImage, though alpine is far less likely to break us.
	AlpineImage = "alpine:3.20"

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

// DefaultDNSResolvers are the recursive nameservers lego uses to verify a
// DNS-01 challenge has propagated, when IssueOptions.Resolvers is empty.
// The lego container inherits whatever resolver the proxy host is
// configured with, which can be flaky — or worse, can already hold a
// stale negative-cache (NXDOMAIN) entry for the exact challenge name, from
// an unrelated earlier lookup against that same resolver, long enough to
// outlast lego's propagation-check window even though the record is live
// at the authoritative nameservers. Pinning to well-known public resolvers
// makes propagation checks deterministic and independent of host DNS
// config and of anything else that happened to query the same name.
var DefaultDNSResolvers = []string{"1.1.1.1:53", "8.8.8.8:53"}

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
	// Host is the primary FQDN to issue the cert for. Also the cert
	// filename (lego saves under <primary>.crt + .key).
	Host string

	// ExtraHosts are additional Subject Alternative Names (SAN) to
	// include in the same cert. Empty for a single-name cert (most
	// apps). Used when a kamal-proxy app registers proxy.hosts: with
	// multiple FQDNs and needs one cert covering all of them.
	ExtraHosts []string

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

	// Resolvers overrides the recursive nameservers lego uses to check
	// DNS-01 propagation (host:port each, e.g. "1.1.1.1:53"). Defaults to
	// DefaultDNSResolvers when empty.
	Resolvers []string

	// EnvVars are passed through to the lego container (for the DNS
	// provider's API credentials). Pushed to /dev/shm on the proxy
	// host with mode 0600 and removed on exit — never persisted.
	EnvVars map[string]string
}

// Issue acquires a fresh cert and writes it into the proxy volume.
func (m *Manager) Issue(ctx context.Context, opts IssueOptions) error {
	return m.runLego(ctx, opts, nil)
}

// Renew refreshes a cert if it's within `days` of expiring. Returns
// without error if the cert isn't due for renewal yet.
//
// lego v5 has no `renew` subcommand — `run` is get-or-renew, and decides
// from --renew-days whether the cert on disk is due. An already-expired
// cert has negative time remaining, so it always renews.
func (m *Manager) Renew(ctx context.Context, opts IssueOptions, days int) error {
	extra := []string{}
	if days > 0 {
		extra = append(extra, "--renew-days", fmt.Sprintf("%d", days))
	}
	return m.runLego(ctx, opts, extra)
}

// RenewAll iterates every cert currently in the proxy volume and runs
// Renew on each, preserving each cert's existing SAN list. Continues
// past failures so one bad cert can't block the others, and returns an
// error naming every host that failed.
func (m *Manager) RenewAll(ctx context.Context, opts IssueOptions, days int) error {
	hosts, err := m.List(ctx)
	if err != nil {
		return err
	}
	if len(hosts) == 0 {
		fmt.Fprintln(m.out, "no certs to renew")
		return nil
	}
	var failed []string
	for _, h := range hosts {
		fmt.Fprintf(m.out, "==> %s\n", h)
		// List() only knows cert filenames, so a multi-domain cert
		// looks single-name here. Recover the SANs from the cert on
		// disk — reissuing without them silently shrinks the cert and
		// breaks TLS on every host but the first.
		names, err := m.CertNames(ctx, h)
		if err != nil {
			failed = append(failed, h)
			fmt.Fprintf(m.out, "  (skipped %s: can't read its current SANs: %v)\n", h, err)
			continue
		}
		hostOpts := opts
		hostOpts.Host = h
		hostOpts.ExtraHosts = sanExtras(h, names)
		if err := m.Renew(ctx, hostOpts, days); err != nil {
			failed = append(failed, h)
			fmt.Fprintf(m.out, "  (renew failed for %s: %v — continuing)\n", h, err)
		}
	}
	fmt.Fprintf(m.out, "\n%d/%d certs ok", len(hosts)-len(failed), len(hosts))
	if len(failed) > 0 {
		// Name every failure on the last line: for an unattended run
		// this is what a human reads out of the log.
		fmt.Fprintf(m.out, ", %d failed: %s\n", len(failed), strings.Join(failed, " "))
		return fmt.Errorf("%d of %d certs failed: %s", len(failed), len(hosts), strings.Join(failed, " "))
	}
	fmt.Fprintln(m.out)
	return nil
}

// CertInfo describes one cert stored in the proxy volume.
type CertInfo struct {
	// Host is the cert filename stem — the primary FQDN.
	Host string
	// NotAfter is the leaf's expiry.
	NotAfter time.Time
	// DNSNames are the leaf's SANs, in certificate order (lego puts
	// the primary first).
	DNSNames []string
}

// DaysLeft reports whole days until expiry, negative once expired.
// Floors rather than truncating so a cert that expired earlier today
// reads -1, not 0 — truncation rounds toward zero, which would report
// an already-dead cert as if it still had the day left.
func (c CertInfo) DaysLeft(now time.Time) int {
	return int(math.Floor(c.NotAfter.Sub(now).Hours() / 24))
}

// CertNames returns the DNS SANs of the cert currently stored for host.
func (m *Manager) CertNames(ctx context.Context, host string) ([]string, error) {
	info, err := m.CertInfo(ctx, host)
	if err != nil {
		return nil, err
	}
	return info.DNSNames, nil
}

// CertInfo reads the cert stored for host and reports its expiry and
// SANs. The volume is docker-private, so the cert is read out through
// the alpine helper rather than from a host path.
func (m *Manager) CertInfo(ctx context.Context, host string) (CertInfo, error) {
	cmd := fmt.Sprintf(
		"docker run --rm -v %s:%s %s cat %s",
		shellQuote(m.volumeName),
		shellQuote(m.mountPoint()),
		shellQuote(m.alpineImg),
		shellQuote(fmt.Sprintf("%s/certificates/%s.crt", m.subdirPath(), host)),
	)
	out, err := m.ssh.Run(ctx, m.proxyHost, cmd)
	if err != nil {
		return CertInfo{}, fmt.Errorf("read cert for %s: %w", host, err)
	}
	leaf, err := leafFromPEM([]byte(out))
	if err != nil {
		return CertInfo{}, fmt.Errorf("cert for %s: %w", host, err)
	}
	return CertInfo{Host: host, NotAfter: leaf.NotAfter, DNSNames: leaf.DNSNames}, nil
}

// leafFromPEM pulls the leaf out of a lego .crt bundle (leaf first,
// then the issuer chain). Pure, so cert parsing is testable without
// docker or SSH.
func leafFromPEM(data []byte) (*x509.Certificate, error) {
	// ssh.Client.Run trims its output, but pem.Decode needs the END
	// line terminated — put the newline back before parsing.
	data = append(bytes.TrimSpace(data), '\n')
	for block, rest := pem.Decode(data); block != nil; block, rest = pem.Decode(rest) {
		if block.Type != "CERTIFICATE" {
			continue
		}
		leaf, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse leaf certificate: %w", err)
		}
		return leaf, nil
	}
	return nil, fmt.Errorf("no CERTIFICATE block found")
}

// dnsNamesFromPEM returns the leaf's DNS SANs.
func dnsNamesFromPEM(data []byte) ([]string, error) {
	leaf, err := leafFromPEM(data)
	if err != nil {
		return nil, err
	}
	return leaf.DNSNames, nil
}

// sanExtras returns the --domains values to add beyond the primary:
// every DNS name in the existing cert except the primary itself,
// de-duplicated. DNS names are case-insensitive, so compare folded but
// emit the name as it appears in the cert.
func sanExtras(primary string, names []string) []string {
	seen := map[string]bool{strings.ToLower(primary): true}
	var extras []string
	for _, n := range names {
		key := strings.ToLower(n)
		if seen[key] {
			continue
		}
		seen[key] = true
		extras = append(extras, n)
	}
	return extras
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
	return parseCertList(out), nil
}

// parseCertList turns the remote `ls` output into hostnames, dropping
// lego's `<host>.issuer.crt` companion files. Those aren't hosts, and
// leaving them in makes `cert list` show phantom entries and
// `renew --all` burn an ACME order failing on a name that doesn't exist.
// Pure so it can be unit tested without an SSH round-trip.
func parseCertList(out string) []string {
	out = strings.TrimSpace(out)
	if out == "" {
		return nil
	}
	var hosts []string
	for _, h := range strings.Split(out, "\n") {
		h = strings.TrimSpace(h)
		if h == "" || strings.HasSuffix(h, ".issuer") {
			continue
		}
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	return hosts
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
		// lego writes four files per cert: the leaf, the issuer chain,
		// the key, and a metadata sidecar. Removing only .crt/.key
		// leaves the other two orphaned in the volume forever, so
		// "remove" quietly didn't.
		shellQuote(fmt.Sprintf(
			"rm -f %s/certificates/%s.crt %s/certificates/%s.key %s/certificates/%s.issuer.crt %s/certificates/%s.json",
			m.subdirPath(), host, m.subdirPath(), host,
			m.subdirPath(), host, m.subdirPath(), host,
		)),
	)
	_, err := m.ssh.Run(ctx, m.proxyHost, cmd)
	return err
}

// runLego pushes credentials (if any) to /dev/shm on the proxy host,
// runs lego in a one-shot container, and cleans up on exit.
func (m *Manager) runLego(ctx context.Context, opts IssueOptions, extra []string) error {
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

	cmd := m.legoCommand(envFileFlag, opts, extra)
	if err := m.ssh.Stream(ctx, m.proxyHost, cmd, m.out); err != nil {
		return err
	}
	// lego (running as root inside the helper container) writes certs
	// as root mode 600. kamal-proxy runs as a non-root user (uid 1001
	// in current upstream images) and can't even traverse the qifa
	// subtree to load the cert. Open it up just enough — directories
	// world-traversable, files world-readable. The volume itself is
	// docker-private, so this isn't loosening anything that matters.
	return m.fixCertPerms(ctx)
}

// legoCommand builds the remote `docker run … lego run …` command line.
// Kept free of the SSH round-trip so the flag wiring is unit testable.
//
// lego v5 dropped the `renew` subcommand outright — `run` is get-or-renew
// — and moved --dns / --email / --domains / --path / --accept-tos /
// --server from global flags to options of `run`, so the action name
// comes BEFORE them. None of this works against lego v4, where those are
// global flags preceding the action: pinning Options.LegoImage to a v4
// tag needs an older qifa binary too. Default LegoImage
// `goacme/lego:latest` has been v5+ since mid-2025.
func (m *Manager) legoCommand(envFileFlag string, opts IssueOptions, extra []string) string {
	args := []string{
		"docker run --rm",
		envFileFlag,
		"-v " + shellQuote(m.volumeName) + ":" + shellQuote(m.mountPoint()),
		shellQuote(m.legoImage),
		"run",
		"--dns " + shellQuote(opts.Provider),
		"--email " + shellQuote(opts.Email),
		"--domains " + shellQuote(opts.Host),
	}
	// Each extra host becomes another --domains flag — lego supports
	// repeating it for SAN. The cert file still gets saved under
	// opts.Host (the first --domains value).
	for _, e := range opts.ExtraHosts {
		args = append(args, "--domains "+shellQuote(e))
	}
	for _, r := range resolverArgs(opts.Resolvers) {
		args = append(args, "--dns.resolvers "+shellQuote(r))
	}
	args = append(args,
		// Check propagation against the authoritative nameservers only.
		// The recursive-resolver check is the half that fails in
		// practice: a public resolver caches a miss for the challenge
		// name and doesn't pick the TXT record up inside lego's window.
		// That stalled roughly half of a 23-cert recovery run — hosts
		// that timed out three times here succeeded first try with the
		// check off. --dns.resolvers above still governs CNAME and apex
		// resolution.
		"--dns.propagation.disable-rns",
		// Let's Encrypt rejects the ARI `replaces` field for a cert it
		// doesn't attribute to the requesting account ("requester
		// account did not request the certificate being replaced"),
		// which fails the whole order with a 403. ARI only schedules
		// renewals early, so nothing is lost by skipping it.
		"--ari-disable",
		// lego sleeps a random 0-8 minutes before each renewal to
		// spread load on the CA. That's per *certificate*, so a
		// renew --all over a few dozen certs spends most of an hour
		// asleep and can outrun the caller's timeout. qifa renews
		// serially and callers do their own scheduling jitter (the
		// weilabs timer uses RandomizedDelaySec), so the CA-friendly
		// spread is already there without paying for it N times.
		"--no-random-sleep",
		"--path "+shellQuote(m.subdirPath()),
		"--accept-tos",
	)
	if opts.Staging {
		args = append(args, "--server", shellQuote("https://acme-staging-v02.api.letsencrypt.org/directory"))
	}
	for _, e := range extra {
		args = append(args, shellQuote(e))
	}
	return strings.Join(args, " ")
}

// fixCertPerms makes the qifa cert subtree readable by whatever
// non-root user kamal-proxy is running as. Idempotent.
func (m *Manager) fixCertPerms(ctx context.Context) error {
	cmd := fmt.Sprintf(
		"docker run --rm -v %s:%s %s chmod -R o+rX %s",
		shellQuote(m.volumeName),
		shellQuote(m.mountPoint()),
		shellQuote(m.alpineImg),
		shellQuote(m.subdirPath()),
	)
	_, err := m.ssh.Run(ctx, m.proxyHost, cmd)
	return err
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

// resolverArgs returns resolvers, or DefaultDNSResolvers if resolvers is
// empty. Pure and side-effect free so it's cheap to unit test.
func resolverArgs(resolvers []string) []string {
	if len(resolvers) > 0 {
		return resolvers
	}
	return DefaultDNSResolvers
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
