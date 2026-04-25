package registry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gokamal/gocart/internal/config"
	"github.com/gokamal/gocart/internal/ssh"
)

type dockerConfig struct {
	Auths map[string]dockerAuth `json:"auths"`
}

type dockerAuth struct {
	Auth string `json:"auth"`
}

func Login(ctx context.Context, client *ssh.Client, cfg config.Registry, host string) (string, error) {
	if cfg.Server == "" {
		return "", nil
	}
	contents, err := configJSON(cfg)
	if err != nil {
		return "", err
	}
	configDir := "/tmp/.godeploy-docker-config"
	configPath := filepath.Join(configDir, "config.json")
	if err := client.Upload(ctx, host, configPath, contents, 0o600); err != nil {
		return "", err
	}
	return configDir, nil
}

func LocalEnv(cfg config.Registry) (map[string]string, func(), error) {
	if cfg.Server == "" {
		return nil, func() {}, nil
	}
	contents, err := configJSON(cfg)
	if err != nil {
		return nil, nil, err
	}
	configDir, err := os.MkdirTemp("", "godeploy-docker-config-")
	if err != nil {
		return nil, nil, fmt.Errorf("create docker config dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), contents, 0o600); err != nil {
		_ = os.RemoveAll(configDir)
		return nil, nil, fmt.Errorf("write docker config: %w", err)
	}
	return map[string]string{"DOCKER_CONFIG": configDir}, func() {
		_ = os.RemoveAll(configDir)
	}, nil
}

func configJSON(cfg config.Registry) ([]byte, error) {
	password, ok := os.LookupEnv(cfg.PasswordEnv)
	if !ok {
		return nil, fmt.Errorf("registry password env %s is not set", cfg.PasswordEnv)
	}
	payload := dockerConfig{
		Auths: map[string]dockerAuth{
			cfg.Server: {
				Auth: base64.StdEncoding.EncodeToString([]byte(cfg.Username + ":" + password)),
			},
		},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal docker config: %w", err)
	}
	return data, nil
}
