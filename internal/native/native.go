// Package native deploys an app as a systemd service running a release
// directory, instead of as a container. See
// docs/qifa-native-deployment-design.md.
//
// The host filesystem is the source of truth, as Docker labels are in Docker
// mode: `current` names the live release, and a release containing .qifa-ok
// passed its health check. The deployer keeps nothing but the audit log.
package native

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tendant/qifa/internal/config"
	"github.com/tendant/qifa/internal/hooks"
	"github.com/tendant/qifa/internal/lock"
	"github.com/tendant/qifa/internal/logs"
	"github.com/tendant/qifa/internal/secrets"
	"github.com/tendant/qifa/internal/ssh"
	"github.com/tendant/qifa/internal/state"
	"github.com/tendant/qifa/internal/version"
)

type Deployer struct {
	cfg    *config.Config
	store  *state.Store
	ssh    *ssh.Client
	log    *logs.Logger
	stdout io.Writer
	stderr io.Writer

	version    string
	allowDirty bool
}

func New(cfg *config.Config, store *state.Store, stdout, stderr io.Writer) (*Deployer, error) {
	if cfg.Native == nil {
		return nil, errors.New("native deployer needs config.native")
	}
	return &Deployer{
		cfg:    cfg,
		store:  store,
		ssh:    ssh.New(cfg.SSH),
		log:    logs.New(stdout),
		stdout: stdout,
		stderr: stderr,
	}, nil
}

// UseSSHUser switches the SSH account, for `native setup --ssh-user`: setup
// needs root, deploys must not.
func (d *Deployer) UseSSHUser(user string) {
	sshCfg := d.cfg.SSH
	sshCfg.User = user
	d.ssh = ssh.New(sshCfg)
}

func (d *Deployer) PinVersion(v string, allowDirty bool) {
	d.version = strings.TrimSpace(v)
	d.allowDirty = allowDirty
}

var releaseIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func (d *Deployer) resolveVersion() (string, error) {
	v, err := version.Resolve(d.version, d.allowDirty)
	if err != nil {
		return "", err
	}
	if !releaseIDRe.MatchString(v) || strings.HasSuffix(v, ".tmp") {
		return "", fmt.Errorf("version %q cannot name a release directory: use letters, digits, '.', '_' and '-'", v)
	}
	return v, nil
}

// hosts lists each host once, web roles first, as the Docker path orders them.
func (d *Deployer) hosts() []string {
	seen := map[string]bool{}
	var out []string
	for _, role := range orderedRoles(d.cfg.Servers) {
		for _, h := range d.cfg.Servers[role].Hosts {
			if !seen[h] {
				seen[h] = true
				out = append(out, h)
			}
		}
	}
	return out
}

func (d *Deployer) rolesOn(host string) []string {
	var roles []string
	for _, role := range orderedRoles(d.cfg.Servers) {
		for _, h := range d.cfg.Servers[role].Hosts {
			if h == host {
				roles = append(roles, role)
				break
			}
		}
	}
	return roles
}

func (d *Deployer) unitsOn(host string) []string {
	var units []string
	for _, role := range d.rolesOn(host) {
		units = append(units, d.cfg.NativeUnitName(role))
	}
	return units
}

// healthURL is the HTTP check for a role. Only the web role (or the only
// role) serves HTTP on native.port; other roles are checked for liveness.
func (d *Deployer) healthURL(role string) string {
	n := d.cfg.Native
	if n.Healthcheck.Path == "" || (role != "web" && len(d.cfg.Servers) > 1) {
		return ""
	}
	port := n.Port
	if n.Healthcheck.Port != 0 {
		port = n.Healthcheck.Port
	}
	return fmt.Sprintf("http://127.0.0.1:%d%s", port, n.Healthcheck.Path)
}

