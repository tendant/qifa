package native

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/tendant/qifa/internal/config"
	"github.com/tendant/qifa/internal/proxy"
)

// kamal-proxy as a systemd service: the native counterpart of the proxy
// container. One per host, shared by every native app on it, booted once as
// root (`qifa proxy boot --ssh-user root`). Apps register routes with it at
// deploy time through its socket, which the deploy user reaches by group
// membership: no sudo is involved in routing.

// proxyHosts are the hosts that run the proxy: proxy_boot.hosts, or the
// hosts of the routed role.
func (d *Deployer) proxyHosts() []string {
	if len(d.cfg.ProxyBoot.Hosts) > 0 {
		return d.cfg.ProxyBoot.Hosts
	}
	if role := d.cfg.NativeProxyRole(); role != "" {
		return d.cfg.Servers[role].Hosts
	}
	return nil
}

// routedOn reports whether host serves the routed role, so a deploy there
// registers the app with the host's proxy.
func (d *Deployer) routedOn(host string) bool {
	if !d.cfg.NativeProxied() {
		return false
	}
	for _, role := range d.rolesOn(host) {
		if role == d.cfg.NativeProxyRole() {
			return true
		}
	}
	return false
}

// routeProxy is the kamal-proxy client for this app's routes. kamal-proxy
// health-checks the target itself before routing to it, using the app's own
// health check path; each check gets 5s, the deploy timeout bounds them all.
func (d *Deployer) routeProxy() *proxy.KamalProxy {
	app := d.cfg.Proxy
	hc := d.cfg.Native.Healthcheck
	app.Healthcheck = config.Healthcheck{Path: hc.Path, Interval: hc.Interval, Timeout: 5 * time.Second, Port: hc.Port}
	return proxy.NewNative(d.ssh, app)
}

func (d *Deployer) registerRoute(ctx context.Context, host string) error {
	target := proxy.Target{Service: d.cfg.Service, Host: "127.0.0.1", Port: d.cfg.Native.Port}
	d.log.Printf("%s: routing %s to 127.0.0.1:%d", host, strings.Join(d.routeNames(), ", "), d.cfg.Native.Port)
	if err := d.routeProxy().Deploy(ctx, host, target); err != nil {
		return fmt.Errorf("%s: the new release is running, but kamal-proxy did not accept the route: %w", host, err)
	}
	return nil
}

func (d *Deployer) routeNames() []string {
	var names []string
	if d.cfg.Proxy.Host != "" {
		names = append(names, d.cfg.Proxy.Host)
	}
	names = append(names, d.cfg.Proxy.Hosts...)
	names = append(names, d.cfg.Proxy.PathPrefixes...)
	return names
}

// RenderProxyUnit renders qifa-proxy.service. kamal-proxy keeps its routes
// and certificates under $HOME/.config/kamal-proxy, so HOME is the state
// directory; the socket path is fixed with KAMAL_PROXY_SOCKET (kamal-proxy
// v0.10+), and UMask 0007 makes the socket group-writable so members of the
// qifa-proxy group can drive it. Ports 80/443 come from the capability, not
// root.
func RenderProxyUnit(httpPort, httpsPort int) string {
	return fmt.Sprintf(`# Managed by qifa (qifa proxy boot). Local edits are overwritten.
[Unit]
Description=qifa: kamal-proxy
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=0

[Service]
Type=exec
User=%[1]s
Group=%[1]s
Environment=HOME=%[2]s
Environment=KAMAL_PROXY_SOCKET=%[3]s
ExecStart=%[4]s run --http-port %[5]d --https-port %[6]d
RuntimeDirectory=qifa-proxy
RuntimeDirectoryMode=0750
StateDirectory=qifa-proxy
StateDirectoryMode=0750
UMask=0007
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=read-only
PrivateTmp=true
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
`, proxy.NativeUser, proxy.NativeStateDir, proxy.NativeSocket, proxy.NativeBinary, httpPort, httpsPort)
}

func (d *Deployer) proxyPorts() (int, int) {
	http, https := d.cfg.ProxyBoot.HTTPPort, d.cfg.ProxyBoot.HTTPSPort
	if http == 0 {
		http = 80
	}
	if https == 0 {
		https = 443
	}
	return http, https
}

