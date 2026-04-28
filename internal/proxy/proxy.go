package proxy

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/gokamal/gocart/internal/config"
	"github.com/gokamal/gocart/internal/ssh"
)

// Proxy is the runtime interface used by the deployer when registering and
// deregistering app routes. The proxy container itself is managed
// independently via the lifecycle methods on KamalProxy (Boot, Upgrade, etc.).
type Proxy interface {
	Deploy(context.Context, string, Target) error
	Remove(context.Context, string, string) error
	Stop(ctx context.Context, host, service, message string, drainTimeout time.Duration) error
	Resume(ctx context.Context, host, service string) error
}

type Target struct {
	Service string
	Host    string
	Port    int
}

type KamalProxy struct {
	client *ssh.Client
	app    config.Proxy
	boot   config.ProxyBoot
}

const proxyContainerName = "kamal-proxy"

func New(client *ssh.Client, app config.Proxy, boot config.ProxyBoot) *KamalProxy {
	return &KamalProxy{client: client, app: app, boot: boot}
}

// Boot starts the kamal-proxy container on every host listed in
// boot.hosts. Idempotent: hosts where the container is already running are
// skipped. Creates the docker network and state volume on first boot.
func (k *KamalProxy) Boot(ctx context.Context) error {
	for _, host := range k.boot.Hosts {
		if _, err := k.client.Run(ctx, host, k.bootCommand()); err != nil {
			return fmt.Errorf("boot proxy on %s: %w", host, err)
		}
	}
	return nil
}

// EnsureRunning checks whether the proxy container is up on host. Returns a
// clear error if not, pointing the user at `qifa proxy boot`.
func (k *KamalProxy) EnsureRunning(ctx context.Context, host string) error {
	out, err := k.client.Run(ctx, host, "docker ps --filter "+shellQuote("name=^"+proxyContainerName)+" --format '{{.Names}}'")
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) == proxyContainerName {
		return nil
	}
	return fmt.Errorf("kamal-proxy is not running on %s — run `qifa proxy boot` first", host)
}

func (k *KamalProxy) Start(ctx context.Context) error {
	for _, host := range k.boot.Hosts {
		if _, err := k.client.Run(ctx, host, "docker start "+proxyContainerName); err != nil {
			return fmt.Errorf("start proxy on %s: %w", host, err)
		}
	}
	return nil
}

func (k *KamalProxy) StopProxy(ctx context.Context) error {
	for _, host := range k.boot.Hosts {
		if _, err := k.client.Run(ctx, host, "docker stop "+proxyContainerName+" >/dev/null 2>&1 || true"); err != nil {
			return fmt.Errorf("stop proxy on %s: %w", host, err)
		}
	}
	return nil
}

func (k *KamalProxy) Restart(ctx context.Context) error {
	if err := k.StopProxy(ctx); err != nil {
		return err
	}
	return k.Start(ctx)
}

// Upgrade re-creates the proxy container with the current boot config (e.g.
// to pick up a new image version). The state volume survives so registered
// routes persist.
func (k *KamalProxy) Upgrade(ctx context.Context) error {
	for _, host := range k.boot.Hosts {
		// Force recreate: rm then run.
		if _, err := k.client.Run(ctx, host, "docker rm -f "+proxyContainerName+" >/dev/null 2>&1 || true"); err != nil {
			return fmt.Errorf("upgrade (rm) on %s: %w", host, err)
		}
		if _, err := k.client.Run(ctx, host, k.bootCommand()); err != nil {
			return fmt.Errorf("upgrade (boot) on %s: %w", host, err)
		}
	}
	return nil
}

// RemoveProxy stops and removes the proxy container on every host. Does NOT
// remove the state volume (so route state survives) unless purge is true.
func (k *KamalProxy) RemoveProxy(ctx context.Context, purge bool) error {
	for _, host := range k.boot.Hosts {
		if _, err := k.client.Run(ctx, host, "docker rm -f "+proxyContainerName+" >/dev/null 2>&1 || true"); err != nil {
			return fmt.Errorf("remove proxy on %s: %w", host, err)
		}
		if purge {
			vol := k.boot.StateVolume
			if vol == "" {
				vol = "kamal-proxy-config"
			}
			if _, err := k.client.Run(ctx, host, "docker volume rm "+shellQuote(vol)+" >/dev/null 2>&1 || true"); err != nil {
				return fmt.Errorf("purge volume on %s: %w", host, err)
			}
		}
	}
	return nil
}

