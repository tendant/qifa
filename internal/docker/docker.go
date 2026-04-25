package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gokamal/gocart/internal/config"
	"github.com/gokamal/gocart/internal/registry"
	"github.com/gokamal/gocart/internal/ssh"
)

type Local struct{}

type BuildSpec struct {
	ContextDir string
	Dockerfile string
	Platform   string
}

func NewLocal() *Local {
	return &Local{}
}

func (l *Local) BuildAndPush(ctx context.Context, cfg *config.Config, imageRef string) error {
	if err := l.Build(ctx, cfg, imageRef); err != nil {
		return err
	}
	return l.Push(ctx, cfg.Registry, imageRef)
}

func (l *Local) Build(ctx context.Context, cfg *config.Config, imageRef string) error {
	spec, cleanup, err := localBuildSpec(ctx, cfg)
	if err != nil {
		return err
	}
	defer cleanup()
	return l.BuildSpec(ctx, spec, imageRef)
}

func (l *Local) BuildSpec(ctx context.Context, spec BuildSpec, imageRef string) error {
	args := []string{"build", "-f", spec.Dockerfile, "-t", imageRef}
	if spec.Platform != "" {
		args = append(args, "--platform", spec.Platform)
	}
	args = append(args, spec.ContextDir)
	extraEnv := map[string]string{}
	if spec.Platform == "" {
		extraEnv["DOCKER_BUILDKIT"] = "0"
	}
	return runLocalEnv(ctx, extraEnv, "docker", args...)
}

func (l *Local) Push(ctx context.Context, registryCfg config.Registry, imageRef string) error {
	registryEnv, cleanup, err := registry.LocalEnv(registryCfg)
	if err != nil {
		return err
	}
	defer cleanup()
	return runLocalEnv(ctx, registryEnv, "docker", "push", imageRef)
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

func (r *Remote) Pull(ctx context.Context, host, dockerConfigDir, imageRef string) error {
	_, err := r.client.Run(ctx, host, withDockerConfig(dockerConfigDir, "docker pull "+shellQuote(imageRef)))
	return err
}

func (r *Remote) Push(ctx context.Context, host, dockerConfigDir, imageRef string) error {
	_, err := r.client.Run(ctx, host, withDockerConfig(dockerConfigDir, "docker push "+shellQuote(imageRef)))
	return err
}

func (r *Remote) Build(ctx context.Context, host string, cfg *config.Config, imageRef string) error {
	switch cfg.Builder.Source {
	case "local":
		archive, err := buildContextArchive(cfg.Builder.Context)
		if err != nil {
			return err
		}
		remoteRoot := fmt.Sprintf("/tmp/qifa-build-%d", time.Now().UTC().UnixNano())
		remoteArchive := filepath.Join(remoteRoot, "context.tar")
		remoteContext := filepath.Join(remoteRoot, "context")
		if err := r.client.Upload(ctx, host, remoteArchive, archive, 0o600); err != nil {
			return err
		}
		command := strings.Join([]string{
			"rm -rf " + shellQuote(remoteContext),
			"mkdir -p " + shellQuote(remoteContext),
			"tar -xf " + shellQuote(remoteArchive) + " -C " + shellQuote(remoteContext),
			buildCommand(BuildSpec{
				ContextDir: remoteContext,
				Dockerfile: filepath.Join(remoteContext, cfg.Builder.Dockerfile),
				Platform:   cfg.Builder.Platform,
			}, imageRef),
			"rm -f " + shellQuote(remoteArchive),
		}, " && ")
		_, err = r.client.Run(ctx, host, command)
		return err
	case "git":
		remoteRoot := fmt.Sprintf("/tmp/qifa-build-%d", time.Now().UTC().UnixNano())
		repoDir := filepath.Join(remoteRoot, "repo")
		contextDir := filepath.Join(repoDir, cfg.Builder.Subdir)
		command := strings.Join([]string{
			"rm -rf " + shellQuote(remoteRoot),
			"mkdir -p " + shellQuote(remoteRoot),
			"git clone " + shellQuote(cfg.Builder.Repo) + " " + shellQuote(repoDir),
			"git -C " + shellQuote(repoDir) + " checkout " + shellQuote(cfg.Builder.Ref),
			buildCommand(BuildSpec{
				ContextDir: contextDir,
				Dockerfile: filepath.Join(contextDir, cfg.Builder.Dockerfile),
				Platform:   cfg.Builder.Platform,
			}, imageRef),
		}, " && ")
		_, err := r.client.Run(ctx, host, command)
		return err
	default:
		return fmt.Errorf("unsupported builder source %q", cfg.Builder.Source)
	}
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

func (r *Remote) ContainerIP(ctx context.Context, host, name string) (string, error) {
	return r.client.Run(ctx, host, "docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "+shellQuote(name))
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
	return runLocalEnv(ctx, nil, binary, args...)
}

func runLocalEnv(ctx context.Context, extraEnv map[string]string, binary string, args ...string) error {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	for key, value := range extraEnv {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	return cmd.Run()
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func withDockerConfig(dir, command string) string {
	if dir == "" {
		return command
	}
	return "DOCKER_CONFIG=" + shellQuote(dir) + " " + command
}

func buildCommand(spec BuildSpec, imageRef string) string {
	args := []string{"docker build", "-f", shellQuote(spec.Dockerfile), "-t", shellQuote(imageRef)}
	if spec.Platform != "" {
		args = append(args, "--platform", shellQuote(spec.Platform))
	}
	args = append(args, shellQuote(spec.ContextDir))
	return strings.Join(args, " ")
}

func buildContextArchive(root string) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	root = filepath.Clean(root)
	if err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			header.Name += "/"
		}
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tw, file)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func localBuildSpec(ctx context.Context, cfg *config.Config) (BuildSpec, func(), error) {
	switch cfg.Builder.Source {
	case "local":
		contextDir := filepath.Clean(cfg.Builder.Context)
		return BuildSpec{
			ContextDir: contextDir,
			Dockerfile: filepath.Join(contextDir, cfg.Builder.Dockerfile),
			Platform:   cfg.Builder.Platform,
		}, func() {}, nil
	case "git":
		root, err := os.MkdirTemp("", "qifa-git-build-")
		if err != nil {
			return BuildSpec{}, nil, err
		}
		cleanup := func() { _ = os.RemoveAll(root) }
		repoDir := filepath.Join(root, "repo")
		if err := runLocal(ctx, "git", "clone", cfg.Builder.Repo, repoDir); err != nil {
			cleanup()
			return BuildSpec{}, nil, err
		}
		if err := runLocal(ctx, "git", "-C", repoDir, "checkout", cfg.Builder.Ref); err != nil {
			cleanup()
			return BuildSpec{}, nil, err
		}
		contextDir := filepath.Join(repoDir, cfg.Builder.Subdir)
		return BuildSpec{
			ContextDir: contextDir,
			Dockerfile: filepath.Join(contextDir, cfg.Builder.Dockerfile),
			Platform:   cfg.Builder.Platform,
		}, cleanup, nil
	default:
		return BuildSpec{}, nil, fmt.Errorf("unsupported builder source %q", cfg.Builder.Source)
	}
}
