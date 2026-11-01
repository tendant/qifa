package native

import (
	"context"
	"fmt"
	"strings"

	"github.com/tendant/qifa/internal/fleet"
	"github.com/tendant/qifa/internal/state"
)

// A config with no native.artifact runs a program already installed on the
// server (servers.<role>.cmd is an absolute path). qifa owns its unit, env,
// restarts, health check, logs and route, but ships no release: a deploy
// installs the env file and restarts, and there is nothing to roll back to.

// installedVersion names a deploy of an installed program when --version
// does not.
const installedVersion = "installed"

// installedExecs lists the programs the roles run, once each.
func (d *Deployer) installedExecs() []string {
	seen := map[string]bool{}
	var out []string
	for _, role := range fleet.OrderedRoles(d.cfg.Servers) {
		if bin := d.cfg.NativeExec(role)[0]; !seen[bin] {
			seen[bin] = true
			out = append(out, bin)
		}
	}
	return out
}

// workdir is where the app runs: native.workdir, or the live release.
func (d *Deployer) workdir() string {
	return unitParams(d.cfg, fleet.OrderedRoles(d.cfg.Servers)[0]).WorkingDir()
}

// deployInstalledHost installs the env and restarts the units. If they do
// not come back healthy, the previous env is restored and they are restarted
// again; on a first deploy, they are stopped.
func (d *Deployer) deployInstalledHost(ctx context.Context, deployment state.Deployment, host, id string, envFile []byte) error {
	dir := d.cfg.Native.Dir
	event := func(kind, msg string) { _ = d.audit().Event(deployment.ID, host, "", kind, msg) }
	event("connecting", "")
	if err := d.ssh.Upload(ctx, host, dir+"/etc/env.next", envFile, 0o600); err != nil {
		return fmt.Errorf("%s: upload env: %w", host, err)
	}
	out, err := d.run(ctx, host, installEnvScript(dir, id))
	if err != nil {
		return fmt.Errorf("%s: install env: %w", host, err)
	}
	hadEnv := strings.TrimSpace(out) == "prev"
	event("activated", id)

	if err := d.restartAndCheck(ctx, host); err != nil {
		event("failed", err.Error())
		if !hadEnv {
			for _, unit := range d.unitsOn(host) {
				_ = d.control(ctx, host, "stop", unit)
			}
			return fmt.Errorf("%w\nfirst deploy on %s failed (units stopped)", err, host)
		}
		d.log.Printf("%s: restoring the previous env", host)
		if _, rerr := d.run(ctx, host, revertEnvScript(dir)); rerr != nil {
			return fmt.Errorf("%w\nROLLBACK FAILED on %s: could not restore the env: %v", err, host, rerr)
		}
		if rerr := d.restartAndCheck(ctx, host); rerr != nil {
			return fmt.Errorf("%w\nROLLBACK FAILED on %s: did not come back with the previous env: %v", err, host, rerr)
		}
		return fmt.Errorf("%w\nrestored the previous env on %s (the program itself is whatever is installed)", err, host)
	}
	if _, err := d.run(ctx, host, "rm -f "+q(dir+"/etc/env.prev")); err != nil {
		d.log.Printf("warning: %s: %v", host, err)
	}
	if d.routedOn(host) {
		if err := d.registerRoute(ctx, host); err != nil {
			event("failed", err.Error())
			return err
		}
	}
	event("deployed", id)
	d.log.Printf("%s: %s is live", host, strings.Join(d.installedExecs(), ", "))
	return nil
}
