package deploy

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/tendant/qifa/internal/config"
	"github.com/tendant/qifa/internal/dockererr"
)

var (
	errRefused = &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	errTimeout = &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}
)

// fakeProbe answers lookups from dns and dials from ports ("ip:port" →
// error, nil meaning open; anything missing is refused). dialed records each
// address tried.
type fakeProbe struct {
	dns    map[string][]string
	ports  map[string]error
	dialed []string
}

func (f *fakeProbe) probe() domainProbe {
	return domainProbe{
		lookup: func(_ context.Context, name string) ([]string, error) {
			if ips, ok := f.dns[name]; ok {
				return ips, nil
			}
			return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
		},
		dial: func(_ context.Context, address string) error {
			f.dialed = append(f.dialed, address)
			if err, ok := f.ports[address]; ok {
				return err
			}
			return errRefused
		},
	}
}

func checkNamed(checks []dockererr.Check, name string) (dockererr.Check, bool) {
	for _, c := range checks {
		if c.Name == name {
			return c, true
		}
	}
	return dockererr.Check{}, false
}

func failures(checks []dockererr.Check) int {
	n := 0
	for _, c := range checks {
		if c.Status == "FAIL" {
			n++
		}
	}
	return n
}

func httpsRoute(name string) config.Proxy {
	return config.Proxy{Host: name, SSL: true, TLS: &config.TLS{Source: "kamal"}}
}

// A fresh server ready for its first deploy: the name points at it, the
// ports are open through the firewall but nothing listens yet.
func TestDomainChecksPassForAReadyServer(t *testing.T) {
	f := &fakeProbe{dns: map[string][]string{"app.example.com": {"203.0.113.10"}}}
	checks := domainChecks(t.Context(), f.probe(), domainInput{
		route: httpsRoute("App.Example.com"), sshAddr: "203.0.113.10", listening: map[int]bool{22: true},
	})
	for _, c := range checks {
		if c.Status != "ok" {
			t.Errorf("%s: %s %s", c.Name, c.Status, c.Detail)
		}
	}
	for _, name := range []string{"dns app.example.com", "port 80", "port 443", "ports free"} {
		if _, ok := checkNamed(checks, name); !ok {
			t.Errorf("no %q check in %+v", name, checks)
		}
	}
}

func TestDomainChecksDNS(t *testing.T) {
	for _, tc := range []struct {
		name      string
		dns       map[string][]string
		hostAddrs []string
		status    string
		want      string
		probed    string
	}{
		{"no record", nil, nil, "warn", "does not resolve from this machine: add an A record pointing at 10.0.0.5; the certificate can't be issued", "10.0.0.5:80"},
		{"elsewhere", map[string][]string{"app.example.com": {"198.51.100.7"}}, nil, "warn", "points at 198.51.100.7, not this host (10.0.0.5)", "10.0.0.5:80"},
		// qifa reaches the host on a private address; the name points at a
		// public one the host owns, so that is the address to probe.
		{"public address on the host", map[string][]string{"app.example.com": {"203.0.113.10"}}, []string{"10.0.0.5", "203.0.113.10"}, "ok", "points at 203.0.113.10, this host", "203.0.113.10:80"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeProbe{dns: tc.dns}
			checks := domainChecks(t.Context(), f.probe(), domainInput{
				route: httpsRoute("app.example.com"), sshAddr: "10.0.0.5", hostAddrs: tc.hostAddrs,
			})
			c, _ := checkNamed(checks, "dns app.example.com")
			if c.Status != tc.status || !strings.Contains(c.Detail, tc.want) {
				t.Errorf("got %s %q", c.Status, c.Detail)
			}
			if len(f.dialed) == 0 || f.dialed[0] != tc.probed {
				t.Errorf("probed %v, want %s first", f.dialed, tc.probed)
			}
			if failures(checks) != 0 {
				t.Errorf("a DNS finding failed doctor: %+v", checks)
			}
		})
	}
}

// A port that times out is a firewall dropping packets: fatal for a
// Let's Encrypt HTTP-01 certificate, a warning when the certificate comes
// another way.
func TestDomainChecksPortTimeout(t *testing.T) {
	for _, tc := range []struct {
		name   string
		route  config.Proxy
		status string
	}{
		{"http-01", httpsRoute("app.example.com"), "FAIL"},
		{"dns-01", config.Proxy{Host: "app.example.com", SSL: true, TLS: &config.TLS{Source: "qifa"}}, "warn"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeProbe{
				dns:   map[string][]string{"app.example.com": {"203.0.113.10"}},
				ports: map[string]error{"203.0.113.10:443": errTimeout},
			}
			checks := domainChecks(t.Context(), f.probe(), domainInput{route: tc.route, sshAddr: "203.0.113.10"})
			c, _ := checkNamed(checks, "port 443")
			if c.Status != tc.status || !strings.Contains(c.Detail, "firewall") || !strings.Contains(c.Detail, "ufw allow 443/tcp") {
				t.Errorf("got %s %q", c.Status, c.Detail)
			}
			if c, _ := checkNamed(checks, "port 80"); c.Status != "ok" {
				t.Errorf("port 80: %s %q", c.Status, c.Detail)
			}
		})
	}
}

