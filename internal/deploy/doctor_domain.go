package deploy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tendant/qifa/internal/config"
	"github.com/tendant/qifa/internal/dockererr"
	sshpkg "github.com/tendant/qifa/internal/ssh"
)

// The domain checks cover what the guide's step 0 leaves to the reader and a
// deploy only reveals as a certificate that never arrives: the names the
// proxy serves must point at the host, its ports 80 and 443 must be open
// through every firewall on the way, and nothing else on the host may hold
// them. They run from this machine, which sees DNS and firewalls much as the
// internet does, though not exactly: a split-horizon resolver, /etc/hosts or
// a VPN can make this machine's view differ. So DNS findings are warnings,
// and only a port that times out, the signature of a firewall dropping
// packets, fails.

// domainProbe is what the domain checks ask of the world, so tests can stand
// in for DNS and the network.
type domainProbe struct {
	lookup func(ctx context.Context, name string) ([]string, error)
	dial   func(ctx context.Context, address string) error
}

const portProbeTimeout = 3 * time.Second

func netDomainProbe() domainProbe {
	return domainProbe{
		lookup: net.DefaultResolver.LookupHost,
		dial: func(ctx context.Context, address string) error {
			dialer := net.Dialer{Timeout: portProbeTimeout}
			conn, err := dialer.DialContext(ctx, "tcp", address)
			if err == nil {
				conn.Close()
			}
			return err
		},
	}
}

// doctorDomain checks a proxy host's names and ports. proxyRunning says
// whether kamal-proxy already holds the ports there.
func (d *Deployer) doctorDomain(ctx context.Context, host string, proxyRunning bool) []dockererr.Check {
	// The host's own addresses, so a name pointing at a public IP the host
	// owns matches even when qifa reaches it by another (a VPN, a private
	// network). Best effort: without them, only the SSH address counts.
	ifaceOut, _ := d.ssh.Run(ctx, host, `ip -o addr show 2>/dev/null | awk '{print $4}'; hostname -I 2>/dev/null; true`)
	// Every TCP socket in LISTEN state, read from /proc, which needs no tools
	// on the host; empty anywhere /proc/net/tcp does not exist.
	listenOut := ""
	if !proxyRunning {
		listenOut, _ = d.ssh.Run(ctx, host, `cat /proc/net/tcp /proc/net/tcp6 2>/dev/null; true`)
	}
	return domainChecks(ctx, netDomainProbe(), domainInput{
		route:        d.cfg.ProxyRoute(),
		boot:         proxyBootFor(d.cfg),
		sshAddr:      sshpkg.HostName(host),
		hostAddrs:    parseHostAddrs(ifaceOut),
		listening:    listeningPorts(listenOut),
		proxyRunning: proxyRunning,
	})
}

type domainInput struct {
	route        config.Proxy
	boot         config.ProxyBoot
	sshAddr      string // the address qifa's SSH connects to
	hostAddrs    []string
	listening    map[int]bool // nil when the host's listeners are unknown
	proxyRunning bool
}

func domainChecks(ctx context.Context, probe domainProbe, in domainInput) []dockererr.Check {
	var checks []dockererr.Check
	httpChallenge := in.route.SSL && (in.route.TLS == nil || in.route.TLS.Source == "" || in.route.TLS.Source == "kamal")

	// What this host answers as: the SSH address (resolved, if a name) and
	// the addresses on its interfaces.
	sshIPs := []string{in.sshAddr}
	if net.ParseIP(in.sshAddr) == nil {
		if ips, err := probe.lookup(ctx, in.sshAddr); err == nil && len(ips) > 0 {
			sshIPs = ips
		}
	}
	mine := map[string]bool{}
	for _, ip := range append(append([]string{}, sshIPs...), in.hostAddrs...) {
		mine[normalizeIP(ip)] = true
	}

	// The port probes go to the address the name points at when that is this
	// host: that is the path the internet takes, through the provider's
	// firewall, which an SSH address on a private network may bypass.
	probeAddr := sshIPs[0]
	for _, name := range proxyNames(in.route) {
		if strings.HasPrefix(name, "*.") {
			checks = append(checks, dockererr.Check{Name: "dns " + name, Status: "info", Detail: "a wildcard; check a name under it by hand"})
			continue
		}
		ips, err := probe.lookup(ctx, name)
		if err != nil || len(ips) == 0 {
			detail := fmt.Sprintf("does not resolve from this machine: add an A record pointing at %s", sshIPs[0])
			if httpChallenge {
				detail += "; the certificate can't be issued until it does"
			}
			checks = append(checks, dockererr.Check{Name: "dns " + name, Status: "warn", Detail: detail})
			continue
		}
		var matched []string
		for _, ip := range ips {
			if mine[normalizeIP(ip)] {
				matched = append(matched, ip)
			}
		}
		if len(matched) == 0 {
			checks = append(checks, dockererr.Check{Name: "dns " + name, Status: "warn", Detail: fmt.Sprintf(
				"points at %s, not this host (%s): fix the record, unless a CDN or load balancer sits in front",
				strings.Join(ips, ", "), strings.Join(sshIPs, ", "))})
			continue
		}
		probeAddr = matched[0]
		checks = append(checks, dockererr.Check{Name: "dns " + name, Status: "ok", Detail: fmt.Sprintf("points at %s, this host", strings.Join(ips, ", "))})
	}

	ports := []int{portOr(in.boot.HTTPPort, 80)}
	if in.route.SSL {
		ports = append(ports, portOr(in.boot.HTTPSPort, 443))
	}
	for _, port := range ports {
		checks = append(checks, portCheck(ctx, probe, probeAddr, port, in.proxyRunning, httpChallenge))
	}

	if !in.proxyRunning && in.listening != nil {
		var taken []string
		for _, port := range ports {
			if in.listening[port] {
				taken = append(taken, ":"+strconv.Itoa(port))
			}
		}
		if len(taken) > 0 {
			checks = append(checks, dockererr.Check{Name: "ports free", Status: "FAIL", Detail: fmt.Sprintf(
				"something on this host already listens on %s, so kamal-proxy can't start: stop it (often nginx or apache2: systemctl disable --now nginx)",
				strings.Join(taken, " and "))})
		} else {
			checks = append(checks, dockererr.Check{Name: "ports free", Status: "ok", Detail: "nothing on this host listens on " + joinPorts(ports) + " yet"})
		}
	}
	return checks
}