// Deploy builds, ships and activates a release on every host. On a host whose
// new release fails to start or to pass its health check, the previous release
// is restored and restarted before the error is returned.
func (d *Deployer) Deploy(ctx context.Context) error {
	n := d.cfg.Native
	v, err := d.resolveVersion()
	if err != nil {
		return err
	}
	hookEnv := map[string]string{"QIFA_VERSION": v}
	if err := hooks.Run(ctx, d.cfg.Hooks.PreBuild, hookEnv); err != nil {
		return err
	}
	if strings.TrimSpace(n.Build) != "" {
		d.log.Printf("building: %s", n.Build)
		cmd := exec.CommandContext(ctx, "/bin/sh", "-c", n.Build)
		cmd.Stdout, cmd.Stderr = d.stdout, d.stderr
		cmd.Env = append(os.Environ(), "QIFA_VERSION="+v)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("native.build failed: %w", err)
		}
	}
	pkg, err := Pack(n.Artifact)
	if err != nil {
		return err
	}
	artifactArch, err := d.artifactArch(pkg)
	if err != nil {
		return err
	}
	d.log.Printf("release %s: %s (%d KiB, sha256 %s)", v, n.Artifact, len(pkg.Data)/1024, pkg.SHA256[:12])

	values, err := secrets.Collect(d.cfg.Env.Clear, d.cfg.Env.Secret, d.cfg.Env.SecretCommand)
	if err != nil {
		return err
	}
	envFile, err := RenderEnv(values)
	if err != nil {
		return err
	}

	hosts := d.hosts()
	d.log.Printf("acquiring lock on %v", hosts)
	locker := lock.New(d.ssh, d.cfg.Service, hosts)
	if err := locker.Acquire(ctx, v); err != nil {
		return err
	}
	defer func() {
		if err := locker.Release(context.Background()); err != nil {
			d.log.Printf("warning: %v", err)
		}
	}()

	deployment := state.Deployment{
		ID:        fmt.Sprintf("%d-%s", time.Now().UTC().Unix(), v),
		Service:   d.cfg.Service,
		Version:   v,
		Image:     "native:" + n.Artifact + "@sha256:" + pkg.SHA256[:12],
		Status:    state.StatusPending,
		StartedAt: time.Now().UTC(),
	}
	if err := d.store.AppendDeployment(deployment); err != nil {
		return err
	}

	// Every host is checked before any is changed: a missing sudo rule or a
	// stale unit on the third host must not leave the first two upgraded.
	if err := d.preflight(ctx, hosts, artifactArch); err != nil {
		return d.fail(deployment, err)
	}
	if err := d.syncFiles(ctx, hosts); err != nil {
		return d.fail(deployment, err)
	}
	if err := d.updateStatus(deployment, state.StatusStarting); err != nil {
		return err
	}
	if err := d.rollout(ctx, deployment, hosts, func(ctx context.Context, host string) error {
		return d.deployHost(ctx, deployment, host, v, pkg, envFile)
	}); err != nil {
		return d.fail(deployment, err)
	}
	if err := d.updateStatus(deployment, state.StatusSucceeded); err != nil {
		return err
	}
	if err := hooks.Run(ctx, d.cfg.Hooks.PostDeploy, hookEnv); err != nil {
		return err
	}
	d.log.Printf("deployment %s succeeded", deployment.ID)
	return nil
}

// artifactArch returns the architecture of the release's executables, checked
// against native.arch. "" means none of them is ELF (scripts, a JVM jar).
func (d *Deployer) artifactArch(pkg *Package) (string, error) {
	want := d.cfg.Native.Arch
	found := ""
	for _, role := range orderedRoles(d.cfg.Servers) {
		local := ExecPathInArtifact(d.cfg.Native.Artifact, pkg.IsDir, d.cfg.NativeExec(role)[0])
		if local == "" {
			continue
		}
		arch, err := ELFArch(local)
		if err != nil {
			return "", fmt.Errorf("native.exec for %s: %w", role, err)
		}
		if arch == "" {
			continue
		}
		if want != "" && arch != want {
			return "", fmt.Errorf("%s is built for %s but native.arch is %s (cross-compile, e.g. GOOS=linux GOARCH=%s)", local, arch, want, want)
		}
		found = arch
	}
	return found, nil
}