// ProxyBoot installs or upgrades the native proxy on every proxy host, as
// root. Idempotent: it restarts the proxy only when the binary or unit
// changed. Routes survive, being in the state directory.
func (d *Deployer) ProxyBoot(ctx context.Context, deployUser string) error {
	if err := checkDeployUser(deployUser, d.cfg.SSH.User); err != nil {
		return err
	}
	if deployUser == "" {
		deployUser = d.cfg.SSH.User
	}
	hosts := d.proxyHosts()
	if len(hosts) == 0 {
		return errors.New("no proxy hosts: set proxy_boot.hosts, or proxy.host on the app so its web hosts run the proxy")
	}
	for _, host := range hosts {
		m, err := d.ssh.Run(ctx, host, "uname -m")
		if err != nil {
			return fmt.Errorf("%s: %w", host, err)
		}
		arch := UnameArch(m)
		bin, err := d.proxyBinary(ctx, arch)
		if err != nil {
			return err
		}
		upload := fmt.Sprintf("/var/tmp/qifa-kamal-proxy-%d", time.Now().UnixNano())
		d.log.Printf("%s: uploading kamal-proxy %s (%s, %d KiB)", host, d.cfg.ProxyBoot.Version, arch, len(bin)/1024)
		if err := d.ssh.Upload(ctx, host, upload, bin, 0o700); err != nil {
			return fmt.Errorf("%s: upload kamal-proxy: %w", host, err)
		}
		http, https := d.proxyPorts()
		script := proxyBootScript(upload, deployUser, RenderProxyUnit(http, https))
		if err := d.runAsRoot(ctx, host, "proxy boot", script); err != nil {
			return err
		}
	}
	d.log.Printf("proxy running; `qifa deploy` registers routes as %s", deployUser)
	return nil
}

func checkDeployUser(flag, sshUser string) error {
	u := flag
	if u == "" {
		u = sshUser
	}
	if u == "" {
		return errors.New("the deploy user is needed: set ssh.user in the config, or pass --deploy-user")
	}
	if !deployUserRe.MatchString(u) || u == "root" {
		return fmt.Errorf("deploy user %q: must be a normal account name, not root", u)
	}
	return nil
}

// runAsRoot runs script as root on host (directly, or through passwordless
// sudo), logging its output.
func (d *Deployer) runAsRoot(ctx context.Context, host, what, script string) error {
	wrapped := fmt.Sprintf(`if [ "$(id -u)" = 0 ]; then sh -c %[1]s; else sudo -n sh -c %[1]s; fi`, q(script))
	out, err := d.ssh.Run(ctx, host, wrapped)
	for _, line := range strings.Split(out, "\n") {
		if line != "" {
			d.log.Printf("%s %s: %s", what, host, line)
		}
	}
	if err != nil {
		return fmt.Errorf("%s on %s (needs root, or passwordless sudo — try --ssh-user root): %w", what, host, err)
	}
	return nil
}

func proxyBootScript(upload, deployUser, unit string) string {
	bin := proxy.NativeBinary
	var b strings.Builder
	fmt.Fprintf(&b, "set -eu\nsrc=%s\nbin=%s\ndeploy=%s\nuser=%s\nunit=%s\nsock=%s\n",
		q(upload), q(bin), q(deployUser), q(proxy.NativeUser), q(proxy.NativeUnit), q(proxy.NativeSocket))
	b.WriteString(`trap 'rm -f "$src"' EXIT
command -v systemctl >/dev/null 2>&1 || { echo "systemd is required on this host" >&2; exit 1; }
id "$deploy" >/dev/null 2>&1 || { echo "deploy user $deploy does not exist; create it first (docs/deploying.md)" >&2; exit 1; }
if docker ps -q --filter name='^kamal-proxy$' 2>/dev/null | grep -q .; then
  echo "the kamal-proxy container is running on this host and owns ports 80/443; a host runs one proxy or the other" >&2
  exit 1
fi
if ! id "$user" >/dev/null 2>&1; then
  nologin=$(command -v nologin || echo /usr/sbin/nologin)
  useradd --system --user-group --home-dir /var/lib/qifa-proxy --no-create-home --shell "$nologin" "$user"
  echo "created service user $user"
fi
restart=0
install -d -m 0755 "$(dirname "$bin")"
if ! cmp -s "$src" "$bin"; then
  install -m 0755 -o root -g root "$src" "$bin.new"
  mv -f "$bin.new" "$bin"
  echo "installed kamal-proxy $("$bin" version 2>/dev/null || echo '(version unknown)')"
  restart=1
fi
`)
	fmt.Fprintf(&b, `printf '%%s' %s > "/etc/systemd/system/.$unit.qifa"
if cmp -s "/etc/systemd/system/.$unit.qifa" "/etc/systemd/system/$unit"; then
  rm -f "/etc/systemd/system/.$unit.qifa"
else
  chmod 0644 "/etc/systemd/system/.$unit.qifa"
  mv -f "/etc/systemd/system/.$unit.qifa" "/etc/systemd/system/$unit"
  systemctl daemon-reload
  echo "installed unit $unit"
  restart=1
fi
`, q(unit))
	b.WriteString(`systemctl=$(command -v systemctl)
tmp=$(mktemp)
printf '# Managed by qifa proxy boot.\n%s ALL=(root) NOPASSWD: %s start %s, %s stop %s, %s restart %s\n' "$deploy" "$systemctl" "$unit" "$systemctl" "$unit" "$systemctl" "$unit" > "$tmp"
if ! visudo -cqf "$tmp"; then rm -f "$tmp"; echo "generated sudoers rule failed visudo -c" >&2; exit 1; fi
chmod 0440 "$tmp"
if cmp -s "$tmp" /etc/sudoers.d/qifa-proxy; then rm -f "$tmp"; else mv -f "$tmp" /etc/sudoers.d/qifa-proxy; echo "installed /etc/sudoers.d/qifa-proxy"; fi
for g in "$user" systemd-journal; do
  if getent group "$g" >/dev/null 2>&1 && ! id -nG "$deploy" | tr ' ' '\n' | grep -qx "$g"; then
    usermod -aG "$g" "$deploy"
    echo "added $deploy to $g"
  fi
done
systemctl enable --quiet "$unit"
if [ "$restart" = 1 ]; then systemctl restart "$unit"; else systemctl start "$unit"; fi
i=0
while [ ! -S "$sock" ]; do
  i=$((i+1))
  if [ "$i" -ge 40 ]; then
    echo "kamal-proxy did not open $sock:" >&2
    journalctl -u "$unit" -n 20 --no-pager >&2 || true
    exit 1
  fi
  sleep 0.25
done
echo "kamal-proxy is running"
`)
	return b.String()
}

