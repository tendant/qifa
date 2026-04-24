package hooks

import (
	"context"
	"fmt"
	"os"
	"os/exec"
)

func Run(ctx context.Context, path string, extraEnv map[string]string) error {
	if path == "" {
		return nil
	}
	cmd := exec.CommandContext(ctx, path)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	env := os.Environ()
	for key, value := range extraEnv {
		env = append(env, key+"="+value)
	}
	cmd.Env = env
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("run hook %s: %w", path, err)
	}
	return nil
}