// Logs streams `docker logs --tail N [--follow] kamal-proxy` from each host
// to out, prefixed with the host name when there's more than one.
func (k *KamalProxy) Logs(ctx context.Context, lines int, follow bool, out io.Writer) error {
	cmd := fmt.Sprintf("docker logs --tail %d", lines)
	if follow {
		cmd += " --follow"
	}
	cmd += " " + proxyContainerName
	for _, host := range k.boot.Hosts {
		w := out
		if len(k.boot.Hosts) > 1 {
			fmt.Fprintf(out, "\n=== %s ===\n", host)
		}
		if err := k.client.Stream(ctx, host, cmd, w); err != nil {
			return err
		}
	}
	return nil
}

// Details prints `docker exec kamal-proxy kamal-proxy list` on each host so
// operators can see what's currently registered.
func (k *KamalProxy) Details(ctx context.Context, out io.Writer) error {
	for _, host := range k.boot.Hosts {
		if len(k.boot.Hosts) > 1 {
			fmt.Fprintf(out, "\n=== %s ===\n", host)
		}
		if err := k.client.Stream(ctx, host, "docker exec "+proxyContainerName+" kamal-proxy list", out); err != nil {
			return err
		}
	}
	return nil
}

func (k *KamalProxy) bootCommand() string {
	httpPort := k.boot.HTTPPort
	if httpPort == 0 {
		httpPort = 80
	}
	httpsPort := k.boot.HTTPSPort
	if httpsPort == 0 {
		httpsPort = 443
	}
	imageRef := k.boot.Image
	if imageRef == "" {
		imageRef = "basecamp/kamal-proxy"
	}
	if k.boot.Version != "" {
		imageRef = imageRef + ":" + k.boot.Version
	}
	network := k.boot.Network
	if network == "" {
		network = "kamal"
	}
	stateVolume := k.boot.StateVolume
	if stateVolume == "" {
		stateVolume = "kamal-proxy-config"
	}
	appsConfigDir := k.boot.AppsConfigDir
	if appsConfigDir == "" {
		appsConfigDir = ".kamal/proxy/apps-config"
	}
	publishFlags := publishArgs(httpPort, httpsPort, k.boot.BindIPs)
	command := strings.Join([]string{
		"docker network create " + shellQuote(network) + " >/dev/null 2>&1 || true",
		"mkdir -p " + shellQuote(appsConfigDir),
		"docker ps --filter " + shellQuote("name=^"+proxyContainerName) + " --format '{{.Names}}' | grep -qx " + shellQuote(proxyContainerName) + " || (docker rm -f " + shellQuote(proxyContainerName) + " >/dev/null 2>&1 || true; docker run -d --restart unless-stopped --name " + shellQuote(proxyContainerName) + " --network " + shellQuote(network) + " --volume " + shellQuote(stateVolume+":/home/kamal-proxy/.config/kamal-proxy") + " --volume " + shellQuote(appsConfigDir+":/home/kamal-proxy/.apps-config") + " " + publishFlags + " --log-opt max-size=10m " + shellQuote(imageRef) + " kamal-proxy run)",
		"for i in 1 2 3 4 5; do docker ps --filter " + shellQuote("name=^"+proxyContainerName) + " --format '{{.Names}}' | grep -qx " + shellQuote(proxyContainerName) + " && exit 0; sleep 1; done; exit 1",
	}, " && ")
	return command
}

// publishArgs returns the docker -p flags for the proxy. With no bindIPs,
// docker binds to 0.0.0.0 by default (one -p per port). With bindIPs, it
// emits one -p per IP per port so the proxy listens only on those IPs.
// IPv6 addresses are wrapped in [brackets] per docker's syntax.
func publishArgs(httpPort, httpsPort int, bindIPs []string) string {
	if len(bindIPs) == 0 {
		return fmt.Sprintf("-p %d:80 -p %d:443", httpPort, httpsPort)
	}
	var args []string
	for _, ip := range bindIPs {
		ip = formatBindIP(ip)
		args = append(args, fmt.Sprintf("-p %s:%d:80", ip, httpPort))
		args = append(args, fmt.Sprintf("-p %s:%d:443", ip, httpsPort))
	}
	return strings.Join(args, " ")
}

func formatBindIP(ip string) string {
	if strings.Contains(ip, ":") && !strings.HasPrefix(ip, "[") {
		return "[" + ip + "]"
	}
	return ip
}

func (k *KamalProxy) Deploy(ctx context.Context, host string, target Target) error {
	command := "docker exec " + shellQuote(proxyContainerName) + " " + k.deployCommand(target)
	_, err := k.client.Run(ctx, host, command)
	return err
}

func (k *KamalProxy) Remove(ctx context.Context, host, service string) error {
	_, err := k.client.Run(ctx, host, "docker exec "+shellQuote(proxyContainerName)+" kamal-proxy remove "+shellQuote(service))
	return err
}

