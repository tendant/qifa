package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gokamal/gocart/internal/config"
	"github.com/gokamal/gocart/internal/dockererr"
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
	return runLocalDocker(ctx, extraEnv, "build image", imageRef, args...)
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
	return runLocalDocker(ctx, registryEnv, "build and push image", imageRef, args...)
}

func (l *Local) Push(ctx context.Context, registryCfg config.Registry, imageRef string) error {
	registryEnv, cleanup, err := registry.LocalEnv(registryCfg)
	if err != nil {
		return err
	}
	defer cleanup()
	return runLocalDocker(ctx, registryEnv, "push image", imageRef, "push", imageRef)
}

type Remote struct {
	client *ssh.Client
	out    io.Writer

	// retries is the number of EXTRA attempts for network-transient
	// operations (pull/push). Zero means a single attempt.
	retries   int
	retryWait time.Duration
	// stallWarn is how long a pull may print nothing before qifa says so. A
	// silent hang is the least debuggable failure mode there is: the operator
	// cannot tell a slow layer from a black-holed connection.
	stallWarn time.Duration
}

const (
	defaultPullRetries   = 2
	defaultPullRetryWait = 5 * time.Second
	defaultStallWarn     = 45 * time.Second
)

func NewRemote(client *ssh.Client, out io.Writer) *Remote {
	if out == nil {
		out = os.Stdout
	}
	return &Remote{
		client:    client,
		out:       out,
		retries:   envInt("QIFA_PULL_RETRIES", defaultPullRetries),
		retryWait: envDuration("QIFA_PULL_RETRY_WAIT", defaultPullRetryWait),
		stallWarn: envDuration("QIFA_PULL_STALL_WARN", defaultStallWarn),
	}
}

// envInt/envDuration expose the retry policy as environment variables rather
// than config: how flaky the path to a registry is belongs to the network the
// deploy runs from, not to the app being deployed. An unparseable value falls
// back to the default rather than failing a deploy over a tuning knob.
func envInt(name string, fallback int) int {
	value, ok := os.LookupEnv(name)
	if !ok {
		return fallback
	}
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || n < 0 {
		return fallback
	}
	return n
}

func envDuration(name string, fallback time.Duration) time.Duration {
	value, ok := os.LookupEnv(name)
	if !ok {
		return fallback
	}
	d, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil || d < 0 {
		return fallback
	}
	return d
}

// WithRetries overrides the retry policy for transient pull/push failures.
func (r *Remote) WithRetries(retries int, wait time.Duration) *Remote {
	if retries >= 0 {
		r.retries = retries
	}
	if wait > 0 {
		r.retryWait = wait
	}
	return r
}

func (r *Remote) EnsureDocker(ctx context.Context, host string) error {
	if _, err := r.client.Run(ctx, host, "docker info >/dev/null"); err != nil {
		return dockererr.Wrap(ctx, r.client, "docker check", host, "", 1, err)
	}
	return nil
}

// Pull fetches an image on the remote host, retrying transient network faults
// and, when it finally gives up, explaining the failure: what docker said,
// what that means, and what the host itself reports about reaching the
// registry (DNS, TCP, TLS, proxy, disk, clock).
func (r *Remote) Pull(ctx context.Context, host, dockerConfigDir, imageRef string) error {
	command := withDockerConfig(dockerConfigDir, "docker pull "+shellQuote(imageRef))
	attempts := r.retries + 1
	var lastErr error
	used := 0
	for attempt := 1; attempt <= attempts; attempt++ {
		used = attempt
		if attempt > 1 {
			fmt.Fprintf(r.out, "==> retrying pull of %s on %s (attempt %d/%d)\n", imageRef, host, attempt, attempts)
		} else {
			fmt.Fprintf(r.out, "==> pulling %s on %s (registry %s)\n", imageRef, host, dockererr.RegistryHost(imageRef))
		}
		err := r.streamWatched(ctx, host, command, fmt.Sprintf("pull of %s", imageRef))
		if err == nil {
			return nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return err
		}
		if !dockererr.Retryable(err) || attempt == attempts {
			break
		}
		cause, _ := dockererr.ClassifyErr(err)
		fmt.Fprintf(r.out, "==> pull failed (%s); retrying in %s\n", cause.Summary, r.retryWait)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(r.retryWait):
		}
	}
	return dockererr.Wrap(ctx, r.client, "pull image", host, imageRef, used, lastErr)
}

