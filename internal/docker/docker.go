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
	"sort"
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

// BuildxPush builds a multi-platform image with `docker buildx build --push`,
// which compiles for every platform and pushes the resulting manifest list to
// the registry in one shot. Multi-arch images can't be loaded into a single-arch
// local daemon, so build and push are inseparable here.
func (l *Local) BuildxPush(ctx context.Context, cfg *config.Config, imageRef string) error {
	spec, cleanup, err := localBuildSpec(ctx, cfg)
	if err != nil {
		return err
	}
	defer cleanup()
	registryEnv, cleanupReg, err := registry.LocalEnv(cfg.Registry)
	if err != nil {
		return err
	}
	defer cleanupReg()
	args := []string{"buildx", "build",
		"--platform", spec.Platform,
		"--push",
		"-f", spec.Dockerfile,
		"-t", imageRef,
		spec.ContextDir,
	}
	return runLocalEnv(ctx, registryEnv, "docker", args...)
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

// BuildxPush runs `docker buildx build --push` on the remote host: a single
// invocation that builds for every platform listed in cfg.Builder.Platform and
// pushes the resulting manifest list to the registry.
func (r *Remote) BuildxPush(ctx context.Context, host string, cfg *config.Config, dockerConfigDir, imageRef string) error {
	if cfg.Builder.IsGit() {
		remoteRoot := fmt.Sprintf("/tmp/qifa-build-%d", time.Now().UTC().UnixNano())
		repoDir := filepath.Join(remoteRoot, "repo")
		contextDir := filepath.Join(repoDir, cfg.Builder.Subdir)
		command := strings.Join([]string{
			"rm -rf " + shellQuote(remoteRoot),
			"mkdir -p " + shellQuote(remoteRoot),
			"git clone " + shellQuote(cfg.Builder.Repo) + " " + shellQuote(repoDir),
			"git -C " + shellQuote(repoDir) + " checkout " + shellQuote(cfg.Builder.Ref),
			withDockerConfig(dockerConfigDir, buildxCommand(BuildSpec{
				ContextDir: contextDir,
				Dockerfile: filepath.Join(contextDir, cfg.Builder.Dockerfile),
				Platform:   cfg.Builder.Platform,
			}, imageRef)),
		}, " && ")
		_, err := r.client.Run(ctx, host, command)
		return err
	}
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
		withDockerConfig(dockerConfigDir, buildxCommand(BuildSpec{
			ContextDir: remoteContext,
			Dockerfile: filepath.Join(remoteContext, cfg.Builder.Dockerfile),
			Platform:   cfg.Builder.Platform,
		}, imageRef)),
		"rm -f " + shellQuote(remoteArchive),
	}, " && ")
	_, err = r.client.Run(ctx, host, command)
	return err
}

func (r *Remote) Build(ctx context.Context, host string, cfg *config.Config, imageRef string) error {
	if cfg.Builder.IsGit() {
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
	}
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
}

func (r *Remote) RunContainer(ctx context.Context, host, name, imageRef, envFile, command, network string, labels map[string]string, hostPort, containerPort int) error {
	var args []string
	args = append(args, "docker run -d --restart unless-stopped")
	args = append(args, "--name "+shellQuote(name))
	if network != "" {
		args = append(args, "--network "+shellQuote(network))
	}
	for _, key := range sortedKeys(labels) {
		args = append(args, "--label "+shellQuote(key+"="+labels[key]))
	}
	if envFile != "" {
		args = append(args, "--env-file "+shellQuote(envFile))
	}
	if hostPort > 0 && containerPort > 0 {
		args = append(args, fmt.Sprintf("-p %d:%d", hostPort, containerPort))
	}
	if command != "" {
		args = append(args, shellQuote(imageRef)+" "+command)
	} else {
		args = append(args, shellQuote(imageRef))
	}
	_, err := r.client.Run(ctx, host, strings.Join(args, " "))
	return err
}

type ContainerInfo struct {
	Name      string
	Version   string
	State     string
	CreatedAt time.Time
	Image     string
}

const (
	LabelService = "qifa.service"
	LabelRole    = "qifa.role"
	LabelVersion = "qifa.version"
)