// Stop puts the service into maintenance mode: kamal-proxy returns the
// configured message (default 503-style) for incoming requests and drains
// in-flight requests over drainTimeout.
func (k *KamalProxy) Stop(ctx context.Context, host, service, message string, drainTimeout time.Duration) error {
	args := []string{"docker", "exec", shellQuote(proxyContainerName),
		"kamal-proxy", "stop", shellQuote(service)}
	if drainTimeout > 0 {
		args = append(args, "--drain-timeout", shellQuote(drainTimeout.String()))
	}
	if message != "" {
		args = append(args, "--message", shellQuote(message))
	}
	_, err := k.client.Run(ctx, host, strings.Join(args, " "))
	return err
}

// Resume brings a stopped service back online — kamal-proxy resumes routing
// requests to the previously registered target.
func (k *KamalProxy) Resume(ctx context.Context, host, service string) error {
	_, err := k.client.Run(ctx, host, "docker exec "+shellQuote(proxyContainerName)+" kamal-proxy resume "+shellQuote(service))
	return err
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func (k *KamalProxy) deployCommand(target Target) string {
	args := []string{"kamal-proxy", "deploy", shellQuote(target.Service)}
	for _, host := range k.hosts() {
		args = append(args, "--host", shellQuote(host))
	}
	healthPath := k.app.Healthcheck.Path
	if healthPath == "" {
		healthPath = "/up"
	}
	args = append(args,
		"--target", shellQuote(fmt.Sprintf("%s:%d", target.Host, target.Port)),
		"--health-check-path", shellQuote(healthPath),
		"--health-check-interval", shellQuote(k.app.Healthcheck.Interval.String()),
		"--health-check-timeout", shellQuote(k.app.Healthcheck.Timeout.String()),
		"--deploy-timeout", shellQuote(k.app.DeployTimeout.String()),
		"--drain-timeout", shellQuote(k.app.DrainTimeout.String()),
		"--target-timeout", shellQuote(k.app.TargetTimeout.String()),
	)
	if k.app.SSL {
		args = append(args, "--tls")
		if k.app.TLSRedirect != nil {
			args = append(args, "--tls-redirect="+strconv.FormatBool(*k.app.TLSRedirect))
		}
		if k.app.TLS != nil {
			switch k.app.TLS.Source {
			case "kamal":
				if k.app.TLS.Staging {
					args = append(args, "--tls-staging")
				}
			case "qifa":
				certPath, keyPath := QifaCertPaths(primaryHost(k.app))
				args = append(args,
					"--tls-certificate-path", shellQuote(certPath),
					"--tls-private-key-path", shellQuote(keyPath),
				)
			case "static":
				args = append(args,
					"--tls-certificate-path", shellQuote(k.app.TLS.CertPath),
					"--tls-private-key-path", shellQuote(k.app.TLS.KeyPath),
				)
			}
		}
	}
	if k.app.ForwardHeaders != nil {
		args = append(args, "--forward-headers="+strconv.FormatBool(*k.app.ForwardHeaders))
	}
	for _, prefix := range k.app.PathPrefixes {
		args = append(args, "--path-prefix", shellQuote(prefix))
	}
	if k.app.StripPathPrefix != nil {
		args = append(args, "--strip-path-prefix="+strconv.FormatBool(*k.app.StripPathPrefix))
	}
	return strings.Join(args, " ")
}

func (k *KamalProxy) hosts() []string {
	hosts := make([]string, 0, len(k.app.Hosts)+1)
	if k.app.Host != "" {
		hosts = append(hosts, k.app.Host)
	}
	hosts = append(hosts, k.app.Hosts...)
	return hosts
}

// primaryHost returns the canonical hostname used as the cert filename
// for source: qifa. Prefers Proxy.Host; falls back to the first entry
// in Proxy.Hosts.
func primaryHost(p config.Proxy) string {
	if p.Host != "" {
		return p.Host
	}
	if len(p.Hosts) > 0 {
		return p.Hosts[0]
	}
	return ""
}

// QifaCertRootInContainer is where qifa-managed certs live inside the
// kamal-proxy container, under its state-volume mount. lego writes to
// the same volume at <volume>/qifa/certificates/<host>.{crt,key}.
const QifaCertRootInContainer = "/home/kamal-proxy/.config/kamal-proxy/qifa/certificates"

// QifaCertPaths returns the cert and key paths kamal-proxy reads when
// source: qifa is in effect for the given proxy host.
func QifaCertPaths(host string) (cert, key string) {
	return QifaCertRootInContainer + "/" + host + ".crt",
		QifaCertRootInContainer + "/" + host + ".key"
}