func (r *Remote) Push(ctx context.Context, host, dockerConfigDir, imageRef string) error {
	command := withDockerConfig(dockerConfigDir, "docker push "+shellQuote(imageRef))
	attempts := r.retries + 1
	var lastErr error
	used := 0
	for attempt := 1; attempt <= attempts; attempt++ {
		used = attempt
		if attempt > 1 {
			fmt.Fprintf(r.out, "==> retrying push of %s from %s (attempt %d/%d)\n", imageRef, host, attempt, attempts)
		}
		err := r.streamWatched(ctx, host, command, fmt.Sprintf("push of %s", imageRef))
		if err == nil {
			return nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return err
		}
		if !dockererr.Retryable(err) || attempt == attempts {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(r.retryWait):
		}
	}
	return dockererr.Wrap(ctx, r.client, "push image", host, imageRef, used, lastErr)
}

// streamWatched runs command on host, streaming its output, and prints a
// notice whenever the command goes quiet for longer than stallWarn so a stuck
// transfer is visible while it is happening rather than at the timeout.
func (r *Remote) streamWatched(ctx context.Context, host, command, what string) error {
	if r.stallWarn <= 0 {
		return r.client.Stream(ctx, host, command, r.out)
	}
	watcher := newStallWatcher(r.out, r.stallWarn, host, what)
	defer watcher.stop()
	return r.client.Stream(ctx, host, command, watcher)
}

// stallWatcher forwards writes and, in the background, reports how long it has
// been since the last one.
type stallWatcher struct {
	out      io.Writer
	mu       sync.Mutex
	lastData time.Time
	done     chan struct{}
	stopOnce sync.Once
}

func newStallWatcher(out io.Writer, after time.Duration, host, what string) *stallWatcher {
	w := &stallWatcher{out: out, lastData: time.Now(), done: make(chan struct{})}
	go func() {
		ticker := time.NewTicker(after)
		defer ticker.Stop()
		for {
			select {
			case <-w.done:
				return
			case <-ticker.C:
				w.mu.Lock()
				idle := time.Since(w.lastData)
				w.mu.Unlock()
				if idle >= after {
					fmt.Fprintf(out, "==> %s on %s has produced no output for %s — still waiting (slow link, or traffic to the registry is being dropped)\n",
						what, host, idle.Round(time.Second))
				}
			}
		}
	}()
	return w
}

func (w *stallWatcher) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.lastData = time.Now()
	w.mu.Unlock()
	return w.out.Write(p)
}

func (w *stallWatcher) stop() { w.stopOnce.Do(func() { close(w.done) }) }

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
				Dockerfile: remoteDockerfileIn("", contextDir, cfg.Builder.Dockerfile),
				Platform:   cfg.Builder.Platform,
			}, imageRef)),
		}, " && ")
		return dockererr.WrapKnownNetwork(ctx, r.client, "build and push image", host, imageRef, 1,
			r.streamWatched(ctx, host, command, "buildx of "+imageRef))
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
			Dockerfile: remoteDockerfileIn(cfg.Builder.Context, remoteContext, cfg.Builder.Dockerfile),
			Platform:   cfg.Builder.Platform,
		}, imageRef)),
		"rm -f " + shellQuote(remoteArchive),
	}, " && ")
	return dockererr.WrapKnownNetwork(ctx, r.client, "build and push image", host, imageRef, 1,
		r.streamWatched(ctx, host, command, "buildx of "+imageRef))
}

