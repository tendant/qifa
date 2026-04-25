package secrets

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// Render builds the env file passed to containers. Sources, in priority order
// (later wins on collision):
//  1. clear:        cleartext key/value map from the config
//  2. secret:       names of env vars to pull from the deployer's local env
//  3. secret_command: stdout of a user-provided shell command, parsed as
//     KEY=VALUE lines (dotenv format). Use to integrate SOPS, Vault, etc.
func Render(clear map[string]string, secretKeys []string, secretCommand string) ([]byte, error) {
	values := map[string]string{}
	for key, value := range clear {
		values[key] = value
	}
	for _, key := range secretKeys {
		value, ok := os.LookupEnv(key)
		if !ok {
			return nil, fmt.Errorf("required secret env %s is not set", key)
		}
		values[key] = value
	}
	if strings.TrimSpace(secretCommand) != "" {
		extra, err := runSecretCommand(secretCommand)
		if err != nil {
			return nil, err
		}
		for k, v := range extra {
			values[k] = v
		}
	}

	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var buf bytes.Buffer
	for _, key := range keys {
		fmt.Fprintf(&buf, "%s=%s\n", key, envFileEscape(values[key]))
	}
	return buf.Bytes(), nil
}

func runSecretCommand(command string) (map[string]string, error) {
	cmd := exec.CommandContext(context.Background(), "sh", "-c", command)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("env.secret_command failed: %w", err)
	}
	values := map[string]string{}
	scanner := bufio.NewScanner(bytes.NewReader(out))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.Index(line, "=")
		if eq <= 0 {
			return nil, fmt.Errorf("env.secret_command output line %d: expected KEY=VALUE, got %q", lineNum, line)
		}
		key := strings.TrimSpace(line[:eq])
		value := line[eq+1:]
		// Strip optional surrounding quotes.
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') ||
				(value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("env.secret_command output: %w", err)
	}
	return values, nil
}

func envFileEscape(value string) string {
	return strings.ReplaceAll(value, "\n", "\\n")
}
