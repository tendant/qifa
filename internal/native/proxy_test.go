package native

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/tendant/qifa/internal/config"
	"github.com/tendant/qifa/internal/proxy"
)

func TestProxyUnit(t *testing.T) {
	unit := RenderProxyUnit(80, 443)
	for _, want := range []string{
		"User=qifa-proxy\n",
		"Environment=HOME=/var/lib/qifa-proxy\n",
		"Environment=KAMAL_PROXY_SOCKET=/run/qifa-proxy/kamal-proxy.sock\n",
		"ExecStart=/opt/qifa/proxy/bin/kamal-proxy run --http-port 80 --https-port 443\n",
		"UMask=0007\n", // group-writable socket for the deploy user
		"AmbientCapabilities=CAP_NET_BIND_SERVICE\n",
		"StartLimitIntervalSec=0\n",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("proxy unit lacks %q", want)
		}
	}
	if strings.Contains(unit, "User=root") {
		t.Fatal("proxy must not run as root")
	}
}

func TestProxyScriptsParse(t *testing.T) {
	cfg := testConfig()
	cfg.Proxy = config.Proxy{Host: "app.example.com"}
	d := &Deployer{cfg: cfg}
	for name, s := range map[string]string{
		"boot":      proxyBootScript("/var/tmp/qifa-kamal-proxy-1", "deploy", RenderProxyUnit(80, 443)),
		"preflight": preflightScript("/opt/qifa/app", []string{"qifa-app.service"}, true),
	} {
		if out, err := exec.Command("sh", "-n", "-c", s).CombinedOutput(); err != nil {
			t.Errorf("%s does not parse: %v\n%s", name, err, out)
		}
	}
	if !d.routedOn("h1") {
		t.Fatal("web host not routed")
	}
}

func TestNativeRouteTargetsLoopback(t *testing.T) {
	cfg := testConfig()
	cfg.Proxy = config.Proxy{Host: "app.example.com", SSL: true, TLS: &config.TLS{Source: "kamal"}}
	d := &Deployer{cfg: cfg}
	if !strings.HasPrefix(proxy.NativeCLI(), "KAMAL_PROXY_SOCKET='/run/qifa-proxy/kamal-proxy.sock' ") {
		t.Fatalf("CLI: %s", proxy.NativeCLI())
	}
	_ = d.routeProxy() // builds without a live client
	list := `{"app": {"hosts": ["app.example.com"], "path_prefixes": ["/"], "tls": true, "targets": ["127.0.0.1:8080"], "read_targets": [], "state": "running"}, "other": {"hosts": [], "targets": ["x"], "state": "stopped"}}`
	if got, err := routeLine(list, "app"); err != nil || got != "app.example.com -> 127.0.0.1:8080 (running, tls)" {
		t.Fatalf("routeLine = %q, %v", got, err)
	}
	if got, _ := routeLine(list, "missing"); got != "" {
		t.Fatalf("missing service: %q", got)
	}
	if _, err := routeLine("Service Host\n", "app"); err == nil {
		t.Fatal("table output accepted as JSON")
	}
}

func TestNativeConfigRoutedOnlyOnProxyRole(t *testing.T) {
	cfg := testConfig()
	cfg.Servers["worker"] = config.Server{Hosts: []string{"h3"}, Exec: []string{"w"}}
	cfg.Proxy = config.Proxy{Host: "app.example.com"}
	d := &Deployer{cfg: cfg}
	if d.routedOn("h3") || !d.routedOn("h1") {
		t.Fatal("route registration must follow the web role")
	}
	if got := strings.Join(d.proxyHosts(), ","); got != "h1,h2" {
		t.Fatalf("proxy hosts = %s", got)
	}
}
