package deploy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gokamal/gocart/internal/config"
	"github.com/gokamal/gocart/internal/state"
)

func TestDeployerEndToEndWithLocalSSH(t *testing.T) {
	tests := []struct {
		name     string
		mode     string
		source   string
		image    string
		registry config.Registry
		host     string
		expect   []string
		reject   []string
	}{
		{
			name:   "local registry",
			mode:   "local",
			source: "local",
			image:  "registry.example.com/testapp",
			registry: config.Registry{
				Server:      "registry.example.com",
				Username:    "reg",
				PasswordEnv: "REGISTRY_PASSWORD",
			},
			expect: []string{"build", "push push registry.example.com/testapp:", "pull pull registry.example.com/testapp:", "run"},
		},
		{
			name:   "remote registry",
			mode:   "remote",
			source: "local",
			image:  "registry.example.com/testapp",
			registry: config.Registry{
				Server:      "registry.example.com",
				Username:    "reg",
				PasswordEnv: "REGISTRY_PASSWORD",
			},
			host:   "127.0.0.1:%PORT%",
			expect: []string{"build", "push push registry.example.com/testapp:", "pull pull registry.example.com/testapp:", "run"},
		},
		{
			name:   "per target local",
			mode:   "per_target",
			source: "local",
			image:  "testapp",
			expect: []string{"build", "run"},
			reject: []string{"push push testapp:", "pull pull testapp:"},
		},
		{
			name:   "local registry git",
			mode:   "local",
			source: "git",
			image:  "registry.example.com/testapp",
			registry: config.Registry{
				Server:      "registry.example.com",
				Username:    "reg",
				PasswordEnv: "REGISTRY_PASSWORD",
			},
			expect: []string{"clone", "checkout", "build", "push push registry.example.com/testapp:", "pull pull registry.example.com/testapp:", "run"},
		},
		{
			name:   "remote registry git",
			mode:   "remote",
			source: "git",
			image:  "registry.example.com/testapp",
			registry: config.Registry{
				Server:      "registry.example.com",
				Username:    "reg",
				PasswordEnv: "REGISTRY_PASSWORD",
			},
			host:   "127.0.0.1:%PORT%",
			expect: []string{"clone", "checkout", "build", "push push registry.example.com/testapp:", "pull pull registry.example.com/testapp:", "run"},
		},
		{
			name:   "per target git",
			mode:   "per_target",
			source: "git",
			image:  "testapp",
			expect: []string{"clone", "checkout", "build", "run"},
			reject: []string{"push push testapp:", "pull pull testapp:"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			env := newIntegrationEnv(t)
			cfg := env.config(t, tt.mode, tt.source, tt.image, tt.registry, tt.host)
			store, err := state.NewStore(filepath.Join(env.root, ".qifa", "state.jsonl"))
			if err != nil {
				t.Fatal(err)
			}

			var stdout bytes.Buffer
			var stderr bytes.Buffer
			deployer, err := New(cfg, store, &stdout, &stderr)
			if err != nil {
				t.Fatal(err)
			}

			if err := deployer.Deploy(ctx); err != nil {
				t.Fatalf(
					"deploy failed: %v\nstdout:\n%s\nstderr:\n%s\ndocker calls:\n%s\nproxy calls:\n%s\nsshd log:\n%s",
					err,
					stdout.String(),
					stderr.String(),
					readIfExists(filepath.Join(env.stateDir, "docker_calls.log")),
					readIfExists(filepath.Join(env.stateDir, "proxy_calls.log")),
					readIfExists(filepath.Join(env.root, "sshd.log")),
				)
			}

			var execOut bytes.Buffer
			if err := deployer.Exec(ctx, "printf hello", &execOut); err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(execOut.String()) != "hello" {
				t.Fatalf("unexpected exec output: %q", execOut.String())
			}

			var logsOut bytes.Buffer
			if err := deployer.Logs(ctx, &logsOut); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(logsOut.String(), "image="+tt.image+":") {
				t.Fatalf("unexpected logs output: %q", logsOut.String())
			}

			if err := deployer.AccessoryBoot(ctx, "redis"); err != nil {
				t.Fatal(err)
			}
			var accessoryLogs bytes.Buffer
			if err := deployer.AccessoryLogs(ctx, "redis", &accessoryLogs); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(accessoryLogs.String(), "image=redis:7") {
				t.Fatalf("unexpected accessory logs: %q", accessoryLogs.String())
			}

			var statusOut bytes.Buffer
			if err := deployer.Status(ctx, &statusOut); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(statusOut.String(), "Succeeded") {
				t.Fatalf("unexpected status output: %q", statusOut.String())
			}

			if err := deployer.Rollback(ctx); err != nil {
				t.Fatal(err)
			}

			hookCalls, err := os.ReadFile(filepath.Join(env.stateDir, "hook_calls.log"))
			if err != nil {
				t.Fatal(err)
			}
			hooks := string(hookCalls)
			for _, expected := range []string{"pre_build", "post_deploy", "pre_rollback"} {
				if !strings.Contains(hooks, expected) {
					t.Fatalf("missing hook call %q in %q", expected, hooks)
				}
			}

			dockerCalls, err := os.ReadFile(filepath.Join(env.stateDir, "docker_calls.log"))
			if err != nil {
				t.Fatal(err)
			}
			calls := string(dockerCalls) + readIfExists(filepath.Join(env.stateDir, "git_calls.log"))
			for _, expected := range tt.expect {
				if !strings.Contains(calls, expected) {
					t.Fatalf("missing docker call %q in %q", expected, calls)
				}
			}
			for _, rejected := range tt.reject {
				if strings.Contains(calls, rejected) {
					t.Fatalf("unexpected docker call %q in %q", rejected, calls)
				}
			}

			registryConfig, err := os.ReadFile(filepath.Join(env.root, "home", ".docker", "config.json"))
			if err == nil && len(registryConfig) > 0 {
				t.Fatalf("expected auth to avoid mutating home docker config, found %q", string(registryConfig))
			}

			proxyCalls, err := os.ReadFile(filepath.Join(env.stateDir, "proxy_calls.log"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(proxyCalls), "deploy") {
				t.Fatalf("expected proxy deploy call, got %q", string(proxyCalls))
			}

			deployments, _, err := store.Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			if len(deployments) != 2 {
				t.Fatalf("expected deploy and rollback records, got %d", len(deployments))
			}
			var sawSucceeded bool
			var sawRolledBack bool
			for _, deployment := range deployments {
				if deployment.Status == state.StatusSucceeded {
					sawSucceeded = true
				}
				if deployment.Status == state.StatusRolledBack {
					sawRolledBack = true
				}
			}
			if !sawSucceeded || !sawRolledBack {
				t.Fatalf("expected succeeded and rolled back states, got %#v", deployments)
			}
		})
	}
}

func TestDeployerNonProxyHealthCheckUsesPublishedPort(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	env := newIntegrationEnv(t)
	cfg := env.config(t, "per_target", "local", "testapp", config.Registry{}, "")
	server := cfg.Servers["web"]
	proxyEnabled := false
	server.Proxy = &proxyEnabled
	server.Port = 19080
	server.AppPort = 80
	cfg.Servers["web"] = server

	store, err := state.NewStore(filepath.Join(env.root, ".qifa", "state.jsonl"))
	if err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	deployer, err := New(cfg, store, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}

	if err := deployer.Deploy(ctx); err != nil {
		t.Fatalf(
			"deploy failed: %v\nstdout:\n%s\nstderr:\n%s\ndocker calls:\n%s\ncurl calls:\n%s\nproxy calls:\n%s\nsshd log:\n%s",
			err,
			stdout.String(),
			stderr.String(),
			readIfExists(filepath.Join(env.stateDir, "docker_calls.log")),
			readIfExists(filepath.Join(env.stateDir, "curl_calls.log")),
			readIfExists(filepath.Join(env.stateDir, "proxy_calls.log")),
			readIfExists(filepath.Join(env.root, "sshd.log")),
		)
	}

	curlCalls := readIfExists(filepath.Join(env.stateDir, "curl_calls.log"))
	if !strings.Contains(curlCalls, "http://127.0.0.1:19080/up") {
		t.Fatalf("expected health check on published port, got %q", curlCalls)
	}
	if strings.Contains(curlCalls, "http://127.0.0.1:80/up") || strings.Contains(curlCalls, fmt.Sprintf("http://127.0.0.1:%d:19080/up", env.port)) {
		t.Fatalf("unexpected health check on container port, got %q", curlCalls)
	}

	proxyCalls := readIfExists(filepath.Join(env.stateDir, "proxy_calls.log"))
	if strings.Contains(proxyCalls, "deploy") {
		t.Fatalf("expected proxy deploy to be skipped, got %q", proxyCalls)
	}
}

type integrationEnv struct {
	root       string
	home       string
	stateDir   string
	fakeBin    string
	buildDir   string
	repoDir    string
	port       int
	sshdConfig string
}

func newIntegrationEnv(t *testing.T) *integrationEnv {
	t.Helper()

	root := t.TempDir()
	home := filepath.Join(root, "home")
	stateDir := filepath.Join(root, "state")
	fakeBin := filepath.Join(root, "fakebin")
	buildDir := filepath.Join(root, "buildctx")
	repoDir := filepath.Join(root, "repo-src")
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{stateDir, fakeBin, buildDir, repoDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv("HOME", home)
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("REGISTRY_PASSWORD", "secret")
	t.Setenv("DATABASE_URL", "postgres://db/test")

	env := &integrationEnv{
		root:     root,
		home:     home,
		stateDir: stateDir,
		fakeBin:  fakeBin,
		buildDir: buildDir,
		repoDir:  repoDir,
		port:     freePort(t),
	}

	env.writeBuildContext(t)
	env.writeGitRepo(t)
	env.writeFakeExecutables(t)
	env.generateKeys(t)
	env.startSSHD(t)

	return env
}

func (e *integrationEnv) config(t *testing.T, mode, source, image string, registryCfg config.Registry, builderHost string) *config.Config {
	t.Helper()

	if builderHost == "" && mode == "remote" {
		builderHost = fmt.Sprintf("127.0.0.1:%d", e.port)
	}
	builderHost = strings.ReplaceAll(builderHost, "%PORT%", fmt.Sprintf("%d", e.port))

	builder := config.Builder{
		Mode:       mode,
		Host:       builderHost,
		Source:     source,
		Dockerfile: "Dockerfile",
		Platform:   "linux/amd64",
	}
	if source == "git" {
		builder.Repo = e.repoDir
		builder.Ref = "main"
		builder.Subdir = "."
	} else {
		builder.Context = e.buildDir
	}

	return &config.Config{
		Service: "testapp",
		Image:   image,
		Servers: map[string]config.Server{
			"web": {
				Hosts: []string{fmt.Sprintf("127.0.0.1:%d", e.port)},
				Port:  3000,
			},
			"worker": {
				Hosts: []string{fmt.Sprintf("127.0.0.1:%d", e.port)},
				Cmd:   "./worker",
			},
		},
		Proxy: config.Proxy{
			Host:    "app.example.test",
			AppPort: 3000,
			Healthcheck: config.Healthcheck{
				Path:     "/up",
				Interval: 10 * time.Millisecond,
				Timeout:  time.Second,
			},
		},
		Registry: registryCfg,
		Env: config.Env{
			Clear: map[string]string{
				"APP_ENV": "production",
			},
			Secret: []string{"DATABASE_URL"},
		},
		Builder: builder,
		SSH: config.SSH{
			User: currentUsername(t),
			Key:  "~/id_ed25519",
		},
		Hooks: config.Hooks{
			PreBuild:    filepath.Join(e.root, "pre_build.sh"),
			PostDeploy:  filepath.Join(e.root, "post_deploy.sh"),
			PreRollback: filepath.Join(e.root, "pre_rollback.sh"),
		},
		Accessories: map[string]config.Accessory{
			"redis": {
				Image: "redis:7",
				Host:  fmt.Sprintf("127.0.0.1:%d", e.port),
			},
		},
	}
}

func (e *integrationEnv) writeBuildContext(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(e.buildDir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.buildDir, "app.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (e *integrationEnv) writeGitRepo(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(e.repoDir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.repoDir, "app.txt"), []byte("hello from git\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (e *integrationEnv) writeFakeExecutables(t *testing.T) {
	t.Helper()

	writeExecutable(t, filepath.Join(e.fakeBin, "docker"), fmt.Sprintf(`#!/bin/sh
set -eu
state=%q
mkdir -p "$state/containers"
echo "$1 $*" >> "$state/docker_calls.log"
cmd="$1"
shift
case "$cmd" in
  build|push|info|pull|login)
    exit 0
    ;;
  ps)
    while [ "$#" -gt 0 ]; do
      case "$1" in
        -a)
          shift
          ;;
        --filter)
          filter="$2"
          shift 2
          ;;
        --format)
          format="$2"
          shift 2
          ;;
        *)
          shift
          ;;
      esac
    done
    prefix=""
    case "${filter:-}" in
      name=^*)
        prefix="${filter#name=^}"
        ;;
    esac
    for path in "$state"/containers/*; do
      [ -e "$path" ] || continue
      name="$(basename "$path")"
      case "$name" in
        "$prefix"*)
          printf '%%s\n' "$name"
          ;;
      esac
    done
    ;;
  run)
    name=""
    envfile=""
    image=""
    usercmd=""
    ipfile=""
    ipoctet=$(($(find "$state/containers" -maxdepth 1 -type f | wc -l) + 10))
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --restart|--name|--env-file|-p)
          key="$1"
          val="$2"
          if [ "$key" = "--name" ]; then name="$val"; fi
          if [ "$key" = "--env-file" ]; then envfile="$val"; fi
          shift 2
          ;;
        -d)
          shift
          ;;
        *)
          image="$1"
          shift
          usercmd="$*"
          break
          ;;
      esac
    done
    ipfile="$state/containers/$name"
    printf 'image=%%s\nenvfile=%%s\ncmd=%%s\nip=172.18.0.%%s\n' "$image" "$envfile" "$usercmd" "$ipoctet" > "$ipfile"
    ;;
  inspect)
    if [ "${1:-}" = "-f" ]; then shift 2; fi
    name="$1"
    awk -F= '/^ip=/{print $2}' "$state/containers/$name"
    ;;
  logs)
    if [ "${1:-}" = "--tail" ]; then shift 2; fi
    cat "$state/containers/$1"
    ;;
  exec)
    name="$1"
    shift
    if [ "$1" = "sh" ] && [ "$2" = "-lc" ]; then
      shift 2
      /bin/sh -c "$1"
      exit 0
    fi
    echo "unsupported exec invocation for $name" >&2
    exit 1
    ;;
  rm)
    if [ "${1:-}" = "-f" ]; then shift; fi
    rm -f "$state/containers/$1"
    ;;
  *)
    echo "unsupported docker command: $cmd" >&2
    exit 1
    ;;
esac
`, e.stateDir))

	writeExecutable(t, filepath.Join(e.fakeBin, "kamal-proxy"), fmt.Sprintf(`#!/bin/sh
set -eu
echo "$*" >> %q/proxy_calls.log
if [ "${1:-}" = "--version" ]; then
  echo "kamal-proxy 0.0.0"
  exit 0
fi
if [ "${1:-}" = "run" ]; then
  : > /tmp/kamal-proxy.sock
fi
`, e.stateDir))

	writeExecutable(t, filepath.Join(e.fakeBin, "curl"), fmt.Sprintf(`#!/bin/sh
set -eu
echo "$*" >> %q/curl_calls.log
echo ok
`, e.stateDir))

	writeExecutable(t, filepath.Join(e.fakeBin, "git"), fmt.Sprintf(`#!/bin/sh
set -eu
state=%q
echo "$*" >> "$state/git_calls.log"
cmd="$1"
shift
case "$cmd" in
  clone)
    src="$1"
    dst="$2"
    mkdir -p "$dst"
    cp -R "$src"/. "$dst"/
    ;;
  -C)
    repo="$1"
    shift
    subcmd="$1"
    shift
    case "$subcmd" in
      checkout)
        printf 'ref=%%s\n' "$1" > "$repo/.git-checkout"
        ;;
      *)
        echo "unsupported git subcommand: $subcmd" >&2
        exit 1
        ;;
    esac
    ;;
  *)
    echo "unsupported git command: $cmd" >&2
    exit 1
    ;;
esac
`, e.stateDir))

	for _, hook := range []string{"pre_build", "post_deploy", "pre_rollback"} {
		writeExecutable(t, filepath.Join(e.root, hook+".sh"), fmt.Sprintf(`#!/bin/sh
set -eu
echo %s >> %q/hook_calls.log
`, hook, e.stateDir))
	}

	writeExecutable(t, filepath.Join(e.root, "remote-shell"), fmt.Sprintf(`#!/bin/sh
set -eu
export PATH=%q:"$PATH"
export FAKE_STATE_DIR=%q
if [ -n "${SSH_ORIGINAL_COMMAND:-}" ]; then
  exec /bin/sh -c "$SSH_ORIGINAL_COMMAND"
fi
exec /bin/sh
`, e.fakeBin, e.stateDir))
}

func (e *integrationEnv) generateKeys(t *testing.T) {
	t.Helper()

	clientKey := filepath.Join(e.home, "id_ed25519")
	hostKey := filepath.Join(e.root, "ssh_host_ed25519_key")

	runCmd(t, exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", clientKey))
	runCmd(t, exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", hostKey))

	pubKey, err := os.ReadFile(clientKey + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.root, "authorized_keys"), pubKey, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (e *integrationEnv) startSSHD(t *testing.T) {
	t.Helper()

	configPath := filepath.Join(e.root, "sshd_config")
	e.sshdConfig = configPath
	sshdLog := filepath.Join(e.root, "sshd.log")
	configText := fmt.Sprintf(`Port %d
ListenAddress 127.0.0.1
HostKey %s
AuthorizedKeysFile %s
PidFile %s
PasswordAuthentication no
KbdInteractiveAuthentication no
ChallengeResponseAuthentication no
PubkeyAuthentication yes
PermitRootLogin no
AllowUsers %s
UsePAM no
StrictModes no
LogLevel VERBOSE
PrintMotd no
ForceCommand %s
`, e.port, filepath.Join(e.root, "ssh_host_ed25519_key"), filepath.Join(e.root, "authorized_keys"), filepath.Join(e.root, "sshd.pid"), currentUsername(t), filepath.Join(e.root, "remote-shell"))
	if err := os.WriteFile(configPath, []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}

	sshdPath, err := exec.LookPath("sshd")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(sshdPath, "-D", "-f", configPath, "-E", sshdLog)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	waitForTCP(t, e.port, sshdLog)

	knownHosts := filepath.Join(e.home, ".ssh", "known_hosts")
	keyscan := exec.Command("ssh-keyscan", "-p", fmt.Sprintf("%d", e.port), "127.0.0.1")
	output, err := keyscan.Output()
	if err != nil {
		logData, _ := os.ReadFile(sshdLog)
		t.Fatalf("ssh-keyscan failed: %v\nsshd log:\n%s", err, string(logData))
	}
	if err := os.WriteFile(knownHosts, output, 0o600); err != nil {
		t.Fatal(err)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func waitForTCP(t *testing.T, port int, logPath string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	logData, _ := os.ReadFile(logPath)
	t.Fatalf("sshd did not start on port %d\nsshd log:\n%s", port, string(logData))
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func runCmd(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%q failed: %v\n%s", strings.Join(cmd.Args, " "), err, string(output))
	}
}

func currentUsername(t *testing.T) string {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	return u.Username
}

func readIfExists(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}
