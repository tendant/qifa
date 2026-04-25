package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/gokamal/gocart/internal/config"
	"github.com/gokamal/gocart/internal/docker"
	"github.com/gokamal/gocart/internal/hooks"
	"github.com/gokamal/gocart/internal/lock"
	"github.com/gokamal/gocart/internal/logs"
	"github.com/gokamal/gocart/internal/proxy"
	"github.com/gokamal/gocart/internal/registry"
	"github.com/gokamal/gocart/internal/secrets"
	"github.com/gokamal/gocart/internal/ssh"
	"github.com/gokamal/gocart/internal/state"
)

type Deployer struct {
	cfg    *config.Config
	store  *state.Store
	log    *logs.Logger
	stderr io.Writer

	localDocker  *docker.Local
	remoteDocker *docker.Remote
	ssh          *ssh.Client
	proxy        proxy.Proxy
}

func New(cfg *config.Config, store *state.Store, stdout, stderr io.Writer) (*Deployer, error) {
	sshClient := ssh.New(cfg.SSH)
	return &Deployer{
		cfg:          cfg,
		store:        store,
		log:          logs.New(stdout),
		stderr:       stderr,
		localDocker:  docker.NewLocal(),
		remoteDocker: docker.NewRemote(sshClient),
		ssh:          sshClient,
		proxy:        proxy.New(sshClient, cfg.Proxy),
	}, nil
}

func (d *Deployer) Deploy(ctx context.Context) error {
	imageRef, version, err := d.resolveImage(ctx)
	if err != nil {
		return err
	}
	locker := lock.New(d.ssh, d.cfg.Service, d.uniqueHosts())
	if err := locker.Acquire(ctx, version); err != nil {
		return err
	}
	defer locker.Release(context.Background())
	deployment := state.Deployment{
		ID:        deploymentID(version),
		Service:   d.cfg.Service,
		Version:   version,
		Image:     imageRef,
		Status:    state.StatusPending,
		StartedAt: time.Now().UTC(),
	}
	if err := d.store.AppendDeployment(deployment); err != nil {
		return err
	}

	if err := hooks.Run(ctx, d.cfg.Hooks.PreBuild, map[string]string{"QIFA_VERSION": version}); err != nil {
		return err
	}
	if err := d.SweepStaleContainers(ctx); err != nil {
		return d.failDeployment(deployment, err)
	}
	if d.cfg.Builder != nil {
		if err := d.updateStatus(deployment, state.StatusBuilding); err != nil {
			return err
		}
		d.log.Printf("building image %s", imageRef)
	} else {
		d.log.Printf("deploying external image %s", imageRef)
	}
	if err := d.prepareImage(ctx, deployment, imageRef); err != nil {
		return d.failDeployment(deployment, err)
	}

	envFile, err := secrets.Render(d.cfg.Env.Clear, d.cfg.Env.Secret, d.cfg.Env.SecretCommand)
	if err != nil {
		return d.failDeployment(deployment, err)
	}

	for _, role := range orderedRoles(d.cfg.Servers) {
		server := d.cfg.Servers[role]
		if err := d.rolloutRole(ctx, deployment, role, server, imageRef, envFile); err != nil {
			return d.failDeployment(deployment, err)
		}
	}

	if err := d.updateStatus(deployment, state.StatusSucceeded); err != nil {
		return err
	}
	if err := hooks.Run(ctx, d.cfg.Hooks.PostDeploy, map[string]string{"QIFA_VERSION": version}); err != nil {
		return err
	}
	if err := d.Prune(ctx); err != nil {
		d.log.Printf("prune warning: %v", err)
	}
	d.log.Printf("deployment %s succeeded", deployment.ID)
	return nil
}