func (r *Remote) ListContainersByService(ctx context.Context, host, service, role string) ([]ContainerInfo, error) {
	const sep = "\x1f"
	format := strings.Join([]string{
		"{{.Names}}",
		"{{.Label \"" + LabelVersion + "\"}}",
		"{{.State}}",
		"{{.CreatedAt}}",
		"{{.Image}}",
	}, sep)
	cmd := "docker ps -a --filter " + shellQuote("label="+LabelService+"="+service)
	if role != "" {
		cmd += " --filter " + shellQuote("label="+LabelRole+"="+role)
	}
	cmd += " --format " + shellQuote(format)
	out, err := r.client.Run(ctx, host, cmd)
	if err != nil {
		return nil, err
	}
	var infos []ContainerInfo
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, sep)
		if len(parts) < 5 {
			continue
		}
		created, _ := parseDockerTime(parts[3])
		infos = append(infos, ContainerInfo{
			Name:      parts[0],
			Version:   parts[1],
			State:     parts[2],
			CreatedAt: created,
			Image:     parts[4],
		})
	}
	sort.Slice(infos, func(i, j int) bool {
		return infos[i].CreatedAt.After(infos[j].CreatedAt)
	})
	return infos, nil
}

func parseDockerTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	layouts := []string{
		"2006-01-02 15:04:05 -0700 MST",
		"2006-01-02 15:04:05 -0700",
		time.RFC3339Nano,
		time.RFC3339,
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized docker time %q", s)
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (r *Remote) ContainerIP(ctx context.Context, host, name string) (string, error) {
	return r.client.Run(ctx, host, "docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "+shellQuote(name))
}

// ImageDigest returns the registry digest (sha256:...) of an image as recorded
// by the local Docker daemon on host. The image must already be pulled.
func (r *Remote) ImageDigest(ctx context.Context, host, image string) (string, error) {
	out, err := r.client.Run(ctx, host, "docker inspect --format '{{index .RepoDigests 0}}' "+shellQuote(image))
	if err != nil {
		return "", err
	}
	out = strings.TrimSpace(out)
	at := strings.LastIndex(out, "@")
	if at < 0 {
		return "", fmt.Errorf("no digest in inspect output: %q", out)
	}
	return out[at+1:], nil
}

func (r *Remote) StopAndRemove(ctx context.Context, host, name string) error {
	_, err := r.client.Run(ctx, host, "docker rm -f "+shellQuote(name)+" >/dev/null 2>&1 || true")
	return err
}

func (r *Remote) StopContainer(ctx context.Context, host, name string) error {
	_, err := r.client.Run(ctx, host, "docker stop "+shellQuote(name)+" >/dev/null 2>&1 || true")
	return err
}

func (r *Remote) StartContainer(ctx context.Context, host, name string) error {
	_, err := r.client.Run(ctx, host, "docker start "+shellQuote(name))
	return err
}

func (r *Remote) PruneDanglingImages(ctx context.Context, host, service string) error {
	cmd := "docker image prune --force --filter " + shellQuote("label="+LabelService+"="+service) + " >/dev/null"
	_, err := r.client.Run(ctx, host, cmd)
	return err
}

func (r *Remote) Logs(ctx context.Context, host, name string) (string, error) {
	return r.client.Run(ctx, host, "docker logs --tail 200 "+shellQuote(name))
}

func (r *Remote) ContainerState(ctx context.Context, host, name string) (string, error) {
	return r.client.Run(ctx, host, "docker inspect -f '{{.State.Status}} exit={{.State.ExitCode}} error={{.State.Error}}' "+shellQuote(name))
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

func buildxCommand(spec BuildSpec, imageRef string) string {
	args := []string{"docker buildx build",
		"--platform", shellQuote(spec.Platform),
		"--push",
		"-f", shellQuote(spec.Dockerfile),
		"-t", shellQuote(imageRef),
		shellQuote(spec.ContextDir),
	}
	return strings.Join(args, " ")
}

// IsMultiPlatform reports whether a builder.platform value declares more than
// one target platform.
func IsMultiPlatform(platform string) bool {
	return strings.Contains(platform, ",")
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
	if cfg.Builder.IsGit() {
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
	}
	contextDir := filepath.Clean(cfg.Builder.Context)
	return BuildSpec{
		ContextDir: contextDir,
		Dockerfile: filepath.Join(contextDir, cfg.Builder.Dockerfile),
		Platform:   cfg.Builder.Platform,
	}, func() {}, nil
}
