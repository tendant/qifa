package deploy

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/gokamal/gocart/internal/docker"
)

// Drift is one reason the host does not match what the config declares.
type Drift struct {
	Role   string
	Host   string
	Reason string
}

// Reconcile compares what is declared against what is running and converges
// only when they differ.
//
// This is what `qifa deploy` cannot be: deploy always replaces the container,
// so running it on a timer would restart every app on every tick. Reconcile
// answers "is this host already what the config says" first, and does nothing
// when the answer is yes — which is what makes it safe to run unattended, on
// a schedule, from a checkout of the repo.
//
// It reports drift rather than guessing at its cause: a missing container, a
// stopped one, an older version still running, or a proxy still pointing at a
// predecessor all look identical from the outside (the site is down, or serving
// something old) and need naming individually.
func (d *Deployer) Reconcile(ctx context.Context, out io.Writer, dryRun bool) error {
	imageRef, version, err := d.resolveImage(ctx)
	if err != nil {
		return err
	}

	drifts, err := d.findDrift(ctx, version)
	if err != nil {
		return err
	}

	if len(drifts) == 0 {
		fmt.Fprintf(out, "in sync: %s at %s\n", d.cfg.Service, imageRef)
		return nil
	}

	for _, drift := range drifts {
		fmt.Fprintf(out, "drift: %s on %s — %s\n", drift.Role, drift.Host, drift.Reason)
	}
	if dryRun {
		fmt.Fprintf(out, "%d difference(s); not converging (--dry-run)\n", len(drifts))
		return nil
	}
	fmt.Fprintf(out, "converging %s to %s\n", d.cfg.Service, imageRef)
	return d.Deploy(ctx)
}

// findDrift returns every way the hosts differ from the declared state. An
// empty result means a deploy would change nothing.
func (d *Deployer) findDrift(ctx context.Context, version string) ([]Drift, error) {
	var drifts []Drift
	for _, role := range orderedRoles(d.cfg.Servers) {
		server := d.cfg.Servers[role]
		wanted := d.containerName(role, version)
		useProxy := serverUsesProxy(role, server)

		for _, host := range server.Hosts {
			state, err := d.remoteDocker.ContainerState(ctx, host, wanted)
			switch {
			case err != nil:
				// Distinguish "no such container" from an unreachable host:
				// converging cannot fix the latter, and trying would bury the
				// real error under a deploy failure.
				running, listErr := d.remoteDocker.ListContainersByService(ctx, host, d.cfg.Service, role)
				if listErr != nil {
					return nil, fmt.Errorf("inspect %s on %s: %w", wanted, host, listErr)
				}
				drifts = append(drifts, Drift{Role: role, Host: host, Reason: describeMissing(wanted, running)})
				continue
			case !strings.HasPrefix(strings.TrimSpace(state), "running"):
				drifts = append(drifts, Drift{Role: role, Host: host,
					Reason: fmt.Sprintf("container %s is %s", wanted, strings.TrimSpace(state))})
				continue
			}

			if !useProxy {
				continue
			}
			// A healthy container the proxy does not point at serves nobody.
			registration, ok, err := d.proxy.Registered(ctx, host, d.cfg.Service)
			if err != nil {
				return nil, err
			}
			switch {
			case !ok:
				drifts = append(drifts, Drift{Role: role, Host: host,
					Reason: fmt.Sprintf("proxy has no route for %s", d.cfg.Service)})
			case !strings.HasPrefix(registration.Target, wanted+":"):
				drifts = append(drifts, Drift{Role: role, Host: host,
					Reason: fmt.Sprintf("proxy points at %s, wanted %s", registration.Target, wanted)})
			}
		}
	}
	return drifts, nil
}

// describeMissing says what is there instead, which is the difference between
// "never deployed" and "an older version is still serving".
func describeMissing(wanted string, running []docker.ContainerInfo) string {
	for _, c := range running {
		if strings.HasPrefix(c.State, "running") {
			return fmt.Sprintf("%s is running, wanted %s", c.Name, wanted)
		}
	}
	return fmt.Sprintf("no container %s", wanted)
}