func (d *Deployer) Prune(ctx context.Context) error {
	retain := d.cfg.Prune.RetainContainers
	if retain <= 0 {
		retain = 5
	}
	hosts := d.uniqueHosts()
	for _, host := range hosts {
		for _, role := range orderedRoles(d.cfg.Servers) {
			containers, err := d.remoteDocker.ListContainersByService(ctx, host, d.cfg.Service, role)
			if err != nil {
				return err
			}
			stopped := make([]docker.ContainerInfo, 0, len(containers))
			for _, c := range containers {
				if c.State == "running" || c.State == "restarting" {
					continue
				}
				stopped = append(stopped, c)
			}
			if len(stopped) <= retain {
				continue
			}
			for _, c := range stopped[retain:] {
				if err := d.remoteDocker.StopAndRemove(ctx, host, c.Name); err != nil {
					return err
				}
			}
		}
		if err := d.remoteDocker.PruneDanglingImages(ctx, host, d.cfg.Service); err != nil {
			return err
		}
	}
	return nil
}

func (d *Deployer) uniqueHosts() []string {
	seen := map[string]struct{}{}
	var hosts []string
	for _, role := range orderedRoles(d.cfg.Servers) {
		for _, host := range d.cfg.Servers[role].Hosts {
			if _, ok := seen[host]; ok {
				continue
			}
			seen[host] = struct{}{}
			hosts = append(hosts, host)
		}
	}
	return hosts
}