func portCheck(ctx context.Context, probe domainProbe, addr string, port int, proxyRunning, httpChallenge bool) dockererr.Check {
	name := "port " + strconv.Itoa(port)
	err := probe.dial(ctx, net.JoinHostPort(addr, strconv.Itoa(port)))
	switch {
	case err == nil:
		detail := "open from this machine to " + addr
		if proxyRunning {
			detail += " (kamal-proxy)"
		}
		return dockererr.Check{Name: name, Status: "ok", Detail: detail}
	case errors.Is(err, syscall.ECONNREFUSED):
		if proxyRunning {
			return dockererr.Check{Name: name, Status: "warn", Detail: fmt.Sprintf(
				"%s refuses connections though kamal-proxy runs: check proxy_boot.bind_ips and http_port/https_port", addr)}
		}
		return dockererr.Check{Name: name, Status: "ok", Detail: fmt.Sprintf(
			"reachable from this machine to %s; nothing listens yet (the first deploy starts kamal-proxy)", addr)}
	case isTimeout(err):
		status := "FAIL"
		if !httpChallenge {
			status = "warn"
		}
		return dockererr.Check{Name: name, Status: status, Detail: fmt.Sprintf(
			"no answer from %s within %s: a firewall drops it. Open TCP %d in your provider's firewall or security group, and run ufw allow %d/tcp if ufw is active",
			addr, portProbeTimeout, port, port)}
	default:
		return dockererr.Check{Name: name, Status: "warn", Detail: fmt.Sprintf("could not reach %s: %v", addr, err)}
	}
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) ||
		(errors.As(err, &netErr) && netErr.Timeout())
}

// proxyNames is every name the proxy routes for the app, lower-cased and
// without repeats.
func proxyNames(route config.Proxy) []string {
	seen := map[string]bool{}
	var names []string
	for _, n := range append([]string{route.Host}, route.Hosts...) {
		n = strings.ToLower(strings.TrimSpace(n))
		if n != "" && !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	return names
}

// parseHostAddrs reads the addresses `ip -o addr` (as a.b.c.d/nn) and
// `hostname -I` print.
func parseHostAddrs(out string) []string {
	var addrs []string
	for _, f := range strings.Fields(out) {
		if i := strings.IndexByte(f, '/'); i >= 0 {
			f = f[:i]
		}
		if net.ParseIP(f) != nil {
			addrs = append(addrs, f)
		}
	}
	return addrs
}

// listeningPorts reads /proc/net/tcp{,6}: the second field is the local
// address as hex ip:port, the fourth the state, 0A for LISTEN. nil when the
// output holds no table at all (no /proc on the host).
func listeningPorts(out string) map[int]bool {
	if !strings.Contains(out, "local_address") {
		return nil
	}
	ports := map[int]bool{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || f[3] != "0A" {
			continue
		}
		i := strings.LastIndexByte(f[1], ':')
		if i < 0 {
			continue
		}
		if port, err := strconv.ParseInt(f[1][i+1:], 16, 32); err == nil {
			ports[int(port)] = true
		}
	}
	return ports
}

func normalizeIP(s string) string {
	if ip := net.ParseIP(s); ip != nil {
		return ip.String()
	}
	return s
}

func portOr(port, fallback int) int {
	if port > 0 {
		return port
	}
	return fallback
}

func joinPorts(ports []int) string {
	sorted := append([]int{}, ports...)
	sort.Ints(sorted)
	parts := make([]string, len(sorted))
	for i, p := range sorted {
		parts[i] = ":" + strconv.Itoa(p)
	}
	return strings.Join(parts, " or ")
}
