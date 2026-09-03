package registry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"

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
	configDir := remoteConfigDir()
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
	configDir, err := os.MkdirTemp("", "qifa-docker-config-")
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

// remoteConfigDir names the staging directory for the docker credentials on
// the target host. It carries the deployer's username because /tmp is sticky:
// a single shared path would be created mode 0600 by whoever deployed first,
// and every later deploy by a different SSH account would fail to overwrite
// it. Teams that all SSH as one deploy user land on the same directory, which
// is fine — it is theirs.
func remoteConfigDir() string {
	name := "unknown"
	if u, err := user.Current(); err == nil && u != nil && u.Username != "" {
		name = sanitizeUser(u.Username)
	}
	return "/tmp/.qifa-docker-config-" + name
}

// sanitizeUser keeps the path a single, predictable segment: Windows and
// directory-service usernames can carry backslashes, spaces, or @.
func sanitizeUser(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
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
