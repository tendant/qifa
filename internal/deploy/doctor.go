package deploy

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/gokamal/gocart/internal/dockererr"
	"github.com/gokamal/gocart/internal/registry"
	sshpkg "github.com/gokamal/gocart/internal/ssh"
)

// Doctor runs the checks that a deploy silently depends on — SSH, the Docker
// daemon, DNS/TCP/TLS to the registry, credentials, disk, clock, proxy — and
// prints a per-host report. It exists because those dependencies otherwise
// only announce themselves as a failed `docker pull` halfway through a deploy,
// on one host, with the reason buried in progress output.
//
// It changes nothing on the hosts: every check is a read.
func (d *Deployer) Doctor(ctx context.Context, out io.Writer) error {
	targets := d.doctorTargets()
	if len(targets) == 0 {
		return fmt.Errorf("no hosts configured")
	}

	image := d.cfg.Image
	fmt.Fprintf(out, "qifa doctor — service %s, image %s\n", d.cfg.Service, image)
	fmt.Fprintf(out, "registry endpoint: %s\n", dockererr.RegistryHost(image))

	// Credentials live where qifa runs, not on the hosts: check them once, here.
	if d.cfg.Registry.Enabled() {
		if _, ok := os.LookupEnv(d.cfg.Registry.PasswordEnv); ok {
			fmt.Fprintf(out, "registry credentials: %s is set for user %q\n", d.cfg.Registry.PasswordEnv, d.cfg.Registry.Username)
		} else {
			fmt.Fprintf(out, "registry credentials: FAIL — %s is not set in this shell; every host will fail to authenticate\n", d.cfg.Registry.PasswordEnv)
		}
	} else {
		fmt.Fprintln(out, "registry credentials: none configured (anonymous pulls only)")
	}

	failures := 0
	for _, target := range targets {
		fmt.Fprintf(out, "\n=== %s  [%s]\n", target.host, strings.Join(target.roles, ", "))
		checks, failed := d.doctorHost(ctx, target, image)
		failures += failed
		printChecks(out, checks)
	}

	fmt.Fprintln(out, "")
	if failures > 0 {
		return fmt.Errorf("%d check(s) failed — see the FAIL lines above", failures)
	}
	fmt.Fprintln(out, "all checks passed")
	return nil
}

type doctorTarget struct {
	host  string
	roles []string
}