func (r *Remote) Build(ctx context.Context, host string, cfg *config.Config, imageRef string) error {
	if cfg.Builder.IsGit() {
		remoteRoot := fmt.Sprintf("/tmp/qifa-build-%d", time.Now().UTC().UnixNano())
		repoDir := filepath.Join(remoteRoot, "repo")
		contextDir := filepath.Join(repoDir, cfg.Builder.Subdir)
		command := strings.Join([]string{
			"rm -rf " + shellQuote(remoteRoot),
			"mkdir -p " + shellQuote(remoteRoot),
			"echo '==> cloning " + cfg.Builder.Repo + " (" + cfg.Builder.Ref + ")'",
			"git clone --progress " + shellQuote(cfg.Builder.Repo) + " " + shellQuote(repoDir),
			"git -C " + shellQuote(repoDir) + " checkout " + shellQuote(cfg.Builder.Ref),
			"echo '==> running docker build'",
			buildCommand(BuildSpec{
				ContextDir: contextDir,
				Dockerfile: remoteDockerfileIn("", contextDir, cfg.Builder.Dockerfile),
				Platform:   cfg.Builder.Platform,
			}, imageRef),
		}, " && ")
		// A build pulls its base images, so build failures share the registry
		// and network causes that pulls have.
		return dockererr.WrapKnownNetwork(ctx, r.client, "build image", host, imageRef, 1,
			r.streamWatched(ctx, host, command, "build of "+imageRef))
	}
	fmt.Fprintf(r.out, "==> compressing build context %s\n", cfg.Builder.Context)
	archive, err := buildContextArchive(cfg.Builder.Context)
	if err != nil {
		return err
	}
	remoteRoot := fmt.Sprintf("/tmp/qifa-build-%d", time.Now().UTC().UnixNano())
	remoteArchive := filepath.Join(remoteRoot, "context.tar")
	remoteContext := filepath.Join(remoteRoot, "context")
	fmt.Fprintf(r.out, "==> uploading build context to %s (%d KB)\n", host, len(archive)/1024)
	if err := r.client.Upload(ctx, host, remoteArchive, archive, 0o600); err != nil {
		return err
	}
	command := strings.Join([]string{
		"rm -rf " + shellQuote(remoteContext),
		"mkdir -p " + shellQuote(remoteContext),
		"echo '==> extracting build context'",
		"tar -xf " + shellQuote(remoteArchive) + " -C " + shellQuote(remoteContext),
		"echo '==> running docker build'",
		buildCommand(BuildSpec{
			ContextDir: remoteContext,
			Dockerfile: remoteDockerfileIn(cfg.Builder.Context, remoteContext, cfg.Builder.Dockerfile),
			Platform:   cfg.Builder.Platform,
		}, imageRef),
		"rm -f " + shellQuote(remoteArchive),
	}, " && ")
	return dockererr.WrapKnownNetwork(ctx, r.client, "build image", host, imageRef, 1,
		r.streamWatched(ctx, host, command, "build of "+imageRef))
}

func (r *Remote) RunContainer(ctx context.Context, host, name, imageRef, envFile, command, network string, labels map[string]string, volumes []string, hostPort, containerPort int, privileged bool, extraPublish []string) error {
	if err := r.ensureVolumeHostDirs(ctx, host, volumes); err != nil {
		return err
	}
	var args []string
	args = append(args, "docker run -d --restart unless-stopped")
	args = append(args, "--name "+shellQuote(name))
	if network != "" {
		args = append(args, "--network "+shellQuote(network))
	}
	if privileged {
		args = append(args, "--privileged")
	}
	for _, key := range sortedKeys(labels) {
		args = append(args, "--label "+shellQuote(key+"="+labels[key]))
	}
	for _, v := range volumes {
		args = append(args, "--volume "+shellQuote(v))
	}
	if envFile != "" {
		args = append(args, "--env-file "+shellQuote(envFile))
	}
	if hostPort > 0 && containerPort > 0 {
		args = append(args, fmt.Sprintf("-p %d:%d", hostPort, containerPort))
	}
	for _, p := range extraPublish {
		args = append(args, "-p "+shellQuote(p))
	}
	if command != "" {
		args = append(args, shellQuote(imageRef)+" "+command)
	} else {
		args = append(args, shellQuote(imageRef))
	}
	if _, err := r.client.Run(ctx, host, strings.Join(args, " ")); err != nil {
		// `docker run` pulls implicitly when the image is missing, so this can
		// fail for exactly the same registry/network reasons a pull does.
		return dockererr.WrapKnownNetwork(ctx, r.client, "start container "+name+" from", host, imageRef, 1, err)
	}
	return nil
}

