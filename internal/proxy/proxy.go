package proxy

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/gokamal/gocart/internal/config"
	"github.com/gokamal/gocart/internal/ssh"
)

type Proxy interface {
	EnsureInstalled(context.Context, string) error
	Deploy(context.Context, string, Target) error
	Remove(context.Context, string, string) error
}

type Target struct {
	Service string
	Host    string
	Port    int
}

type KamalProxy struct {
	client *ssh.Client
	cfg    config.Proxy
}

const (
	proxyContainerName = "kamal-proxy"
)

func New(client *ssh.Client, cfg config.Proxy) *KamalProxy {
	return &KamalProxy{client: client, cfg: cfg}
}

func (k *KamalProxy) EnsureInstalled(ctx context.Context, host string) error {
	command := k.bootCommand()
	_, err := k.client.Run(ctx, host, command)
	return err
}

func (k *KamalProxy) bootCommand() string {
	httpPort := k.cfg.HTTPPort
	if httpPort == 0 {
		httpPort = 80
	}
	httpsPort := k.cfg.HTTPSPort
	if httpsPort == 0 {
		httpsPort = 443
	}
	imageRef := k.cfg.Image
	if imageRef == "" {
		imageRef = "basecamp/kamal-proxy"
	}
	if k.cfg.Version != "" {
		imageRef = imageRef + ":" + k.cfg.Version
	}
	network := k.cfg.Network
	if network == "" {
		network = "kamal"
	}
	stateVolume := k.cfg.StateVolume
	if stateVolume == "" {
		stateVolume = "kamal-proxy-config"
	}
	appsConfigDir := k.cfg.AppsConfigDir
	if appsConfigDir == "" {
		appsConfigDir = ".kamal/proxy/apps-config"
	}
	command := strings.Join([]string{
		"docker network create " + shellQuote(network) + " >/dev/null 2>&1 || true",
		"mkdir -p " + shellQuote(appsConfigDir),
		"docker ps --filter " + shellQuote("name=^"+proxyContainerName) + " --format '{{.Names}}' | grep -qx " + shellQuote(proxyContainerName) + " || (docker rm -f " + shellQuote(proxyContainerName) + " >/dev/null 2>&1 || true; docker run -d --restart unless-stopped --name " + shellQuote(proxyContainerName) + " --network " + shellQuote(network) + " --volume " + shellQuote(stateVolume+":/home/kamal-proxy/.config/kamal-proxy") + " --volume " + shellQuote(appsConfigDir+":/home/kamal-proxy/.apps-config") + " -p " + fmt.Sprintf("%d:80", httpPort) + " -p " + fmt.Sprintf("%d:443", httpsPort) + " --log-opt max-size=10m " + shellQuote(imageRef) + " kamal-proxy run)",
		"for i in 1 2 3 4 5; do docker ps --filter " + shellQuote("name=^"+proxyContainerName) + " --format '{{.Names}}' | grep -qx " + shellQuote(proxyContainerName) + " && exit 0; sleep 1; done; exit 1",
	}, " && ")
	return command
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

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func (k *KamalProxy) deployCommand(target Target) string {
	args := []string{"kamal-proxy", "deploy", shellQuote(target.Service)}
	for _, host := range k.hosts() {
		args = append(args, "--host", shellQuote(host))
	}
	healthPath := k.cfg.Healthcheck.Path
	if healthPath == "" {
		healthPath = "/up"
	}
	args = append(args,
		"--target", shellQuote(fmt.Sprintf("%s:%d", target.Host, target.Port)),
		"--health-check-path", shellQuote(healthPath),
		"--health-check-interval", shellQuote(k.cfg.Healthcheck.Interval.String()),
		"--health-check-timeout", shellQuote(k.cfg.Healthcheck.Timeout.String()),
		"--deploy-timeout", shellQuote(k.cfg.DeployTimeout.String()),
		"--drain-timeout", shellQuote(k.cfg.DrainTimeout.String()),
		"--target-timeout", shellQuote(k.cfg.TargetTimeout.String()),
	)
	if k.cfg.TLS {
		args = append(args, "--tls")
	}
	if k.cfg.TLSRedirect != nil {
		args = append(args, "--tls-redirect="+strconv.FormatBool(*k.cfg.TLSRedirect))
	}
	if k.cfg.TLSStaging {
		args = append(args, "--tls-staging")
	}
	if k.cfg.ForwardHeaders != nil {
		args = append(args, "--forward-headers="+strconv.FormatBool(*k.cfg.ForwardHeaders))
	}
	for _, prefix := range k.cfg.PathPrefixes {
		args = append(args, "--path-prefix", shellQuote(prefix))
	}
	if k.cfg.StripPathPrefix != nil {
		args = append(args, "--strip-path-prefix="+strconv.FormatBool(*k.cfg.StripPathPrefix))
	}
	return strings.Join(args, " ")
}

func (k *KamalProxy) hosts() []string {
	hosts := make([]string, 0, len(k.cfg.Hosts)+1)
	if k.cfg.Host != "" {
		hosts = append(hosts, k.cfg.Host)
	}
	hosts = append(hosts, k.cfg.Hosts...)
	return hosts
}