func TestDomainChecksPortsTaken(t *testing.T) {
	f := &fakeProbe{
		dns:   map[string][]string{"app.example.com": {"203.0.113.10"}},
		ports: map[string]error{"203.0.113.10:80": nil},
	}
	in := domainInput{route: httpsRoute("app.example.com"), sshAddr: "203.0.113.10", listening: map[int]bool{80: true}}
	checks := domainChecks(t.Context(), f.probe(), in)
	c, _ := checkNamed(checks, "ports free")
	if c.Status != "FAIL" || !strings.Contains(c.Detail, "already listens on :80, so kamal-proxy can't start") {
		t.Errorf("got %s %q", c.Status, c.Detail)
	}

	// Once kamal-proxy runs, it is what holds the ports.
	in.proxyRunning = true
	checks = domainChecks(t.Context(), f.probe(), in)
	if _, ok := checkNamed(checks, "ports free"); ok {
		t.Errorf("checked for free ports with kamal-proxy running: %+v", checks)
	}
	if c, _ := checkNamed(checks, "port 80"); c.Status != "ok" || !strings.Contains(c.Detail, "kamal-proxy") {
		t.Errorf("port 80: %s %q", c.Status, c.Detail)
	}
	if c, _ := checkNamed(checks, "port 443"); c.Status != "warn" || !strings.Contains(c.Detail, "refuses connections though kamal-proxy runs") {
		t.Errorf("port 443: %s %q", c.Status, c.Detail)
	}
}

// Without ssl only port 80 matters, on the proxy's configured port; a host
// whose listeners are unknown gets no "ports free" verdict.
func TestDomainChecksPlainHTTPOnACustomPort(t *testing.T) {
	f := &fakeProbe{dns: map[string][]string{"app.example.com": {"203.0.113.10"}}}
	checks := domainChecks(t.Context(), f.probe(), domainInput{
		route: config.Proxy{Host: "app.example.com"}, boot: config.ProxyBoot{HTTPPort: 8080}, sshAddr: "203.0.113.10",
	})
	if len(f.dialed) != 1 || f.dialed[0] != "203.0.113.10:8080" {
		t.Errorf("dialed %v", f.dialed)
	}
	if _, ok := checkNamed(checks, "ports free"); ok {
		t.Errorf("a ports-free verdict without the host's listeners: %+v", checks)
	}
	if c, _ := checkNamed(checks, "dns app.example.com"); strings.Contains(c.Detail, "certificate") {
		t.Errorf("mentions a certificate without ssl: %q", c.Detail)
	}
}

func TestListeningPorts(t *testing.T) {
	out := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:0050 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 1 0000000000000000 100 0 0 10 0
   1: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 2 1 0000000000000000 100 0 0 10 0
   2: 0500000A:0016 0B00000A:D431 01 00000000:00000000 02:000A7B0A 00000000     0        0 3 4 0000000000000000 20 4 30 10 -1
  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000000000000:01BB 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 4 1 0000000000000000 100 0 0 10 0
`
	got := listeningPorts(out)
	for _, port := range []int{80, 8080, 443} {
		if !got[port] {
			t.Errorf("%d not listening in %v", port, got)
		}
	}
	if got[22] {
		t.Errorf("an established connection counted as listening: %v", got)
	}
	if listeningPorts("") != nil {
		t.Error("no /proc should mean unknown, not none")
	}
}

func TestParseHostAddrs(t *testing.T) {
	got := parseHostAddrs("127.0.0.1/8\n10.0.0.5/24\n::1/128\nfe80::1/64\n10.0.0.5 203.0.113.10 \n")
	want := "127.0.0.1 10.0.0.5 ::1 fe80::1 10.0.0.5 203.0.113.10"
	if strings.Join(got, " ") != want {
		t.Errorf("got %v", got)
	}
}

func TestIsTimeout(t *testing.T) {
	if !isTimeout(errTimeout) || !isTimeout(context.DeadlineExceeded) || isTimeout(errRefused) || isTimeout(errors.New("x")) {
		t.Error("misclassified")
	}
}
