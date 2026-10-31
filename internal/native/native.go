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
	"strings"
	"sync"
	"time"

	"github.com/tendant/qifa/internal/config"
	"github.com/tendant/qifa/internal/fleet"
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

// hosts lists each host once, web roles first.
func (d *Deployer) hosts() []string { return fleet.Hosts(d.cfg.Servers) }

func (d *Deployer) rolesOn(host string) []string { return fleet.RolesOn(d.cfg.Servers, host) }

func (d *Deployer) audit() fleet.Audit { return fleet.Audit{Store: d.store} }

func (d *Deployer) unitsOn(host string) []string {
	var units []string
	for _, role := range d.rolesOn(host) {
		units = append(units, d.cfg.NativeUnitName(role))
	}
	return units
}

// healthURL is the HTTP check for a role: any role with an app_port, when a
// healthcheck.path is set. Other roles are checked for staying active.
func (d *Deployer) healthURL(role string) string {
	hc := d.cfg.Health()
	port := d.cfg.AppPort(role)
	if hc.Path == "" || port == 0 {
		return ""
	}
	if hc.Port != 0 {
		port = hc.Port
	}
	return fmt.Sprintf("http://127.0.0.1:%d%s", port, hc.Path)
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
	defer pkg.Remove()
	artifactArch, err := d.artifactArch(pkg)
	if err != nil {
		return err
	}
	d.log.Printf("release %s: %s (%d KiB, sha256 %s)", v, n.Artifact, pkg.Size/1024, pkg.SHA256[:12])

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
		return d.audit().Fail(deployment, err)
	}
	if err := d.syncFiles(ctx, hosts); err != nil {
		return d.audit().Fail(deployment, err)
	}
	if err := d.audit().Status(deployment, state.StatusStarting); err != nil {
		return err
	}
	if err := fleet.Batches(ctx, hosts, d.cfg.Rollout, d.log.Printf, func(ctx context.Context, host string) error {
		return d.deployHost(ctx, deployment, host, v, pkg, envFile)
	}); err != nil {
		return d.audit().Fail(deployment, err)
	}
	if err := d.audit().Status(deployment, state.StatusSucceeded); err != nil {
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
	for _, role := range fleet.OrderedRoles(d.cfg.Servers) {
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
	routed := d.routedOn(host)
	out, err := d.ssh.Run(ctx, host, preflightScript(d.cfg.Native.Dir, d.unitsOn(host), routed))
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
		problems = append(problems, fmt.Sprintf("%s is missing or not writable by %s — run `qifa setup --ssh-user root`", d.cfg.Native.Dir, d.sshUser()))
	}
	hostArch := UnameArch(kv["arch"])
	if want := firstNonEmpty(d.cfg.Native.Arch, artifactArch); want != "" && hostArch != want {
		problems = append(problems, fmt.Sprintf("host is %s but the release is built for %s", hostArch, want))
	}
	for _, role := range d.rolesOn(host) {
		unit := d.cfg.NativeUnitName(role)
		if kv["sudo:"+unit] != "ok" {
			problems = append(problems, fmt.Sprintf("%s may not run `sudo -n systemctl restart %s` — run `qifa setup --ssh-user root`", d.sshUser(), unit))
		}
		switch sum := kv["unitsum:"+unit]; sum {
		case "":
			problems = append(problems, fmt.Sprintf("unit %s is not installed — run `qifa setup --ssh-user root`", unit))
		case sha256Hex(RenderUnit(unitParams(d.cfg, role))):
		default:
			problems = append(problems, fmt.Sprintf("unit %s differs from the config (exec, user or restart changed) — run `qifa setup --ssh-user root`", unit))
		}
	}
	if routed && kv["proxy"] != "ok" {
		problems = append(problems, fmt.Sprintf("kamal-proxy is not running here, or %s cannot reach its socket — run `qifa proxy boot --ssh-user root`", d.sshUser()))
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

func (d *Deployer) deployHost(ctx context.Context, deployment state.Deployment, host, id string, pkg *Package, envFile []byte) error {
	n := d.cfg.Native
	event := func(kind, msg string) { _ = d.audit().Event(deployment.ID, host, "", kind, msg) }
	event("connecting", "")

	d.log.Printf("%s: uploading release %s", host, id)
	archive, err := pkg.Open()
	if err != nil {
		return err
	}
	err = d.ssh.UploadFrom(ctx, host, uploadPath(n.Dir, id), archive, 0o600)
	archive.Close()
	if err != nil {
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
	if d.routedOn(host) {
		if err := d.registerRoute(ctx, host); err != nil {
			event("failed", err.Error())
			return err
		}
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
	hc := d.cfg.Health()
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
		script := healthScript(unit, url, int(hc.DeployTimeout.Seconds()), int(hc.Interval.Seconds()), int(hc.Timeout.Seconds()))
		if _, err := d.ssh.Run(ctx, host, script); err != nil {
			return d.withDiagnostics(ctx, host, unit, fmt.Errorf("%s: %s did not become healthy within %s: %w", host, unit, hc.DeployTimeout, err))
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
	for _, role := range fleet.OrderedRoles(d.cfg.Servers) {
		check := firstNonEmpty(d.healthURL(role), "unit active")
		fmt.Fprintf(out, "  Role %s: %s on %v\n", role, d.cfg.NativeUnitName(role), d.cfg.Servers[role].Hosts)
		fmt.Fprintf(out, "    ExecStart=%s\n", ExecStart(n.Dir, d.cfg.NativeExec(role)))
		fmt.Fprintf(out, "    health:  %s (within %s)\n", check, d.cfg.Health().DeployTimeout)
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

// Logs shows the journal of the service's units on every host. With more
// than one host each line is prefixed with its host; following streams all
// hosts at once.
func (d *Deployer) Logs(ctx context.Context, lines int, follow bool, out io.Writer) error {
	hosts := d.hosts()
	command := func(host string) string {
		cmd := []string{"journalctl", "--no-pager", "-n", fmt.Sprint(lines)}
		for _, unit := range d.unitsOn(host) {
			cmd = append(cmd, "-u", unit)
		}
		if follow {
			cmd = append(cmd, "-f")
		}
		quoted := make([]string, len(cmd))
		for i, c := range cmd {
			quoted[i] = q(c)
		}
		return strings.Join(quoted, " ")
	}
	if len(hosts) == 1 {
		return d.ssh.Stream(ctx, hosts[0], command(hosts[0]), out)
	}
	var mu sync.Mutex
	stream := func(host string) error {
		w := &fleet.PrefixWriter{Prefix: host + " | ", Out: out, Mu: &mu}
		err := d.ssh.Stream(ctx, host, command(host), w)
		w.Flush()
		if err != nil {
			return fmt.Errorf("%s: %w", host, err)
		}
		return nil
	}
	if !follow {
		for _, host := range hosts {
			if err := stream(host); err != nil {
				return err
			}
		}
		return nil
	}
	errs := make([]error, len(hosts))
	var wg sync.WaitGroup
	for i, host := range hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = stream(host)
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// Exec runs command on the first host of the first role, as the service
// account, in the live release, with the app's environment: what the app
// itself would see. The env file is valid shell (see RenderEnv), so the
// deploy user loads it and sudo -E passes it on, and no value ever appears
// on a command line. Needs the SETENV rule `qifa setup` installs.
func (d *Deployer) Exec(ctx context.Context, command string, out io.Writer) error {
	host := d.cfg.Servers[fleet.OrderedRoles(d.cfg.Servers)[0]].Hosts[0]
	return d.ssh.Stream(ctx, host, execScript(d.cfg.Native.Dir, d.cfg.Native.User, command), out)
}

func (d *Deployer) LockStatus(ctx context.Context, out io.Writer) error {
	status, err := lock.New(d.ssh, d.cfg.Service, d.hosts()).Status(ctx)
	if err != nil {
		return err
	}
	fleet.WriteLockStatus(out, d.hosts(), status)
	return nil
}

func (d *Deployer) LockRelease(ctx context.Context) error {
	return lock.New(d.ssh, d.cfg.Service, d.hosts()).ForceRelease(ctx)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