// ensureVolumeHostDirs runs mkdir -p on the host side of each bind-mount
// volume. Named volumes (e.g. "myvol:/data") are skipped — Docker creates
// those automatically. Paths that already exist (file or dir) are also
// skipped — single-file bind mounts (e.g. config.json:/etc/app/config.json)
// are valid and the file is typically pre-populated by the files: block,
// so blindly mkdir'ing it would fail with "File exists".
func (r *Remote) ensureVolumeHostDirs(ctx context.Context, host string, volumes []string) error {
	var dirs []string
	for _, v := range volumes {
		parts := strings.SplitN(v, ":", 2)
		if len(parts) < 2 {
			continue
		}
		host_path := parts[0]
		// Bind mount: starts with / or . or ~. Named volume: doesn't.
		if !strings.HasPrefix(host_path, "/") && !strings.HasPrefix(host_path, ".") && !strings.HasPrefix(host_path, "~") {
			continue
		}
		dirs = append(dirs, host_path)
	}
	if len(dirs) == 0 {
		return nil
	}
	quoted := make([]string, 0, len(dirs))
	for _, d := range dirs {
		quoted = append(quoted, shellQuote(d))
	}
	// Skip mkdir if the path already exists (file or directory). Real
	// mkdir errors (permission denied, etc.) still abort via `|| exit 1`.
	cmd := fmt.Sprintf(
		`for p in %s; do [ -e "$p" ] || mkdir -p "$p" || exit 1; done`,
		strings.Join(quoted, " "),
	)
	_, err := r.client.Run(ctx, host, cmd)
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
	out, err := r.client.Run(ctx, host, "docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "+shellQuote(name))
	if err != nil {
		return "", err
	}
	return parseContainerIP(out, name, host)
}

