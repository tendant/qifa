package registry

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/gokamal/gocart/internal/config"
	"github.com/gokamal/gocart/internal/ssh"
)

func Login(ctx context.Context, client *ssh.Client, cfg config.Registry, host string) error {
	if cfg.Server == "" {
		return nil
	}
	password, ok := os.LookupEnv(cfg.PasswordEnv)
	if !ok {
		return fmt.Errorf("registry password env %s is not set", cfg.PasswordEnv)
	}
	command := fmt.Sprintf(
		"docker login %s -u %s --password-stdin",
		shellQuote(cfg.Server),
		shellQuote(cfg.Username),
	)
	if err := client.Upload(ctx, host, "/tmp/.godeploy-registry-password", []byte(password), 0o600); err != nil {
		return err
	}
	command = "cat /tmp/.godeploy-registry-password | " + command + " && rm -f /tmp/.godeploy-registry-password"
	_, err := client.Run(ctx, host, command)
	return err
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
