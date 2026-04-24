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
	_, err := k.client.Run(ctx, host, "kamal-proxy --version >/dev/null")
	return err
}

func (k *KamalProxy) Deploy(ctx context.Context, host string, target Target) error {
	command := fmt.Sprintf(
		"kamal-proxy deploy --service %s --host %s --target %s:%d --health-check-path %s",
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
	_, err := k.client.Run(ctx, host, "kamal-proxy remove --service "+shellQuote(service))
	return err
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
