// Package lock provides a per-service mutual-exclusion lock held across the
// configured target hosts. The lock is a directory under /tmp on each host,
// created via mkdir (atomic) so two concurrent acquirers can't both succeed.
package lock

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"strings"
	"time"

	"github.com/gokamal/gocart/internal/ssh"
)

type Holder struct {
	User       string    `json:"user"`
	Host       string    `json:"host"`
	Service    string    `json:"service"`
	Version    string    `json:"version,omitempty"`
	AcquiredAt time.Time `json:"acquired_at"`
}

type Lock struct {
	client  *ssh.Client
	service string
	hosts   []string
	held    []string
}

func New(client *ssh.Client, service string, hosts []string) *Lock {
	return &Lock{client: client, service: service, hosts: hosts}
}

// Acquire takes the lock on every configured host. If any host's lock can't
// be taken, hosts already locked are released and an error describing the
// holder is returned.
func (l *Lock) Acquire(ctx context.Context, version string) error {
	holderJSON, err := json.Marshal(currentHolder(l.service, version))
	if err != nil {
		return err
	}
	dir := lockPath(l.service)
	for _, host := range l.hosts {
		if _, err := l.client.Run(ctx, host, "mkdir "+shellQuote(dir)); err != nil {
			existing, _ := l.client.Run(ctx, host, "cat "+shellQuote(dir)+"/holder.json 2>/dev/null")
			l.Release(context.Background())
			if h := strings.TrimSpace(existing); h != "" {
				return fmt.Errorf("lock for %s held on %s: %s", l.service, host, h)
			}
			return fmt.Errorf("could not acquire lock for %s on %s: %w", l.service, host, err)
		}
		if err := l.client.Upload(ctx, host, dir+"/holder.json", holderJSON, 0o644); err != nil {
			l.held = append(l.held, host) // so Release cleans this one too
			l.Release(context.Background())
			return fmt.Errorf("write lock metadata on %s: %w", host, err)
		}
		l.held = append(l.held, host)
	}
	return nil
}

// Release removes the lock directory on every host where Acquire succeeded.
// Best-effort: errors per host are ignored (lock files are advisory).
func (l *Lock) Release(ctx context.Context) {
	dir := lockPath(l.service)
	for _, host := range l.held {
		_, _ = l.client.Run(ctx, host, "rm -rf "+shellQuote(dir))
	}
	l.held = nil
}

// ForceRelease removes the lock directory on every configured host, regardless
// of whether this Lock instance acquired it. For manual recovery from stale
// locks left by crashed deploys.
func (l *Lock) ForceRelease(ctx context.Context) error {
	dir := lockPath(l.service)
	var errs []string
	for _, host := range l.hosts {
		if _, err := l.client.Run(ctx, host, "rm -rf "+shellQuote(dir)); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", host, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("force release: %s", strings.Join(errs, "; "))
	}
	return nil
}

// Status returns the holder JSON per host, or empty string if no lock is held
// on that host.
func (l *Lock) Status(ctx context.Context) (map[string]string, error) {
	dir := lockPath(l.service)
	out := map[string]string{}
	for _, host := range l.hosts {
		holder, _ := l.client.Run(ctx, host, "cat "+shellQuote(dir)+"/holder.json 2>/dev/null")
		out[host] = strings.TrimSpace(holder)
	}
	return out, nil
}

func currentHolder(service, version string) Holder {
	name := "unknown"
	if u, err := user.Current(); err == nil && u != nil {
		name = u.Username
	}
	host, _ := os.Hostname()
	return Holder{
		User:       name,
		Host:       host,
		Service:    service,
		Version:    version,
		AcquiredAt: time.Now().UTC(),
	}
}

func lockPath(service string) string {
	return "/tmp/qifa-lock-" + service
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