// proxyBinary returns a Linux kamal-proxy for arch: proxy_boot.binary, or one
// built from source at proxy_boot.version (kamal-proxy publishes no binaries,
// only an image) and cached on the deployer.
func (d *Deployer) proxyBinary(ctx context.Context, arch string) ([]byte, error) {
	if p := d.cfg.ProxyBoot.Binary; p != "" {
		got, err := ELFArch(p)
		if err != nil {
			return nil, fmt.Errorf("proxy_boot.binary: %w", err)
		}
		if got != arch {
			return nil, fmt.Errorf("proxy_boot.binary %s is built for %q, the host is %s", p, got, arch)
		}
		return os.ReadFile(p)
	}
	v := d.cfg.ProxyBoot.Version
	cache, err := os.UserCacheDir()
	if err != nil {
		return nil, err
	}
	out := filepath.Join(cache, "qifa", "kamal-proxy", v, "linux-"+arch, "kamal-proxy")
	if data, err := os.ReadFile(out); err == nil {
		return data, nil
	}
	if _, err := exec.LookPath("go"); err != nil {
		return nil, fmt.Errorf("kamal-proxy publishes no binaries, so qifa builds %s with Go, and go is not installed here: install Go, or set proxy_boot.binary to a linux/%s kamal-proxy", v, arch)
	}
	d.log.Printf("building kamal-proxy %s for linux/%s (cached in %s)", v, arch, filepath.Dir(out))
	if err := buildKamalProxy(ctx, v, arch, out, d.stderr); err != nil {
		return nil, err
	}
	return os.ReadFile(out)
}