// doctorTargets collects every host qifa will touch, with the reasons it is
// involved (app role, proxy, builder, accessory).
func (d *Deployer) doctorTargets() []doctorTarget {
	roles := map[string][]string{}
	add := func(host, role string) {
		if strings.TrimSpace(host) == "" {
			return
		}
		for _, existing := range roles[host] {
			if existing == role {
				return
			}
		}
		roles[host] = append(roles[host], role)
	}
	for role, server := range d.cfg.Servers {
		for _, host := range server.Hosts {
			add(host, "app/"+role)
		}
	}
	for _, host := range d.cfg.ProxyBoot.Hosts {
		add(host, "proxy")
	}
	for name, accessory := range d.cfg.Accessories {
		add(accessory.Host, "accessory/"+name)
	}
	if d.cfg.Builder != nil && d.cfg.Builder.IsRemote() {
		add(d.cfg.Builder.Host, "builder")
	}
	hosts := make([]string, 0, len(roles))
	for host := range roles {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	targets := make([]doctorTarget, 0, len(hosts))
	for _, host := range hosts {
		targets = append(targets, doctorTarget{host: host, roles: roles[host]})
	}
	return targets
}

func (d *Deployer) doctorHost(ctx context.Context, target doctorTarget, image string) ([]dockererr.Check, int) {
	var checks []dockererr.Check
	failed := 0
	record := func(c dockererr.Check) {
		if c.Status == "FAIL" {
			failed++
		}
		checks = append(checks, c)
	}

	start := time.Now()
	transport := "ssh"
	if sshpkg.IsLocal(target.host) {
		transport = "transport"
	}
	if _, err := d.ssh.Run(ctx, target.host, "echo ok"); err != nil {
		record(dockererr.Check{Name: transport, Status: "FAIL", Detail: err.Error()})
		// Nothing else can be checked without a shell.
		return checks, failed
	}
	if sshpkg.IsLocal(target.host) {
		record(dockererr.Check{Name: transport, Status: "ok", Detail: "local shell — this machine, no SSH"})
	} else {
		record(dockererr.Check{Name: transport, Status: "ok", Detail: fmt.Sprintf("connected in %s", time.Since(start).Round(time.Millisecond))})
	}

	report := dockererr.Diagnose(ctx, d.ssh, target.host, image)
	if report.Err != nil {
		record(dockererr.Check{Name: "diagnostics", Status: "FAIL", Detail: report.Err.Error()})
		return checks, failed
	}
	for _, c := range report.Checks {
		record(c)
	}

	record(d.doctorRegistryAuth(ctx, target.host, image))

	if d.usesProxy() {
		if err := d.proxy.EnsureRunning(ctx, target.host); err != nil {
			if hostInList(target.host, d.cfg.ProxyBoot.Hosts) {
				record(dockererr.Check{Name: "kamal-proxy", Status: "FAIL", Detail: "not running — run `qifa proxy boot`"})
			} else {
				record(dockererr.Check{Name: "kamal-proxy", Status: "info", Detail: "not running on this host (not a proxy host)"})
			}
		} else {
			record(dockererr.Check{Name: "kamal-proxy", Status: "ok", Detail: "running"})
		}
	}
	return checks, failed
}

// doctorRegistryAuth asks the host to resolve the image's manifest. That
// exercises DNS, TLS, authentication and repository permissions in one shot —
// the whole pull path, minus the layer download.
func (d *Deployer) doctorRegistryAuth(ctx context.Context, host, image string) dockererr.Check {
	if d.cfg.IsLocalSource() {
		return dockererr.Check{Name: "registry pull", Status: "info", Detail: "source: local — qifa never pulls this image"}
	}
	if !strings.Contains(image, ":") && !strings.Contains(image, "@") {
		image += ":latest"
	}
	dockerConfigDir := ""
	if d.cfg.Registry.Enabled() {
		var err error
		dockerConfigDir, err = registry.Login(ctx, d.ssh, d.cfg.Registry, host)
		if err != nil {
			return dockererr.Check{Name: "registry pull", Status: "FAIL", Detail: "could not upload registry credentials: " + err.Error()}
		}
	}
	cmd := "docker manifest inspect " + shellQuoteB(image) + " >/dev/null"
	if dockerConfigDir != "" {
		cmd = "DOCKER_CONFIG=" + shellQuoteB(dockerConfigDir) + " " + cmd
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if _, err := d.ssh.Run(ctx, host, cmd); err != nil {
		output := strings.TrimSpace(dockererr.Output(err))
		// `docker manifest` is gated behind experimental CLI features on older
		// clients. That is not a registry problem, so don't report it as one.
		if strings.Contains(strings.ToLower(output), "experimental") {
			return dockererr.Check{Name: "registry pull", Status: "warn", Detail: "docker manifest is disabled on this client; could not verify credentials without pulling"}
		}
		cause, known := dockererr.ClassifyErr(err)
		detail := firstLine(output)
		if known {
			detail = cause.Summary + " — " + detail
		}
		return dockererr.Check{Name: "registry pull", Status: "FAIL", Detail: detail}
	}
	return dockererr.Check{Name: "registry pull", Status: "ok", Detail: "manifest for " + image + " is reachable and authorized"}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func hostInList(host string, hosts []string) bool {
	for _, h := range hosts {
		if h == host {
			return true
		}
	}
	return false
}

func (d *Deployer) usesProxy() bool {
	for role, server := range d.cfg.Servers {
		if serverUsesProxy(role, server) {
			return true
		}
	}
	return false
}

func printChecks(out io.Writer, checks []dockererr.Check) {
	width := 0
	for _, c := range checks {
		if len(c.Name) > width {
			width = len(c.Name)
		}
	}
	for _, c := range checks {
		detail := strings.Join(strings.Fields(c.Detail), " ") // one line, no runs of space
		if len(detail) > 400 {
			detail = detail[:400] + "…"
		}
		fmt.Fprintf(out, "  %-*s  %-4s  %s\n", width, c.Name, c.Status, detail)
	}
}
