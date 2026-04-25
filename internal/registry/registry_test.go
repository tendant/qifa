package registry

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gokamal/gocart/internal/config"
)

func TestLocalEnvWritesDockerConfig(t *testing.T) {
	t.Setenv("REGISTRY_PASSWORD", "secret")

	env, cleanup, err := LocalEnv(config.Registry{
		Server:      "registry.example.com",
		Username:    "reg",
		PasswordEnv: "REGISTRY_PASSWORD",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	configDir := env["DOCKER_CONFIG"]
	data, err := os.ReadFile(filepath.Join(configDir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}

	var payload struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}

	got := payload.Auths["registry.example.com"].Auth
	want := base64.StdEncoding.EncodeToString([]byte("reg:secret"))
	if got != want {
		t.Fatalf("unexpected auth: %s", got)
	}
}
