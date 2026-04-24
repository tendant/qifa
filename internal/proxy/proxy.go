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

func New(client *ssh.Client, cfg config.Proxy) *KamalProxy {
	return &KamalProxy{client: client, cfg: cfg}
}

func (k *KamalProxy) EnsureInstalled(ctx context.Context, host string) error {
	command := strings.Join([]string{
		"command -v kamal-proxy >/dev/null",
		"([ -S /tmp/kamal-proxy.sock ] || [ -e /tmp/kamal-proxy.sock ] || nohup kamal-proxy run >/tmp/kamal-proxy.log 2>&1 </dev/null &)",
		"for i in 1 2 3 4 5; do ([ -S /tmp/kamal-proxy.sock ] || [ -e /tmp/kamal-proxy.sock ]) && exit 0; sleep 1; done; exit 1",
	}, " && ")
	_, err := k.client.Run(ctx, host, command)
	return err
}

func (k *KamalProxy) Deploy(ctx context.Context, host string, target Target) error {
	command := k.deployCommand(target)
	_, err := k.client.Run(ctx, host, command)
	return err
}

func (k *KamalProxy) Remove(ctx context.Context, host, service string) error {
	_, err := k.client.Run(ctx, host, "kamal-proxy remove "+shellQuote(service))
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
	args = append(args,
		"--target", shellQuote(fmt.Sprintf("%s:%d", target.Host, target.Port)),
		"--health-check-path", shellQuote(k.cfg.Healthcheck.Path),
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
