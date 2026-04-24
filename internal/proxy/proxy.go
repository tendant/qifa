package proxy

import (
	"context"
	"fmt"
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
	AppPort int
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
		"([ -S /tmp/kamal-proxy.sock ] || nohup kamal-proxy run >/tmp/kamal-proxy.log 2>&1 </dev/null &)",
		"for i in 1 2 3 4 5; do [ -S /tmp/kamal-proxy.sock ] && exit 0; sleep 1; done; exit 1",
	}, " && ")
	_, err := k.client.Run(ctx, host, command)
	return err
}

func (k *KamalProxy) Deploy(ctx context.Context, host string, target Target) error {
	command := fmt.Sprintf(
		"kamal-proxy deploy %s --host %s --target %s:%d --health-check-path %s",
		shellQuote(target.Service),
		shellQuote(k.cfg.Host),
		shellQuote(target.Host),
		target.AppPort,
		shellQuote(k.cfg.Healthcheck.Path),
	)
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