func (d *Deployer) preflight(ctx context.Context, hosts []string, artifactArch string) error {
	var problems []string
	for _, host := range hosts {
		p, warnings, err := d.checkHost(ctx, host, artifactArch)
		if err != nil {
			return fmt.Errorf("preflight on %s: %w", host, err)
		}
		for _, w := range warnings {
			d.log.Printf("warning: %s: %s", host, w)
		}
		for _, line := range p {
			problems = append(problems, host+": "+line)
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("hosts are not ready; nothing was changed:\n  %s", strings.Join(problems, "\n  "))
	}
	return nil
}

// checkHost runs the read-only preflight on one host. Problems block a
// deploy; warnings describe a state worth knowing about (an interrupted
// earlier deploy) that the deploy itself will repair.
func (d *Deployer) checkHost(ctx context.Context, host, artifactArch string) (problems, warnings []string, err error) {
	out, err := d.ssh.Run(ctx, host, preflightScript(d.cfg.Native.Dir, d.unitsOn(host)))
	if err != nil {
		return nil, nil, err
	}
	kv := parseKV(out)
	if kv["systemd"] != "yes" {
		return []string{"systemd is not available"}, nil, nil
	}
	if kv["bus"] != "ok" {
		problems = append(problems, fmt.Sprintf("systemctl cannot reach systemd as %s (no D-Bus system bus?) — install dbus", d.sshUser()))
	}
	if kv["dir"] != "ok" {
		problems = append(problems, fmt.Sprintf("%s is missing or not writable by %s — run `qifa native setup`", d.cfg.Native.Dir, d.sshUser()))
	}
	hostArch := UnameArch(kv["arch"])
	if want := firstNonEmpty(d.cfg.Native.Arch, artifactArch); want != "" && hostArch != want {
		problems = append(problems, fmt.Sprintf("host is %s but the release is built for %s", hostArch, want))
	}
	for _, role := range d.rolesOn(host) {
		unit := d.cfg.NativeUnitName(role)
		if kv["sudo:"+unit] != "ok" {
			problems = append(problems, fmt.Sprintf("%s may not run `sudo -n systemctl restart %s` — run `qifa native setup`", d.sshUser(), unit))
		}
		switch sum := kv["unitsum:"+unit]; sum {
		case "":
			problems = append(problems, fmt.Sprintf("unit %s is not installed — run `qifa native setup`", unit))
		case sha256Hex(RenderUnit(unitParams(d.cfg, role))):
		default:
			problems = append(problems, fmt.Sprintf("unit %s differs from the config (exec, user or restart changed) — run `qifa native setup`", unit))
		}
	}
	if kv["currentok"] == "no" {
		warnings = append(warnings, fmt.Sprintf("current release %s never passed its health check (an interrupted deploy?)", strings.TrimPrefix(kv["current"], "releases/")))
	}
	if kv["tmp"] == "yes" {
		warnings = append(warnings, "a half-staged release was left behind; it is removed on the next deploy")
	}
	return problems, warnings, nil
}

func (d *Deployer) sshUser() string {
	if d.cfg.SSH.User != "" {
		return d.cfg.SSH.User
	}
	return "the ssh user"
}

// rollout runs fn over hosts in rollout.batch_size batches, as the Docker path
// does: hosts in a batch in parallel, batches in order, stopping at the first
// failed batch.
func (d *Deployer) rollout(ctx context.Context, deployment state.Deployment, hosts []string, fn func(context.Context, string) error) error {
	size := 1
	if d.cfg.Rollout.BatchSize != nil {
		size = *d.cfg.Rollout.BatchSize
	}
	if size <= 0 || size > len(hosts) {
		size = len(hosts)
	}
	for i := 0; i < len(hosts); i += size {
		batch := hosts[i:min(i+size, len(hosts))]
		if i > 0 && d.cfg.Rollout.BatchWait > 0 {
			select {
			case <-time.After(d.cfg.Rollout.BatchWait):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		var wg sync.WaitGroup
		errs := make([]error, len(batch))
		for j, host := range batch {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[j] = fn(ctx, host)
			}()
		}
		wg.Wait()
		if err := errors.Join(errs...); err != nil {
			if rest := len(hosts) - (i + len(batch)); rest > 0 {
				d.log.Printf("skipping %d remaining host(s)", rest)
			}
			return err
		}
	}
	return nil
}

func (d *Deployer) deployHost(ctx context.Context, deployment state.Deployment, host, id string, pkg *Package, envFile []byte) error {
	n := d.cfg.Native
	event := func(kind, msg string) { _ = d.appendEvent(deployment.ID, host, kind, msg) }
	event("connecting", "")

	d.log.Printf("%s: uploading release %s", host, id)
	if err := d.ssh.Upload(ctx, host, uploadPath(n.Dir, id), pkg.Data, 0o600); err != nil {
		return fmt.Errorf("%s: upload: %w", host, err)
	}
	var execs []string
	for _, role := range d.rolesOn(host) {
		execs = append(execs, d.cfg.NativeExec(role)[0])
	}
	out, err := d.ssh.Run(ctx, host, stageScript(n.Dir, id, pkg.SHA256, n.SharedDirs, execs))
	if err != nil {
		return fmt.Errorf("%s: stage release %s: %w", host, id, err)
	}
	if strings.Contains(out, "reused") {
		d.log.Printf("%s: release %s already present with the same contents", host, id)
	}
	if err := d.ssh.Upload(ctx, host, n.Dir+"/etc/env.next", envFile, 0o600); err != nil {
		return fmt.Errorf("%s: upload env: %w", host, err)
	}

	// Past this point the live release changes; any failure restores it.
	prev, err := d.ssh.Run(ctx, host, activateScript(n.Dir, id))
	if err != nil {
		return fmt.Errorf("%s: activate %s: %w", host, id, err)
	}
	prev = strings.TrimSpace(prev)
	d.log.Printf("%s: current -> releases/%s (was %s)", host, id, firstNonEmpty(prev, "none"))
	event("activated", id)

	if err := d.restartAndCheck(ctx, host); err != nil {
		event("failed", err.Error())
		return d.recover(ctx, host, prev, err)
	}
	if _, err := d.ssh.Run(ctx, host, markOKScript(n.Dir, id)); err != nil {
		return fmt.Errorf("%s: mark release %s healthy: %w", host, id, err)
	}
	event("deployed", id)
	d.log.Printf("%s: release %s is live", host, id)
	if err := d.prune(ctx, host); err != nil {
		d.log.Printf("warning: %s: pruning old releases: %v", host, err)
	}
	return nil
}

// restartAndCheck restarts every unit on host and waits for each to pass.
func (d *Deployer) restartAndCheck(ctx context.Context, host string) error {
	hc := d.cfg.Native.Healthcheck
	for _, role := range d.rolesOn(host) {
		unit := d.cfg.NativeUnitName(role)
		d.log.Printf("%s: restarting %s", host, unit)
		if _, err := d.ssh.Sudo(ctx, host, "systemctl", "restart", unit); err != nil {
			return d.withDiagnostics(ctx, host, unit, fmt.Errorf("%s: restart %s: %w", host, unit, err))
		}
	}
	for _, role := range d.rolesOn(host) {
		unit := d.cfg.NativeUnitName(role)
		url := d.healthURL(role)
		what := "active"
		if url != "" {
			what = url
		}
		d.log.Printf("%s: waiting for %s (%s)", host, unit, what)
		script := healthScript(unit, url, int(hc.Timeout.Seconds()), int(hc.Interval.Seconds()))
		if _, err := d.ssh.Run(ctx, host, script); err != nil {
			return d.withDiagnostics(ctx, host, unit, fmt.Errorf("%s: %s did not become healthy within %s: %w", host, unit, hc.Timeout, err))
		}
	}
	return nil
}

// recover puts the previous release back after a failed activation. With no
// previous release (a first deploy), the units are stopped so a crashing app
// is not restarted forever, and the failed release is kept for inspection.
func (d *Deployer) recover(ctx context.Context, host, prev string, cause error) error {
	if prev == "" {
		for _, unit := range d.unitsOn(host) {
			_, _ = d.ssh.Sudo(ctx, host, "systemctl", "stop", unit)
		}
		return fmt.Errorf("%w\nfirst deploy on %s failed; there is no previous release to restore (units stopped)", cause, host)
	}
	d.log.Printf("%s: rolling back to %s", host, prev)
	if _, err := d.ssh.Run(ctx, host, revertScript(d.cfg.Native.Dir, prev)); err != nil {
		return fmt.Errorf("%w\nROLLBACK FAILED on %s: could not restore %s: %v", cause, host, prev, err)
	}
	if err := d.restartAndCheck(ctx, host); err != nil {
		return fmt.Errorf("%w\nROLLBACK FAILED on %s: %s did not come back: %v", cause, host, prev, err)
	}
	d.log.Printf("%s: restored %s", host, prev)
	return fmt.Errorf("%w\nrestored %s on %s", cause, prev, host)
}

func (d *Deployer) withDiagnostics(ctx context.Context, host, unit string, cause error) error {
	out, _ := d.ssh.Run(ctx, host, diagnosticsScript(unit))
	if strings.TrimSpace(out) == "" {
		return cause
	}
	return fmt.Errorf("%w\n%s", cause, out)
}

func (d *Deployer) syncFiles(ctx context.Context, hosts []string) error {
	for _, f := range d.cfg.Files {
		data, err := os.ReadFile(f.Src)
		if err != nil {
			return fmt.Errorf("read %s: %w", f.Src, err)
		}
		mode := f.Mode
		if mode == 0 {
			mode = 0o644
		}
		for _, host := range hosts {
			d.log.Printf("sync: %s -> %s:%s (%d bytes)", f.Src, host, f.Dest, len(data))
			if err := d.ssh.Upload(ctx, host, f.Dest, data, os.FileMode(mode)); err != nil {
				return fmt.Errorf("upload %s to %s:%s: %w", f.Src, host, f.Dest, err)
			}
		}
	}
	return nil
}

// Plan prints what Deploy would do, without building or connecting.
func (d *Deployer) Plan(ctx context.Context, out io.Writer) error {
	n := d.cfg.Native
	v, err := d.resolveVersion()
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Plan for service=%s (native)\n", d.cfg.Service)
	fmt.Fprintf(out, "  Release:  %s\n", v)
	fmt.Fprintf(out, "  Build:    %s\n", firstNonEmpty(n.Build, "(none; artifact must already exist)"))
	fmt.Fprintf(out, "  Artifact: %s\n", n.Artifact)
	fmt.Fprintf(out, "  Install:  %s/releases/%s, current -> it\n", n.Dir, v)
	for _, role := range orderedRoles(d.cfg.Servers) {
		check := firstNonEmpty(d.healthURL(role), "unit active")
		fmt.Fprintf(out, "  Role %s: %s on %v\n", role, d.cfg.NativeUnitName(role), d.cfg.Servers[role].Hosts)
		fmt.Fprintf(out, "    ExecStart=%s\n", ExecStart(n.Dir, d.cfg.NativeExec(role)))
		fmt.Fprintf(out, "    health:  %s (timeout %s)\n", check, n.Healthcheck.Timeout)
	}
	fmt.Fprintln(out, "  On failure: restore the previous release and restart")
	return nil
}

func (d *Deployer) Stop(ctx context.Context) error    { return d.eachUnit(ctx, "stop") }
func (d *Deployer) Start(ctx context.Context) error   { return d.eachUnit(ctx, "start") }
func (d *Deployer) Restart(ctx context.Context) error { return d.eachUnit(ctx, "restart") }

func (d *Deployer) eachUnit(ctx context.Context, verb string) error {
	for _, host := range d.hosts() {
		for _, unit := range d.unitsOn(host) {
			d.log.Printf("%s: systemctl %s %s", host, verb, unit)
			if _, err := d.ssh.Sudo(ctx, host, "systemctl", verb, unit); err != nil {
				return d.withDiagnostics(ctx, host, unit, err)
			}
		}
	}
	return nil
}

// Logs shows the journal of the first host's units.
func (d *Deployer) Logs(ctx context.Context, lines int, follow bool, out io.Writer) error {
	hosts := d.hosts()
	cmd := []string{"journalctl", "--no-pager", "-n", fmt.Sprint(lines)}
	for _, unit := range d.unitsOn(hosts[0]) {
		cmd = append(cmd, "-u", unit)
	}
	if follow {
		cmd = append(cmd, "-f")
	}
	quoted := make([]string, len(cmd))
	for i, c := range cmd {
		quoted[i] = q(c)
	}
	return d.ssh.Stream(ctx, hosts[0], strings.Join(quoted, " "), out)
}

func (d *Deployer) LockStatus(ctx context.Context, out io.Writer) error {
	status, err := lock.New(d.ssh, d.cfg.Service, d.hosts()).Status(ctx)
	if err != nil {
		return err
	}
	for _, host := range d.hosts() {
		fmt.Fprintf(out, "%s\t%s\n", host, firstNonEmpty(status[host], "(free)"))
	}
	return nil
}

func (d *Deployer) LockRelease(ctx context.Context) error {
	return lock.New(d.ssh, d.cfg.Service, d.hosts()).ForceRelease(ctx)
}

func (d *Deployer) fail(deployment state.Deployment, cause error) error {
	_ = d.updateStatus(deployment, state.StatusFailed)
	return cause
}

func (d *Deployer) updateStatus(deployment state.Deployment, next state.Status) error {
	deployment.Status = next
	if next == state.StatusSucceeded || next == state.StatusFailed || next == state.StatusRolledBack {
		now := time.Now().UTC()
		deployment.FinishedAt = &now
	}
	return d.store.AppendDeployment(deployment)
}

func (d *Deployer) appendEvent(deploymentID, host, eventType, message string) error {
	return d.store.AppendEvent(state.Event{
		ID:           fmt.Sprintf("%d", time.Now().UnixNano()),
		DeploymentID: deploymentID,
		Host:         host,
		EventType:    eventType,
		Message:      message,
		CreatedAt:    time.Now().UTC(),
	})
}

func orderedRoles(servers map[string]config.Server) []string {
	roles := make([]string, 0, len(servers))
	for role := range servers {
		if role != "web" {
			roles = append(roles, role)
		}
	}
	sort.Strings(roles)
	if _, ok := servers["web"]; ok {
		roles = append([]string{"web"}, roles...)
	}
	return roles
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