// buildKamalProxy cross-compiles kamal-proxy at version v in a scratch module,
// stamping the version so `kamal-proxy version` reports it.
func buildKamalProxy(ctx context.Context, v, arch, out string, stderr io.Writer) error {
	work, err := os.MkdirTemp("", "qifa-kamal-proxy-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	tmpOut := filepath.Join(work, "kamal-proxy")
	steps := [][]string{
		{"go", "mod", "init", "qifa-kamal-proxy-build"},
		{"go", "get", "github.com/basecamp/kamal-proxy@" + v},
		{"go", "build", "-trimpath", "-ldflags", "-s -w -X github.com/basecamp/kamal-proxy/internal/version.Version=" + v,
			"-o", tmpOut, "github.com/basecamp/kamal-proxy/cmd/kamal-proxy"},
	}
	for _, step := range steps {
		cmd := exec.CommandContext(ctx, step[0], step[1:]...)
		cmd.Dir = work
		cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0", "GOFLAGS=-mod=mod")
		cmd.Stdout, cmd.Stderr = stderr, stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("building kamal-proxy %s (%s): %w", v, strings.Join(step, " "), err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	return os.Rename(tmpOut, out)
}

// ProxyRemove undoes ProxyBoot, as root. Routes and certificates are kept
// unless purge, which also deletes the state directory and account.
func (d *Deployer) ProxyRemove(ctx context.Context, purge bool) error {
	var b strings.Builder
	fmt.Fprintf(&b, "set -eu\nunit=%s\n", q(proxy.NativeUnit))
	b.WriteString(`systemctl disable --now "$unit" >/dev/null 2>&1 || true
if [ -e "/etc/systemd/system/$unit" ]; then rm -f "/etc/systemd/system/$unit"; systemctl daemon-reload; echo "removed unit $unit"; fi
if [ -e /etc/sudoers.d/qifa-proxy ]; then rm -f /etc/sudoers.d/qifa-proxy; echo "removed /etc/sudoers.d/qifa-proxy"; fi
`)
	fmt.Fprintf(&b, "if [ -e %[1]s ]; then rm -rf -- %[1]s; echo \"removed %[2]s\"; fi\n", q(filepath.Dir(filepath.Dir(proxy.NativeBinary))), filepath.Dir(filepath.Dir(proxy.NativeBinary)))
	if purge {
		fmt.Fprintf(&b, "if [ -e %[1]s ]; then rm -rf -- %[1]s; echo \"deleted %[2]s (routes, certificates)\"; fi\n", q(proxy.NativeStateDir), proxy.NativeStateDir)
		fmt.Fprintf(&b, "if id %[1]s >/dev/null 2>&1; then userdel %[1]s; echo \"deleted user %[2]s\"; fi\n", q(proxy.NativeUser), proxy.NativeUser)
	} else {
		fmt.Fprintf(&b, "echo \"kept %s (routes, certificates); --purge deletes it\"\n", proxy.NativeStateDir)
	}
	for _, host := range d.proxyHosts() {
		if err := d.runAsRoot(ctx, host, "proxy remove", b.String()); err != nil {
			return err
		}
	}
	return nil
}

// ProxyCtl starts, stops or restarts the proxy service as the deploy user,
// through the sudoers rule proxy boot installs.
func (d *Deployer) ProxyCtl(ctx context.Context, verb string) error {
	for _, host := range d.proxyHosts() {
		d.log.Printf("%s: systemctl %s %s", host, verb, proxy.NativeUnit)
		if _, err := d.ssh.Sudo(ctx, host, "systemctl", verb, proxy.NativeUnit); err != nil {
			return d.withDiagnostics(ctx, host, proxy.NativeUnit, err)
		}
	}
	return nil
}

func (d *Deployer) ProxyLogs(ctx context.Context, lines int, follow bool, out io.Writer) error {
	hosts := d.proxyHosts()
	if len(hosts) == 0 {
		return errors.New("no proxy hosts configured")
	}
	cmd := fmt.Sprintf("journalctl --no-pager -u %s -n %d", q(proxy.NativeUnit), lines)
	if follow {
		cmd += " -f"
	}
	return d.ssh.Stream(ctx, hosts[0], cmd, out)
}

// ProxyDetails prints the routes registered on each proxy host.
func (d *Deployer) ProxyDetails(ctx context.Context, out io.Writer) error {
	for _, host := range d.proxyHosts() {
		fmt.Fprintf(out, "%s:\n", host)
		if err := d.ssh.Stream(ctx, host, proxy.NativeCLI()+" list", out); err != nil {
			return fmt.Errorf("%s: %w", host, err)
		}
	}
	return nil
}

// Maintenance answers the app's routes with a maintenance page, draining
// in-flight requests; Live routes them to the app again.
func (d *Deployer) Maintenance(ctx context.Context, message string, drainTimeout time.Duration) error {
	if !d.cfg.NativeProxied() {
		return errors.New("maintenance mode works through kamal-proxy: set proxy.host to route this app")
	}
	for _, host := range d.proxyHosts() {
		if err := d.routeProxy().Stop(ctx, host, d.cfg.Service, message, drainTimeout); err != nil {
			return fmt.Errorf("%s: %w", host, err)
		}
		d.log.Printf("%s: %s in maintenance", host, d.cfg.Service)
	}
	return nil
}

func (d *Deployer) Live(ctx context.Context) error {
	if !d.cfg.NativeProxied() {
		return errors.New("maintenance mode works through kamal-proxy: set proxy.host to route this app")
	}
	for _, host := range d.proxyHosts() {
		if err := d.routeProxy().Resume(ctx, host, d.cfg.Service); err != nil {
			return fmt.Errorf("%s: %w", host, err)
		}
		d.log.Printf("%s: %s live", host, d.cfg.Service)
	}
	return nil
}
