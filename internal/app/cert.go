package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/tendant/qifa/internal/cert"
	"github.com/tendant/qifa/internal/config"
	"github.com/tendant/qifa/internal/proxy"
	"github.com/tendant/qifa/internal/ssh"
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
  qifa cert issue  <host> [extra-host ...] --provider <name> --email <addr> [--staging] [--env-file <path>] [--dns-resolvers host:port[,host:port...]] [--lego-image <ref>]
  qifa cert renew  --email <addr> [--provider <name>] [--renew-days N]
  qifa cert renew  <host> [extra-host ...] --email <addr> [--provider <name>] [--renew-days N]
  qifa cert renew  --all  --email <addr> [--provider <name>] [--renew-days N] [--env-file <path>] [--dns-resolvers host:port[,host:port...]] [--lego-image <ref>]
  qifa cert list   [--expiry]
  qifa cert remove <host>

Pass extra positional hostnames after <host> to issue a multi-domain
(SAN) cert covering all of them. The first host is the cert filename;
the rest become Subject Alternative Names. Useful for apps that
register multiple proxy.hosts: in qifa.yaml — kamal-proxy serves the
same cert for every host on the app, so single-name certs break TLS
on all hosts but the first.

--dns-resolvers overrides the recursive nameservers used to check DNS-01
propagation (comma-separated, or repeat the flag). Defaults to public
resolvers (see cert.DefaultDNSResolvers) rather than whatever resolver the
proxy host happens to be configured with, since that host resolver can
hold a stale negative-cache entry for the exact challenge name from an
unrelated earlier lookup and fail propagation checks that would otherwise
succeed.

--lego-image overrides the lego container image (or set QIFA_LEGO_IMAGE;
the flag wins). The default is pinned to an exact version on purpose:
qifa's command line is lego-v5-specific, so a floating :latest can break
every issuance the moment upstream ships a new major. Only move the pin
once you've verified the newer tag.

--expiry makes "cert list" print "host <TAB> days-left <TAB> SANs" so a
scheduled renewal can select due hosts without parsing certs in shell.

"cert renew --all" recovers each cert's existing SANs from the cert on
disk, so multi-domain certs keep every name. A host whose current cert
can't be read is skipped rather than reissued single-name.

"cert renew" with no host renews this app's own cert (proxy.host plus
any proxy.hosts as SANs; provider from proxy.tls.provider), then, if it
changed, re-registers this app's running containers with kamal-proxy.
kamal-proxy reads certs only when a route is deployed and never re-reads
the files; re-registering swaps the route in place without dropping a
request, and a cert that fails to load leaves the old route serving.
Nothing changed means nothing is touched, so it is safe to run daily —
one line per app, each with its own qifa.yaml.