// parseContainerIP validates what the inspect template produced. A container
// with no address is the normal case for one that is restarting, and docker
// says so in a way that reads like an address: docker 28 renders the unset
// netip.Addr as the literal "invalid IP", older daemons print nothing. Left
// unchecked it reached the healthcheck as http://invalid IP:8090/readyz,
// whose "Could not resolve host: invalid" buried the real problem — that the
// container never stayed up.
func parseContainerIP(out, name, host string) (string, error) {
	ip := strings.TrimSpace(out)
	if ip != "" && net.ParseIP(ip) != nil {
		return ip, nil
	}
	detail := "docker reported no address"
	if ip != "" {
		detail = fmt.Sprintf("docker reported %q", ip)
	}
	return "", fmt.Errorf("container %s on %s has no IP address (%s) — it is not running; see its state and logs below", name, host, detail)
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

// ImageExists reports whether an image is already present on the host. It uses
// an `&& echo yes || echo no` sentinel so a missing image (docker inspect exits
// nonzero) is distinguished from an SSH/transport failure, which surfaces as a
// real error rather than a false "absent".
func (r *Remote) ImageExists(ctx context.Context, host, image string) (bool, error) {
	out, err := r.client.Run(ctx, host, "docker image inspect "+shellQuote(image)+" >/dev/null 2>&1 && echo yes || echo no")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "yes", nil
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

// Logs returns the container's stdout AND stderr. docker logs sends each to
// the matching stream, and a crashing program says why on stderr — so without
// 2>&1 the deploy diagnostics showed a container's ordinary startup chatter
// and silently dropped the error that killed it.
func (r *Remote) Logs(ctx context.Context, host, name string) (string, error) {
	return r.client.Run(ctx, host, "docker logs --tail 200 "+shellQuote(name)+" 2>&1")
}

// LogsStream pipes `docker logs --tail <lines> [--follow] <name>` to out in
// real time. Cancel ctx (e.g. via Ctrl-C in the CLI) to stop a follow.
func (r *Remote) LogsStream(ctx context.Context, host, name string, lines int, follow bool, out io.Writer) error {
	cmd := fmt.Sprintf("docker logs --tail %d", lines)
	if follow {
		cmd += " --follow"
	}
	cmd += " " + shellQuote(name)
	return r.client.Stream(ctx, host, cmd, out)
}

func (r *Remote) ContainerState(ctx context.Context, host, name string) (string, error) {
	return r.client.Run(ctx, host, "docker inspect -f '{{.State.Status}} exit={{.State.ExitCode}} error={{.State.Error}}' "+shellQuote(name))
}

func (r *Remote) Exec(ctx context.Context, host, name, command string) (string, error) {
	return r.client.Run(ctx, host, "docker exec "+shellQuote(name)+" sh -lc "+shellQuote(command))
}

// ExecStream runs a command in the container and pipes BOTH its streams to
// out. Capturing stdout alone loses everything a program reports on stderr —
// `nginx -v`, most CLIs' errors, every usage message — which makes an
// interactive exec look like it silently did nothing.
func (r *Remote) ExecStream(ctx context.Context, host, name, command string, out io.Writer) error {
	return r.client.Stream(ctx, host, "docker exec "+shellQuote(name)+" sh -lc "+shellQuote(command), out)
}

// dockerfileIn resolves builder.dockerfile against a build context on the
// machine that will run the build. An absolute path is honoured as-is —
// `docker build -f /abs/Dockerfile ctx` is valid and people write it —
// whereas joining it to the context produced paths like
// "<context>/private/tmp/..." and the opaque error "unable to evaluate
// symlinks in Dockerfile path".
func dockerfileIn(contextDir, dockerfile string) string {
	if filepath.IsAbs(dockerfile) {
		return dockerfile
	}
	return filepath.Join(contextDir, dockerfile)
}

// remoteDockerfileIn resolves it against the context as it exists on the
// REMOTE host, where a local absolute path does not exist. An absolute path
// that points inside the local context keeps its position within the
// uploaded copy; anything else falls back to its base name, which is where a
// single-file Dockerfile lands.
func remoteDockerfileIn(localContext, remoteContext, dockerfile string) string {
	if !filepath.IsAbs(dockerfile) {
		return filepath.Join(remoteContext, dockerfile)
	}
	if localContext != "" {
		if rel, err := filepath.Rel(localContext, dockerfile); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.Join(remoteContext, rel)
		}
	}
	return filepath.Join(remoteContext, filepath.Base(dockerfile))
}

func runLocal(ctx context.Context, binary string, args ...string) error {
	return runLocalEnv(ctx, nil, binary, args...)
}

func runLocalEnv(ctx context.Context, extraEnv map[string]string, binary string, args ...string) error {
	_, err := runLocalCapture(ctx, extraEnv, binary, args...)
	return err
}

// runLocalCapture runs a command, streaming its output to the terminal while
// keeping a bounded tail so failures can be classified. Returns that tail.
func runLocalCapture(ctx context.Context, extraEnv map[string]string, binary string, args ...string) (string, error) {
	var tail bytes.Buffer
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdout = io.MultiWriter(os.Stdout, &tail)
	cmd.Stderr = io.MultiWriter(os.Stderr, &tail)
	cmd.Env = os.Environ()
	for key, value := range extraEnv {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	err := cmd.Run()
	output := tail.String()
	if len(output) > 8<<10 {
		output = output[len(output)-(8<<10):]
	}
	return output, err
}

// runLocalDocker runs a local docker command and explains a failure the same
// way remote ones are explained. There is no host to probe — the connectivity
// in question is this machine's — so only the cause and hint are added.
func runLocalDocker(ctx context.Context, extraEnv map[string]string, action, imageRef string, args ...string) error {
	output, err := runLocalCapture(ctx, extraEnv, "docker", args...)
	if err == nil {
		return nil
	}
	return dockererr.Wrap(ctx, nil, action, "", imageRef, 1, &localError{output: output, err: err})
}

// localError carries a local command's output to the classifier, mirroring
// ssh.RemoteError for remote ones.
type localError struct {
	output string
	err    error
}

func (e *localError) Error() string        { return e.err.Error() }
func (e *localError) Unwrap() error        { return e.err }
func (e *localError) RemoteOutput() string { return e.output }

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
			Dockerfile: dockerfileIn(contextDir, cfg.Builder.Dockerfile),
			Platform:   cfg.Builder.Platform,
		}, cleanup, nil
	}
	contextDir := filepath.Clean(cfg.Builder.Context)
	return BuildSpec{
		ContextDir: contextDir,
		Dockerfile: dockerfileIn(contextDir, cfg.Builder.Dockerfile),
		Platform:   cfg.Builder.Platform,
	}, func() {}, nil
}
