package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gokamal/gocart/internal/cert"
	"github.com/gokamal/gocart/internal/config"
	"github.com/gokamal/gocart/internal/ssh"
)

// certEnvPrefixes lists env-var prefixes auto-forwarded to lego when
// running cert ops. Covers the most common DNS providers; users with
// other providers can add vars via --env-file.
var certEnvPrefixes = []string{
	"AZURE_",
	"AWS_",
	"CF_",
	"CLOUDFLARE_",
	"DNS_",
	"DO_",
	"GANDI_",
	"GANDIV5_",
	"GCE_",
	"GOOGLE_",
	"HETZNER_",
	"LEGO_",
	"LINODE_",
	"NAMECHEAP_",
	"OVH_",
	"PORKBUN_",
	"VULTR_",
}

func runCert(ctx context.Context, args []string, stdout, stderr io.Writer, configFile string) error {
	if len(args) == 0 {
		return certUsageError()
	}
	switch args[0] {
	case "issue":
		return runCertIssue(ctx, args[1:], stdout, stderr, configFile)
	case "renew":
		return runCertRenew(ctx, args[1:], stdout, stderr, configFile)
	case "list":
		return runCertList(ctx, args[1:], stdout, stderr, configFile)
	case "remove":
		return runCertRemove(ctx, args[1:], stdout, stderr, configFile)
	default:
		return certUsageError()
	}
}

func certUsageError() error {
	return errors.New(`usage:
  qifa cert issue  <host> [extra-host ...] --provider <name> --email <addr> [--staging] [--env-file <path>]
  qifa cert renew  <host> [extra-host ...] [--days N]
  qifa cert renew  --all  --provider <name> --email <addr> [--days N] [--env-file <path>]
  qifa cert list
  qifa cert remove <host>

Pass extra positional hostnames after <host> to issue a multi-domain
(SAN) cert covering all of them. The first host is the cert filename;
the rest become Subject Alternative Names. Useful for apps that
register multiple proxy.hosts: in qifa.yaml — kamal-proxy serves the
same cert for every host on the app, so single-name certs break TLS
on all hosts but the first.`)
}

type certFlags struct {
	// hosts holds every positional argument. For issue/renew, hosts[0]
	// is the primary FQDN (also the cert filename) and hosts[1:] are
	// additional SAN entries. For remove, only hosts[0] is meaningful.
	hosts    []string
	provider string
	email    string
	staging  bool
	envFile  string
	days     int
	all      bool
}

// host returns the primary FQDN (hosts[0]) or "" if none was provided.
// Most callers check this first to decide whether to print usage.
func (f certFlags) host() string {
	if len(f.hosts) == 0 {
		return ""
	}
	return f.hosts[0]
}

// extraHosts returns any additional SAN entries beyond the primary.
func (f certFlags) extraHosts() []string {
	if len(f.hosts) < 2 {
		return nil
	}
	return f.hosts[1:]
}

func parseCertFlags(args []string) (certFlags, error) {
	var f certFlags
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--all":
			f.all = true
		case a == "--staging":
			f.staging = true
		case a == "--provider":
			val, err := nextValue(a, args, &i)
			if err != nil {
				return f, err
			}
			f.provider = val
		case a == "--email":
			val, err := nextValue(a, args, &i)
			if err != nil {
				return f, err
			}
			f.email = val
		case a == "--env-file":
			val, err := nextValue(a, args, &i)
			if err != nil {
				return f, err
			}
			f.envFile = val
		case a == "--days":
			val, err := nextValue(a, args, &i)
			if err != nil {
				return f, err
			}
			n, perr := parseDays(val)
			if perr != nil {
				return f, perr
			}
			f.days = n
		case strings.HasPrefix(a, "--"):
			return f, fmt.Errorf("unknown flag %q", a)
		default:
			f.hosts = append(f.hosts, a)
		}
	}
	return f, nil
}

func parseDays(s string) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("--days: %q is not a non-negative integer", s)
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

func runCertIssue(ctx context.Context, args []string, stdout, stderr io.Writer, configFile string) error {
	f, err := parseCertFlags(args)
	if err != nil {
		return err
	}
	if f.host() == "" {
		return errors.New("usage: qifa cert issue <host> [extra-host ...] --provider <name> --email <addr> [--staging] [--env-file <path>]")
	}
	if f.provider == "" {
		return errors.New("--provider is required")
	}
	if f.email == "" {
		return errors.New("--email is required")
	}
	mgr, err := newCertManager(stdout, configFile)
	if err != nil {
		return err
	}
	envVars, err := collectCertEnv(f.envFile)
	if err != nil {
		return err
	}
	return mgr.Issue(ctx, cert.IssueOptions{
		Host:       f.host(),
		ExtraHosts: f.extraHosts(),
		Email:      f.email,
		Provider:   f.provider,
		Staging:    f.staging,
		EnvVars:    envVars,
	})
}