func (d *Deployer) rolloutRole(ctx context.Context, deployment state.Deployment, role string, server config.Server, imageRef string, envFile []byte) error {
	buildOnHost := d.cfg.Builder.IsPerTarget()
	hosts := server.Hosts
	batchSize := 1
	if d.cfg.Rollout.BatchSize != nil {
		batchSize = *d.cfg.Rollout.BatchSize
	}
	if batchSize <= 0 || batchSize > len(hosts) {
		batchSize = len(hosts)
	}
	for i := 0; i < len(hosts); i += batchSize {
		end := i + batchSize
		if end > len(hosts) {
			end = len(hosts)
		}
		batch := hosts[i:end]
		if i > 0 && d.cfg.Rollout.BatchWait > 0 {
			select {
			case <-time.After(d.cfg.Rollout.BatchWait):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if err := d.runBatch(ctx, deployment, role, server, imageRef, envFile, batch, buildOnHost); err != nil {
			return err
		}
	}
	return nil
}

func (d *Deployer) runBatch(ctx context.Context, deployment state.Deployment, role string, server config.Server, imageRef string, envFile []byte, hosts []string, buildOnHost bool) error {
	if len(hosts) == 1 {
		return d.deployHost(ctx, deployment, role, hosts[0], server, imageRef, envFile, buildOnHost)
	}
	var wg sync.WaitGroup
	errCh := make(chan error, len(hosts))
	for _, host := range hosts {
		wg.Add(1)
		go func(host string) {
			defer wg.Done()
			if err := d.deployHost(ctx, deployment, role, host, server, imageRef, envFile, buildOnHost); err != nil {
				errCh <- fmt.Errorf("%s: %w", host, err)
			}
		}(host)
	}
	wg.Wait()
	close(errCh)
	var errs []string
	for err := range errCh {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func (d *Deployer) deployHost(ctx context.Context, deployment state.Deployment, role, host string, server config.Server, imageRef string, envFile []byte, buildOnHost bool) error {
	containerName := d.containerName(role, deployment.Version)
	remoteEnv := fmt.Sprintf("/tmp/%s.env", containerName)
	appPort := d.appPort(role, server)
	useProxy := serverUsesProxy(role, server)

	if err := d.appendEvent(deployment.ID, host, role, "connecting", "connecting to host"); err != nil {
		return err
	}
	if err := d.remoteDocker.EnsureDocker(ctx, host); err != nil {
		return err
	}
	if useProxy {
		if err := d.proxy.EnsureInstalled(ctx, host); err != nil {
			return err
		}
	}
	previousContainer, err := d.findRunningContainer(ctx, host, role)
	if err != nil {
		return err
	}
	if err := d.ssh.Upload(ctx, host, remoteEnv, envFile, 0o600); err != nil {
		return err
	}
	if buildOnHost {
		if err := d.updateStatus(deployment, state.StatusBuilding); err != nil {
			return err
		}
		if err := d.remoteDocker.Build(ctx, host, d.cfg, imageRef); err != nil {
			return err
		}
	} else if d.cfg.Registry.Enabled() || d.cfg.Builder == nil {
		dockerConfigDir := ""
		if d.cfg.Registry.Enabled() {
			var err error
			dockerConfigDir, err = registry.Login(ctx, d.ssh, d.cfg.Registry, host)
			if err != nil {
				return err
			}
		}
		if err := d.updateStatus(deployment, state.StatusPulling); err != nil {
			return err
		}
		if err := d.remoteDocker.Pull(ctx, host, dockerConfigDir, imageRef); err != nil {
			return err
		}
	}
	if err := d.updateStatus(deployment, state.StatusStarting); err != nil {
		return err
	}
	publishedPort := 0
	containerPort := 0
	if !useProxy {
		publishedPort = server.Port
		containerPort = appPort
		if previousContainer != "" {
			if err := d.remoteDocker.StopContainer(ctx, host, previousContainer); err != nil {
				return err
			}
			previousContainer = ""
		}
	}
	containerNetwork := ""
	if useProxy {
		containerNetwork = d.cfg.Proxy.Network
	}
	labels := map[string]string{
		docker.LabelService: d.cfg.Service,
		docker.LabelRole:    role,
		docker.LabelVersion: deployment.Version,
	}
	if err := d.remoteDocker.StopAndRemove(ctx, host, containerName); err != nil {
		return err
	}
	if err := d.remoteDocker.RunContainer(ctx, host, containerName, imageRef, remoteEnv, server.Cmd, containerNetwork, labels, publishedPort, containerPort); err != nil {
		_ = d.remoteDocker.StopAndRemove(ctx, host, containerName)
		return err
	}
	if err := d.updateStatus(deployment, state.StatusHealthChecking); err != nil {
		return err
	}
	targetHost := "127.0.0.1"
	healthPort := appPort
	if useProxy {
		containerIP, err := d.remoteDocker.ContainerIP(ctx, host, containerName)
		if err != nil {
			return err
		}
		targetHost = containerIP
	} else {
		healthPort = server.Port
	}
	if err := d.healthCheck(ctx, host, targetHost, healthPort, containerName, useProxy); err != nil {
		return err
	}
	if useProxy {
		if err := d.updateStatus(deployment, state.StatusSwitchingTraffic); err != nil {
			return err
		}
		if err := d.proxy.Deploy(ctx, host, proxy.Target{
			Service: d.cfg.Service,
			Host:    targetHost,
			Port:    appPort,
		}); err != nil {
			return err
		}
	}
	if err := d.appendEvent(deployment.ID, host, role, "deployed", "host deployed successfully"); err != nil {
		return err
	}
	if err := d.cleanupPriorContainer(ctx, host, previousContainer, containerName); err != nil {
		return err
	}
	return nil
}

func serverUsesProxy(role string, server config.Server) bool {
	if server.Proxy != nil {
		return *server.Proxy
	}
	return role == "web"
}

func (d *Deployer) Rollback(ctx context.Context, version string) error {
	locker := lock.New(d.ssh, d.cfg.Service, d.uniqueHosts())
	if err := locker.Acquire(ctx, version); err != nil {
		return err
	}
	defer locker.Release(context.Background())
	if err := hooks.Run(ctx, d.cfg.Hooks.PreRollback, nil); err != nil {
		return err
	}
	var prevVersion, prevImage string
	var err error
	if version == "" {
		prevVersion, prevImage, err = d.findPreviousVersion(ctx)
	} else {
		prevVersion, prevImage, err = d.findVersion(ctx, version)
	}
	if err != nil {
		return err
	}
	deployment := state.Deployment{
		ID:        deploymentID(prevVersion + "-rollback"),
		Service:   d.cfg.Service,
		Version:   prevVersion,
		Image:     prevImage,
		Status:    state.StatusRolledBack,
		StartedAt: time.Now().UTC(),
	}
	if err := d.store.AppendDeployment(deployment); err != nil {
		return err
	}

	envFile, err := secrets.Render(d.cfg.Env.Clear, d.cfg.Env.Secret, d.cfg.Env.SecretCommand)
	if err != nil {
		return err
	}
	for _, role := range orderedRoles(d.cfg.Servers) {
		server := d.cfg.Servers[role]
		for _, host := range server.Hosts {
			if err := d.deployHost(ctx, deployment, role, host, server, prevImage, envFile, false); err != nil {
				return err
			}
		}
	}
	if err := d.updateStatus(deployment, state.StatusRolledBack); err != nil {
		return err
	}
	d.log.Printf("rolled back to %s", prevImage)
	return nil
}

// findVersion locates an existing labeled container with the given version on
// every host of every role. Returns the version+image if found on every host;
// errors if any host is missing a container with that version.
func (d *Deployer) findVersion(ctx context.Context, version string) (string, string, error) {
	image := ""
	for _, role := range orderedRoles(d.cfg.Servers) {
		server := d.cfg.Servers[role]
		for _, host := range server.Hosts {
			containers, err := d.remoteDocker.ListContainersByService(ctx, host, d.cfg.Service, role)
			if err != nil {
				return "", "", err
			}
			matched := false
			for _, c := range containers {
				if c.Version == version {
					if image == "" {
						image = c.Image
					}
					matched = true
					break
				}
			}
			if !matched {
				return "", "", fmt.Errorf("version %q not found for %s/%s on %s", version, d.cfg.Service, role, host)
			}
		}
	}
	if image == "" {
		return "", "", fmt.Errorf("version %q not found", version)
	}
	return version, image, nil
}

func (d *Deployer) findPreviousVersion(ctx context.Context) (string, string, error) {
	var best docker.ContainerInfo
	var found bool
	for _, role := range orderedRoles(d.cfg.Servers) {
		server := d.cfg.Servers[role]
		for _, host := range server.Hosts {
			containers, err := d.remoteDocker.ListContainersByService(ctx, host, d.cfg.Service, role)
			if err != nil {
				return "", "", err
			}
			currentVersion := ""
			for _, c := range containers {
				if c.State == "running" || c.State == "restarting" {
					currentVersion = c.Version
					break
				}
			}
			for _, c := range containers {
				if c.Version == "" || c.Version == currentVersion {
					continue
				}
				if !found || c.CreatedAt.After(best.CreatedAt) {
					best = c
					found = true
				}
				break
			}
		}
	}
	if !found {
		return "", "", errors.New("no previous version found")
	}
	return best.Version, best.Image, nil
}

func (d *Deployer) Stop(ctx context.Context) error {
	for _, role := range orderedRoles(d.cfg.Servers) {
		server := d.cfg.Servers[role]
		for _, host := range server.Hosts {
			name, err := d.findRunningContainer(ctx, host, role)
			if err != nil {
				return err
			}
			if name == "" {
				continue
			}
			if err := d.remoteDocker.StopContainer(ctx, host, name); err != nil {
				return err
			}
			d.log.Printf("stopped %s on %s", name, host)
		}
	}
	return nil
}

func (d *Deployer) Start(ctx context.Context) error {
	for _, role := range orderedRoles(d.cfg.Servers) {
		server := d.cfg.Servers[role]
		useProxy := serverUsesProxy(role, server)
		appPort := d.appPort(role, server)
		for _, host := range server.Hosts {
			containers, err := d.remoteDocker.ListContainersByService(ctx, host, d.cfg.Service, role)
			if err != nil {
				return err
			}
			if len(containers) == 0 {
				return fmt.Errorf("no container found for %s/%s on %s", d.cfg.Service, role, host)
			}
			top := containers[0]
			if top.State != "running" && top.State != "restarting" {
				if err := d.remoteDocker.StartContainer(ctx, host, top.Name); err != nil {
					return err
				}
				d.log.Printf("started %s on %s", top.Name, host)
			}
			if useProxy {
				if err := d.proxy.EnsureInstalled(ctx, host); err != nil {
					return err
				}
				containerIP, err := d.remoteDocker.ContainerIP(ctx, host, top.Name)
				if err != nil {
					return err
				}
				if err := d.proxy.Deploy(ctx, host, proxy.Target{Service: d.cfg.Service, Host: containerIP, Port: appPort}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (d *Deployer) Restart(ctx context.Context) error {
	if err := d.Stop(ctx); err != nil {
		return err
	}
	return d.Start(ctx)
}

func (d *Deployer) Remove(ctx context.Context) error {
	for _, host := range d.uniqueHosts() {
		anyProxy := false
		for _, role := range orderedRoles(d.cfg.Servers) {
			server := d.cfg.Servers[role]
			if serverUsesProxy(role, server) {
				anyProxy = true
				break
			}
		}
		if anyProxy {
			if err := d.proxy.Remove(ctx, host, d.cfg.Service); err != nil {
				d.log.Printf("proxy remove warning on %s: %v", host, err)
			}
		}
		for _, role := range orderedRoles(d.cfg.Servers) {
			containers, err := d.remoteDocker.ListContainersByService(ctx, host, d.cfg.Service, role)
			if err != nil {
				return err
			}
			for _, c := range containers {
				if err := d.remoteDocker.StopAndRemove(ctx, host, c.Name); err != nil {
					return err
				}
			}
		}
		if err := d.remoteDocker.PruneDanglingImages(ctx, host, d.cfg.Service); err != nil {
			return err
		}
	}
	d.log.Printf("removed all containers for %s", d.cfg.Service)
	return nil
}

func (d *Deployer) prepareImage(ctx context.Context, deployment state.Deployment, imageRef string) error {
	switch {
	case d.cfg.Builder == nil, d.cfg.Builder.IsPerTarget():
		return nil
	case d.cfg.Builder.IsLocal():
		if docker.IsMultiPlatform(d.cfg.Builder.Platform) {
			// buildx --push is one shot: build all platforms, push manifest list.
			if err := d.updateStatus(deployment, state.StatusPushing); err != nil {
				return err
			}
			return d.localDocker.BuildxPush(ctx, d.cfg, imageRef)
		}
		if err := d.localDocker.Build(ctx, d.cfg, imageRef); err != nil {
			return err
		}
		if err := d.updateStatus(deployment, state.StatusPushing); err != nil {
			return err
		}
		return d.localDocker.Push(ctx, d.cfg.Registry, imageRef)
	default: // remote builder
		if err := d.remoteDocker.EnsureDocker(ctx, d.cfg.Builder.Host); err != nil {
			return err
		}
		if docker.IsMultiPlatform(d.cfg.Builder.Platform) {
			dockerConfigDir, err := registry.Login(ctx, d.ssh, d.cfg.Registry, d.cfg.Builder.Host)
			if err != nil {
				return err
			}
			if err := d.updateStatus(deployment, state.StatusPushing); err != nil {
				return err
			}
			return d.remoteDocker.BuildxPush(ctx, d.cfg.Builder.Host, d.cfg, dockerConfigDir, imageRef)
		}
		if err := d.remoteDocker.Build(ctx, d.cfg.Builder.Host, d.cfg, imageRef); err != nil {
			return err
		}
		dockerConfigDir, err := registry.Login(ctx, d.ssh, d.cfg.Registry, d.cfg.Builder.Host)
		if err != nil {
			return err
		}
		if err := d.updateStatus(deployment, state.StatusPushing); err != nil {
			return err
		}
		return d.remoteDocker.Push(ctx, d.cfg.Builder.Host, dockerConfigDir, imageRef)
	}
}

func (d *Deployer) resolveImage(ctx context.Context) (imageRef, version string, err error) {
	if d.cfg.Builder != nil {
		v := resolveVersion()
		return fmt.Sprintf("%s:%s", d.cfg.Image, v), v, nil
	}
	if _, err := config.ParseImageVersion(d.cfg.Image); err != nil {
		return "", "", err
	}
	hosts := d.uniqueHosts()
	if len(hosts) == 0 {
		return "", "", errors.New("no hosts configured")
	}
	first := hosts[0]
	if err := d.remoteDocker.EnsureDocker(ctx, first); err != nil {
		return "", "", err
	}
	dockerConfigDir := ""
	if d.cfg.Registry.Enabled() {
		dockerConfigDir, err = registry.Login(ctx, d.ssh, d.cfg.Registry, first)
		if err != nil {
			return "", "", err
		}
	}
	if err := d.remoteDocker.Pull(ctx, first, dockerConfigDir, d.cfg.Image); err != nil {
		return "", "", err
	}
	digest, err := d.remoteDocker.ImageDigest(ctx, first, d.cfg.Image)
	if err != nil {
		return "", "", err
	}
	repo := config.ImageRepo(d.cfg.Image)
	return repo + "@" + digest, shortDigest(digest), nil
}

func shortDigest(digest string) string {
	if i := strings.Index(digest, ":"); i >= 0 {
		digest = digest[i+1:]
	}
	if len(digest) > 12 {
		return digest[:12]
	}
	return digest
}

func (d *Deployer) Status(ctx context.Context, out io.Writer) error {
	deployments, events, err := d.store.Snapshot()
	if err != nil {
		return err
	}
	sort.Slice(deployments, func(i, j int) bool {
		return deployments[i].StartedAt.After(deployments[j].StartedAt)
	})
	for _, dep := range deployments {
		fmt.Fprintf(out, "%s %s %s %s\n", dep.StartedAt.Format(time.RFC3339), dep.Service, dep.Version, dep.Status)
	}
	if len(events) > 0 {
		fmt.Fprintln(out, "")
		for _, event := range events {
			fmt.Fprintf(out, "%s %s %s %s\n", event.CreatedAt.Format(time.RFC3339), event.Role, event.Host, event.Message)
		}
	}
	printedHeader := false
	for _, role := range orderedRoles(d.cfg.Servers) {
		server := d.cfg.Servers[role]
		for _, host := range server.Hosts {
			containers, err := d.remoteDocker.ListContainersByService(ctx, host, d.cfg.Service, role)
			if err != nil {
				fmt.Fprintf(out, "active %s %s ERROR %v\n", role, host, err)
				continue
			}
			for _, c := range containers {
				if c.State != "running" && c.State != "restarting" {
					continue
				}
				if !printedHeader {
					fmt.Fprintln(out, "")
					printedHeader = true
				}
				fmt.Fprintf(out, "active %s %s %s %s %s\n", c.CreatedAt.Format(time.RFC3339), role, host, c.Version, c.Name)
			}
		}
	}
	return nil
}

// Maintenance puts the service into maintenance mode on every host where the
// proxy is running. kamal-proxy returns `message` for incoming requests and
// drains in-flight requests over drainTimeout. Use Live to come back.
func (d *Deployer) Maintenance(ctx context.Context, message string, drainTimeout time.Duration) error {
	for _, host := range d.proxyHosts() {
		if err := d.proxy.EnsureInstalled(ctx, host); err != nil {
			return err
		}
		if err := d.proxy.Stop(ctx, host, d.cfg.Service, message, drainTimeout); err != nil {
			return err
		}
		d.log.Printf("maintenance enabled for %s on %s", d.cfg.Service, host)
	}
	return nil
}

// Live takes the service out of maintenance mode (kamal-proxy resume) on every
// host where the proxy is running.
func (d *Deployer) Live(ctx context.Context) error {
	for _, host := range d.proxyHosts() {
		if err := d.proxy.Resume(ctx, host, d.cfg.Service); err != nil {
			return err
		}
		d.log.Printf("live: %s on %s", d.cfg.Service, host)
	}
	return nil
}

// proxyHosts returns the unique set of hosts where any role uses the proxy.
func (d *Deployer) proxyHosts() []string {
	seen := map[string]struct{}{}
	var hosts []string
	for _, role := range orderedRoles(d.cfg.Servers) {
		server := d.cfg.Servers[role]
		if !serverUsesProxy(role, server) {
			continue
		}
		for _, host := range server.Hosts {
			if _, ok := seen[host]; ok {
				continue
			}
			seen[host] = struct{}{}
			hosts = append(hosts, host)
		}
	}
	return hosts
}

// LockStatus prints the lock holder per host (or "(free)" if not locked).
func (d *Deployer) LockStatus(ctx context.Context, out io.Writer) error {
	locker := lock.New(d.ssh, d.cfg.Service, d.uniqueHosts())
	status, err := locker.Status(ctx)
	if err != nil {
		return err
	}
	for _, host := range d.uniqueHosts() {
		if v := status[host]; v == "" {
			fmt.Fprintf(out, "%s\t(free)\n", host)
		} else {
			fmt.Fprintf(out, "%s\t%s\n", host, v)
		}
	}
	return nil
}

// LockRelease forcibly clears the deploy lock on every configured host.
// For recovery from a deploy that crashed without releasing.
func (d *Deployer) LockRelease(ctx context.Context) error {
	locker := lock.New(d.ssh, d.cfg.Service, d.uniqueHosts())
	return locker.ForceRelease(ctx)
}

// ListContainers prints labeled containers per role/host with their state and
// version, sorted most-recent first. Useful for finding rollback targets.
func (d *Deployer) ListContainers(ctx context.Context, out io.Writer) error {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ROLE\tHOST\tVERSION\tSTATE\tCREATED\tCONTAINER")
	for _, role := range orderedRoles(d.cfg.Servers) {
		for _, host := range d.cfg.Servers[role].Hosts {
			containers, err := d.remoteDocker.ListContainersByService(ctx, host, d.cfg.Service, role)
			if err != nil {
				fmt.Fprintf(tw, "%s\t%s\tERROR\t-\t-\t%v\n", role, host, err)
				continue
			}
			if len(containers) == 0 {
				fmt.Fprintf(tw, "%s\t%s\t-\t-\t-\t(no containers)\n", role, host)
				continue
			}
			for _, c := range containers {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
					role, host, c.Version, c.State,
					c.CreatedAt.Format(time.RFC3339), c.Name)
			}
		}
	}
	return tw.Flush()
}

func (d *Deployer) Logs(ctx context.Context, out io.Writer) error {
	host, container, err := d.defaultTarget(ctx)
	if err != nil {
		return err
	}
	logOutput, err := d.remoteDocker.Logs(ctx, host, container)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, logOutput)
	return err
}

func (d *Deployer) Exec(ctx context.Context, command string, out io.Writer) error {
	host, container, err := d.defaultTarget(ctx)
	if err != nil {
		return err
	}
	result, err := d.remoteDocker.Exec(ctx, host, container, command)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, result)
	return err
}

func (d *Deployer) AccessoryBoot(ctx context.Context, name string) error {
	accessory, ok := d.cfg.Accessories[name]
	if !ok {
		return fmt.Errorf("unknown accessory %s", name)
	}
	containerName := d.cfg.Service + "-accessory-" + name
	if err := d.remoteDocker.Pull(ctx, accessory.Host, "", accessory.Image); err != nil {
		return err
	}
	if err := d.remoteDocker.StopAndRemove(ctx, accessory.Host, containerName); err != nil {
		return err
	}
	return d.remoteDocker.RunContainer(ctx, accessory.Host, containerName, accessory.Image, "", "", "", nil, 0, 0)
}

func (d *Deployer) AccessoryLogs(ctx context.Context, name string, out io.Writer) error {
	accessory, ok := d.cfg.Accessories[name]
	if !ok {
		return fmt.Errorf("unknown accessory %s", name)
	}
	containerName := d.cfg.Service + "-accessory-" + name
	result, err := d.remoteDocker.Logs(ctx, accessory.Host, containerName)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, result)
	return err
}

func (d *Deployer) healthCheck(ctx context.Context, host, targetHost string, targetPort int, containerName string, useProxy bool) error {
	if targetPort == 0 {
		_, err := d.remoteDocker.Exec(ctx, host, containerName, "true")
		if err != nil {
			return d.withContainerDiagnostics(ctx, host, containerName, err)
		}
		return nil
	}
	path := d.cfg.Proxy.Healthcheck.Path
	if path == "" {
		if useProxy {
			path = "/up"
		} else {
			path = "/"
		}
	}
	command := fmt.Sprintf("for i in 1 2 3 4 5; do curl -fsS http://%s:%d%s && exit 0; sleep %d; done; exit 1", targetHost, targetPort, path, int(d.cfg.Proxy.Healthcheck.Interval.Seconds()))
	_, err := d.ssh.Run(ctx, host, command)
	if err != nil {
		return d.withContainerDiagnostics(ctx, host, containerName, err)
	}
	return nil
}

func (d *Deployer) failDeployment(deployment state.Deployment, cause error) error {
	_ = d.updateStatus(deployment, state.StatusFailed)
	return cause
}

func (d *Deployer) withContainerDiagnostics(ctx context.Context, host, containerName string, cause error) error {
	lines := []string{cause.Error()}
	if state, err := d.remoteDocker.ContainerState(ctx, host, containerName); err == nil && strings.TrimSpace(state) != "" {
		lines = append(lines, "container_state: "+strings.TrimSpace(state))
	}
	if logs, err := d.remoteDocker.Logs(ctx, host, containerName); err == nil && strings.TrimSpace(logs) != "" {
		lines = append(lines, "container_logs:")
		lines = append(lines, strings.TrimSpace(logs))
	}
	return fmt.Errorf("%s", strings.Join(lines, "\n"))
}

func (d *Deployer) updateStatus(deployment state.Deployment, next state.Status) error {
	deployment.Status = next
	if next == state.StatusSucceeded || next == state.StatusFailed || next == state.StatusRolledBack {
		now := time.Now().UTC()
		deployment.FinishedAt = &now
	}
	return d.store.AppendDeployment(deployment)
}

func (d *Deployer) appendEvent(deploymentID, host, role, eventType, message string) error {
	return d.store.AppendEvent(state.Event{
		ID:           fmt.Sprintf("%d", time.Now().UnixNano()),
		DeploymentID: deploymentID,
		Host:         host,
		Role:         role,
		EventType:    eventType,
		Message:      message,
		CreatedAt:    time.Now().UTC(),
	})
}

func (d *Deployer) containerName(role, version string) string {
	return fmt.Sprintf("%s-%s-%s", d.cfg.Service, role, version)
}

func (d *Deployer) defaultTarget(ctx context.Context) (host string, container string, err error) {
	for _, role := range orderedRoles(d.cfg.Servers) {
		server := d.cfg.Servers[role]
		for _, h := range server.Hosts {
			name, lookupErr := d.findRunningContainer(ctx, h, role)
			if lookupErr != nil {
				return "", "", lookupErr
			}
			if name != "" {
				return h, name, nil
			}
		}
	}
	return "", "", errors.New("no running container found")
}

func (d *Deployer) cleanupPriorContainer(ctx context.Context, host, previousContainer, currentContainer string) error {
	if previousContainer == "" || previousContainer == currentContainer {
		return nil
	}
	return d.remoteDocker.StopContainer(ctx, host, previousContainer)
}

// SweepStaleContainers walks every role/host and removes any orphan running
// labeled container — defined as any running labeled container that isn't the
// most-recently-created one (the legitimate active). Stopped containers are
// left in place as rollback candidates (managed by Prune). Idempotent in the
// happy case.
func (d *Deployer) SweepStaleContainers(ctx context.Context) error {
	for _, role := range orderedRoles(d.cfg.Servers) {
		for _, host := range d.cfg.Servers[role].Hosts {
			containers, err := d.remoteDocker.ListContainersByService(ctx, host, d.cfg.Service, role)
			if err != nil {
				return err
			}
			sawActive := false
			for _, c := range containers {
				if c.State != "running" && c.State != "restarting" {
					continue
				}
				if !sawActive {
					sawActive = true
					continue
				}
				d.log.Printf("sweeping orphan container %s on %s", c.Name, host)
				if err := d.remoteDocker.StopAndRemove(ctx, host, c.Name); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (d *Deployer) findRunningContainer(ctx context.Context, host, role string) (string, error) {
	containers, err := d.remoteDocker.ListContainersByService(ctx, host, d.cfg.Service, role)
	if err != nil {
		return "", err
	}
	for _, c := range containers {
		if c.State == "running" || c.State == "restarting" {
			return c.Name, nil
		}
	}
	return "", nil
}

func (d *Deployer) appPort(role string, server config.Server) int {
	if server.AppPort > 0 {
		return server.AppPort
	}
	if server.Port > 0 {
		return server.Port
	}
	if role != "web" {
		return 0
	}
	return d.cfg.Proxy.AppPort
}

func orderedRoles(servers map[string]config.Server) []string {
	roles := make([]string, 0, len(servers))
	for role := range servers {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	sort.SliceStable(roles, func(i, j int) bool {
		return roles[i] == "web"
	})
	return roles
}

func resolveVersion() string {
	out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err == nil {
		return strings.TrimSpace(string(out))
	}
	return time.Now().UTC().Format("20060102-150405")
}

func deploymentID(version string) string {
	return fmt.Sprintf("%d-%s", time.Now().UTC().Unix(), version)
}