"cert renew --all" renews every cert on the proxy host but can only
serve this app's; other renewed certs are reported as an error until
"qifa -c <their qifa.yaml> start" re-registers them.`)
}

type certFlags struct {
	// hosts holds every positional argument. For issue/renew, hosts[0]
	// is the primary FQDN (also the cert filename) and hosts[1:] are
	// additional SAN entries. For remove, only hosts[0] is meaningful.
	hosts     []string
	provider  string
	email     string
	staging   bool
	envFile   string
	renewDays int
	all       bool
	expiry    bool
	resolvers []string
	legoImage string
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
		case a == "--expiry":
			f.expiry = true
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
		case a == "--renew-days":
			val, err := nextValue(a, args, &i)
			if err != nil {
				return f, err
			}
			n, perr := parseRenewDays(val)
			if perr != nil {
				return f, perr
			}
			f.renewDays = n
		case a == "--lego-image":
			val, err := nextValue(a, args, &i)
			if err != nil {
				return f, err
			}
			f.legoImage = val
		case a == "--dns-resolvers":
			val, err := nextValue(a, args, &i)
			if err != nil {
				return f, err
			}
			for _, r := range strings.Split(val, ",") {
				if r = strings.TrimSpace(r); r != "" {
					f.resolvers = append(f.resolvers, r)
				}
			}
		case strings.HasPrefix(a, "--"):
			return f, fmt.Errorf("unknown flag %q", a)
		default:
			f.hosts = append(f.hosts, a)
		}
	}
	return f, nil
}

func parseRenewDays(s string) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("--renew-days: %q is not a non-negative integer", s)
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
		return errors.New("usage: qifa cert issue <host> [extra-host ...] --provider <name> --email <addr> [--staging] [--env-file <path>] [--dns-resolvers host:port[,host:port...]]")
	}
	if f.provider == "" {
		return errors.New("--provider is required")
	}
	if f.email == "" {
		return errors.New("--email is required")
	}
	mgr, err := newCertManager(stdout, configFile, f.legoImage)
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
		Resolvers:  f.resolvers,
	})
}

func runCertRenew(ctx context.Context, args []string, stdout, stderr io.Writer, configFile string) error {
	f, err := parseCertFlags(args)
	if err != nil {
		return err
	}
	// Resolve once: newCertManager and withRuntime each chdir to the
	// config's directory, and an absolute path keeps that idempotent.
	configFile, err = useConfigDir(configFile)
	if err != nil {
		return err
	}
	cfg, err := config.Load(configFile)
	if err != nil {
		return err
	}
	// The app's own cert: what a bare `cert renew` renews, and the only
	// one whose route this config can re-register.
	appCert := ""
	if cfg.Proxy.SSL && cfg.Proxy.TLS != nil && cfg.Proxy.TLS.Source == "qifa" {
		appCert = proxy.PrimaryHost(cfg.Proxy)
		if f.provider == "" {
			f.provider = cfg.Proxy.TLS.Provider
		}
	}
	if f.provider == "" {
		return errors.New("--provider is required (or set proxy.tls.provider)")
	}
	// Each renewal is its own ACME interaction.
	if f.email == "" {
		return errors.New("--email is required")
	}
	mgr, err := newCertManager(stdout, configFile, f.legoImage)
	if err != nil {
		return err
	}
	envVars, err := collectCertEnv(f.envFile)
	if err != nil {
		return err
	}
	days := f.renewDays
	if days == 0 {
		days = 30
	}
	opts := cert.IssueOptions{
		Email:     f.email,
		Provider:  f.provider,
		Staging:   f.staging,
		EnvVars:   envVars,
		Resolvers: f.resolvers,
	}
	var renewed []string
	var renewErr error
	if f.all {
		renewed, renewErr = mgr.RenewAll(ctx, opts, days)
	} else {
		opts.Host, opts.ExtraHosts = f.host(), f.extraHosts()
		if opts.Host == "" {
			if appCert == "" {
				return errors.New("usage: qifa cert renew [<host> [extra-host ...] | --all] — with no host, renews this app's cert, which needs proxy.ssl: true and proxy.tls.source: qifa")
			}
			opts.Host, opts.ExtraHosts = appCert, appSANs(appCert, cfg.Proxy)
		}
		changed, err := mgr.Renew(ctx, opts, days)
		if err != nil {
			return err
		}
		if changed {
			renewed = []string{opts.Host}
		}
	}
	// Serve whatever did renew even when other certs failed.
	return errors.Join(renewErr, serveRenewed(ctx, stdout, stderr, configFile, appCert, renewed))
}

// appSANs returns the app's proxy hostnames other than primary: the
// SANs its cert must carry so every host it answers on gets valid TLS.
func appSANs(primary string, p config.Proxy) []string {
	var extras []string
	for _, h := range append([]string{p.Host}, p.Hosts...) {
		if h != "" && h != primary && !slices.Contains(extras, h) {
			extras = append(extras, h)
		}
	}
	return extras
}

// serveRenewed makes kamal-proxy serve renewed certs. It reads a cert only
// when a route is deployed, so this re-registers the config's app — rebuilt
// from qifa.yaml and the running containers' labels, with no state kept
// anywhere. A renewed cert of another app can only be served from that
// app's config, so those are named in the error rather than left silent.
func serveRenewed(ctx context.Context, stdout, stderr io.Writer, configFile, appCert string, renewed []string) error {
	if len(renewed) == 0 {
		fmt.Fprintln(stdout, "no cert changed; nothing to re-register")
		return nil
	}
	var errs []error
	var others []string
	for _, h := range renewed {
		if h != appCert {
			others = append(others, h)
			continue
		}
		errs = append(errs, withRuntime(ctx, stdout, stderr, configFile, "cert", func(rt *runtime) error {
			n, err := rt.deployer.ReregisterProxy(ctx)
			if err == nil && n == 0 {
				fmt.Fprintf(stdout, "renewed %s, but no running container to re-register; the next deploy or start serves it\n", h)
			}
			return err
		}))
	}
	if len(others) > 0 {
		errs = append(errs, fmt.Errorf("renewed but not served yet (kamal-proxy reads certs only when a route is deployed): %s — run 'qifa -c <its qifa.yaml> start' for each app, which re-registers without downtime", strings.Join(others, " ")))
	}
	return errors.Join(errs...)
}

func runCertList(ctx context.Context, args []string, stdout, stderr io.Writer, configFile string) error {
	f, err := parseCertFlags(args)
	if err != nil {
		return err
	}
	if len(f.hosts) > 0 {
		return errors.New("qifa cert list takes no positional arguments")
	}
	mgr, err := newCertManager(stdout, configFile, f.legoImage)
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
	if !f.expiry {
		for _, h := range hosts {
			fmt.Fprintln(stdout, h)
		}
		return nil
	}
	// --expiry: host <TAB> days-left <TAB> comma-separated SANs. Tab
	// separated and unadorned so a scheduled renewal can pick the due
	// hosts out with awk instead of shelling out to openssl.
	now := time.Now()
	var failed []string
	for _, h := range hosts {
		info, err := mgr.CertInfo(ctx, h)
		if err != nil {
			failed = append(failed, h)
			fmt.Fprintf(stdout, "%s\tERROR\t%v\n", h, err)
			continue
		}
		fmt.Fprintf(stdout, "%s\t%d\t%s\n", h, info.DaysLeft(now), strings.Join(info.DNSNames, ","))
	}
	if len(failed) > 0 {
		return fmt.Errorf("could not read %d cert(s): %s", len(failed), strings.Join(failed, " "))
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
	mgr, err := newCertManager(stdout, configFile, f.legoImage)
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
func newCertManager(out io.Writer, configFile, legoImage string) (*cert.Manager, error) {
	configFile, err := useConfigDir(configFile)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(configFile)
	if err != nil {
		return nil, fmt.Errorf("qifa cert needs a config file (tried %s): %w", configFile, err)
	}
	host := proxyHostFromConfig(cfg)
	if host == "" {
		return nil, fmt.Errorf("can't find proxy host: set proxy_boot.hosts (or servers.<role>.hosts) in %s", configFile)
	}
	client := ssh.New(cfg.SSH)
	return cert.New(client, host, out, cert.Options{
		LegoImage:   resolveLegoImage(legoImage),
		AlpineImage: os.Getenv("QIFA_ALPINE_IMAGE"),
	}), nil
}

// resolveLegoImage picks the lego image for this run: --lego-image wins,
// then QIFA_LEGO_IMAGE (what an unattended runner sets), then "" so
// cert.New falls back to its pinned default.
//
// Deliberately QIFA_LEGO_IMAGE and not LEGO_IMAGE: certEnvPrefixes
// forwards LEGO_* into the lego container, where lego itself would
// interpret it. QIFA_* is not forwarded.
func resolveLegoImage(flag string) string {
	if flag != "" {
		return flag
	}
	return os.Getenv("QIFA_LEGO_IMAGE")
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