func runCertRenew(ctx context.Context, args []string, stdout, stderr io.Writer, configFile string) error {
	f, err := parseCertFlags(args)
	if err != nil {
		return err
	}
	mgr, err := newCertManager(stdout, configFile)
	if err != nil {
		return err
	}
	envVars, err := collectCertEnv(f.envFile)
	if err != nil {
		return err
	}
	days := f.days
	if days == 0 {
		days = 30
	}
	if f.all {
		// `renew --all` requires email and provider just like issue,
		// since each renewal is its own ACME interaction.
		if f.provider == "" {
			return errors.New("--provider is required with --all")
		}
		if f.email == "" {
			return errors.New("--email is required with --all")
		}
		return mgr.RenewAll(ctx, cert.IssueOptions{
			Email:    f.email,
			Provider: f.provider,
			Staging:  f.staging,
			EnvVars:  envVars,
		}, days)
	}
	if f.host() == "" {
		return errors.New("usage: qifa cert renew <host> [extra-host ...] [--days N]   (or: qifa cert renew --all ...)")
	}
	if f.provider == "" {
		return errors.New("--provider is required")
	}
	if f.email == "" {
		return errors.New("--email is required")
	}
	return mgr.Renew(ctx, cert.IssueOptions{
		Host:       f.host(),
		ExtraHosts: f.extraHosts(),
		Email:      f.email,
		Provider:   f.provider,
		Staging:    f.staging,
		EnvVars:    envVars,
	}, days)
}

func runCertList(ctx context.Context, args []string, stdout, stderr io.Writer, configFile string) error {
	if len(args) > 0 {
		return errors.New("qifa cert list takes no arguments")
	}
	mgr, err := newCertManager(stdout, configFile)
	if err != nil {
		return err
	}
	hosts, err := mgr.List(ctx)
	if err != nil {
		return err
	}
	if len(hosts) == 0 {
		fmt.Fprintln(stdout, "no qifa-managed certs found")
		return nil
	}
	for _, h := range hosts {
		fmt.Fprintln(stdout, h)
	}
	return nil
}

func runCertRemove(ctx context.Context, args []string, stdout, stderr io.Writer, configFile string) error {
	f, err := parseCertFlags(args)
	if err != nil {
		return err
	}
	if f.host() == "" {
		return errors.New("usage: qifa cert remove <host>")
	}
	if len(f.extraHosts()) > 0 {
		return fmt.Errorf("qifa cert remove takes one host; got %d extra (%q)", len(f.extraHosts()), f.extraHosts())
	}
	mgr, err := newCertManager(stdout, configFile)
	if err != nil {
		return err
	}
	if err := mgr.Remove(ctx, f.host()); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "removed cert for %s. redeploy any app that referenced it.\n", f.host())
	return nil
}

// newCertManager loads the config file to derive the proxy host
// and SSH config, then constructs a cert.Manager.
func newCertManager(out io.Writer, configFile string) (*cert.Manager, error) {
	cfg, err := config.Load(configFile)
	if err != nil {
		return nil, fmt.Errorf("qifa cert needs a config file (tried %s): %w", configFile, err)
	}
	host := proxyHostFromConfig(cfg)
	if host == "" {
		return nil, fmt.Errorf("can't find proxy host: set proxy_boot.hosts (or servers.<role>.hosts) in %s", configFile)
	}
	client := ssh.New(cfg.SSH)
	return cert.New(client, host, out, cert.Options{}), nil
}

// proxyHostFromConfig picks the canonical SSH target for cert ops.
// proxy_boot.hosts is the right answer (that's where the proxy runs);
// fall back to the first server's first host if proxy_boot is empty,
// since every example carries servers.* even when proxy_boot is the
// owning surface for the proxy itself.
func proxyHostFromConfig(cfg *config.Config) string {
	if len(cfg.ProxyBoot.Hosts) > 0 {
		return cfg.ProxyBoot.Hosts[0]
	}
	for _, server := range cfg.Servers {
		if len(server.Hosts) > 0 {
			return server.Hosts[0]
		}
	}
	return ""
}

// collectCertEnv gathers env vars for the lego container. Pulls vars
// matching certEnvPrefixes from the current process env, then layers
// vars from --env-file (KEY=VALUE per line, # comments allowed).
// File values override prefix-matched env values on key collision.
func collectCertEnv(envFile string) (map[string]string, error) {
	out := map[string]string{}
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		k := kv[:eq]
		for _, prefix := range certEnvPrefixes {
			if strings.HasPrefix(k, prefix) {
				out[k] = kv[eq+1:]
				break
			}
		}
	}
	if envFile == "" {
		return out, nil
	}
	f, err := os.Open(envFile)
	if err != nil {
		return nil, fmt.Errorf("--env-file: %w", err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			return nil, fmt.Errorf("--env-file %s line %d: expected KEY=VALUE", envFile, lineNo)
		}
		k := strings.TrimSpace(line[:eq])
		v := line[eq+1:]
		// Strip surrounding quotes (common in .env files).
		if len(v) >= 2 {
			if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
				v = v[1 : len(v)-1]
			}
		}
		out[k] = v
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func nextValue(flag string, args []string, i *int) (string, error) {
	if *i+1 >= len(args) {
		return "", fmt.Errorf("%s requires a value", flag)
	}
	*i++
	return args[*i], nil
}
