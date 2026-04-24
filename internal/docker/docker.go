package docker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/gokamal/gocart/internal/config"
	"github.com/gokamal/gocart/internal/ssh"
)

type Local struct{}

func NewLocal() *Local {
	return &Local{}
}

func (l *Local) BuildAndPush(ctx context.Context, cfg *config.Config, imageRef string) error {
	args := []string{"build", "-f", cfg.Builder.Dockerfile, "-t", imageRef}
	if cfg.Builder.Platform != "" {
		args = append(args, "--platform", cfg.Builder.Platform)
	}
	args = append(args, cfg.Builder.Context)
	if err := runLocal(ctx, "docker", args...); err != nil {
		return err
	}
	return runLocal(ctx, "docker", "push", imageRef)
}

type Remote struct {
	client *ssh.Client
}

func NewRemote(client *ssh.Client) *Remote {
	return &Remote{client: client}
}

func (r *Remote) EnsureDocker(ctx context.Context, host string) error {
	_, err := r.client.Run(ctx, host, "docker info >/dev/null")
	return err
}

func (r *Remote) Pull(ctx context.Context, host, imageRef string) error {
	_, err := r.client.Run(ctx, host, "docker pull "+shellQuote(imageRef))
	return err
}

func (r *Remote) RunContainer(ctx context.Context, host, name, imageRef, envFile, command string, port int) error {
	var args []string
	args = append(args, "docker run -d --restart unless-stopped")
	args = append(args, "--name "+shellQuote(name))
	if envFile != "" {
		args = append(args, "--env-file "+shellQuote(envFile))
	}
	if port > 0 {
		args = append(args, fmt.Sprintf("-p %d:%d", port, port))
	}
	if command != "" {
		args = append(args, shellQuote(imageRef)+" "+command)
	} else {
		args = append(args, shellQuote(imageRef))
	}
	_, err := r.client.Run(ctx, host, strings.Join(args, " "))
	return err
}

func (r *Remote) StopAndRemove(ctx context.Context, host, name string) error {
	_, err := r.client.Run(ctx, host, "docker rm -f "+shellQuote(name)+" >/dev/null 2>&1 || true")
	return err
}

func (r *Remote) Logs(ctx context.Context, host, name string) (string, error) {
	return r.client.Run(ctx, host, "docker logs --tail 200 "+shellQuote(name))
}

func (r *Remote) Exec(ctx context.Context, host, name, command string) (string, error) {
	return r.client.Run(ctx, host, "docker exec "+shellQuote(name)+" sh -lc "+shellQuote(command))
}

func runLocal(ctx context.Context, binary string, args ...string) error {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
