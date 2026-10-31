package native

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tendant/qifa/internal/hooks"
	"github.com/tendant/qifa/internal/lock"
	"github.com/tendant/qifa/internal/proxy"
	"github.com/tendant/qifa/internal/secrets"
	"github.com/tendant/qifa/internal/state"
)

type release struct {
	ID      string
	Staged  int64 // when it was staged
	Healthy int64 // last time it passed a health check; 0 = never
}

// inventory is what a host says about this service. It is the native
// equivalent of listing labelled containers.
type inventory struct {
	Current  string // release ID, "" if none
	Releases []release
	Tmp      []string
	Units    map[string]string // unit -> systemctl is-active
	Health   string            // up / down / "" when not checked
}

func (inv inventory) find(id string) (release, bool) {
	for _, r := range inv.Releases {
		if r.ID == id {
			return r, true
		}
	}
	return release{}, false
}

func parseInventory(out string) inventory {
	inv := inventory{Units: map[string]string{}}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "current":
			inv.Current = strings.TrimPrefix(v, "releases/")
		case "release":
			f := strings.Fields(v)
			if len(f) != 3 {
				continue
			}
			st, _ := strconv.ParseInt(f[1], 10, 64)
			ok, _ := strconv.ParseInt(f[2], 10, 64)
			inv.Releases = append(inv.Releases, release{ID: f[0], Staged: st, Healthy: ok})
		case "tmp":
			inv.Tmp = append(inv.Tmp, v)
		case "unit":
			name, st, _ := strings.Cut(v, " ")
			inv.Units[name] = firstNonEmpty(strings.TrimSpace(st), "unknown")
		case "health":
			inv.Health = v
		}
	}
	// Newest first.
	sort.SliceStable(inv.Releases, func(i, j int) bool { return inv.Releases[i].Staged > inv.Releases[j].Staged })
	return inv
}

func (d *Deployer) inventory(ctx context.Context, host string, probe bool) (inventory, error) {
	url := ""
	if probe {
		for _, role := range d.rolesOn(host) {
			if u := d.healthURL(role); u != "" {
				url = u
			}
		}
	}
	out, err := d.ssh.Run(ctx, host, inventoryScript(d.cfg.Native.Dir, d.unitsOn(host), url))
	if err != nil {
		return inventory{}, err
	}
	return parseInventory(out), nil
}

// pruneSet picks releases to delete: everything beyond the newest `retain`,
// except the live release and the newest healthy one that is not live — the
// automatic rollback target is never pruned, however old it is.
func pruneSet(inv inventory, retain int) []string {
	keep := map[string]bool{inv.Current: true}
	if t := rollbackTarget(inv); t != "" {
		keep[t] = true
	}
	var del []string
	kept := 0
	for _, r := range inv.Releases { // newest first
		if keep[r.ID] || kept < retain {
			kept++
			continue
		}
		del = append(del, r.ID)
	}
	return del
}

// rollbackTarget is the most recently healthy release that is not live.
func rollbackTarget(inv inventory) string {
	best := release{}
	for _, r := range inv.Releases {
		if r.ID != inv.Current && r.Healthy > best.Healthy {
			best = r
		}
	}
	return best.ID
}

func (d *Deployer) prune(ctx context.Context, host string) error {
	inv, err := d.inventory(ctx, host, false)
	if err != nil {
		return err
	}
	del := pruneSet(inv, d.cfg.Native.Retain)
	if len(del) == 0 {
		return nil
	}
	d.log.Printf("%s: pruning releases %s", host, strings.Join(del, ", "))
	_, err = d.ssh.Run(ctx, host, removeReleasesScript(d.cfg.Native.Dir, del))
	return err
}

// Rollback makes an earlier release live on every host: the named version, or
// the most recently healthy release that is not live now. It runs with the
// env file as it is (today's config), as a Docker rollback does. A release
// that fails its health check is reverted, like a failed deploy.
func (d *Deployer) Rollback(ctx context.Context, version string) error {
	hosts := d.hosts()
	locker := lock.New(d.ssh, d.cfg.Service, hosts)
	if err := locker.Acquire(ctx, firstNonEmpty(version, "rollback")); err != nil {
		return err
	}
	defer func() {
		if err := locker.Release(context.Background()); err != nil {
			d.log.Printf("warning: %v", err)
		}
	}()
	if err := hooks.Run(ctx, d.cfg.Hooks.PreRollback, map[string]string{"QIFA_VERSION": version}); err != nil {
		return err
	}

	invs := map[string]inventory{}
	for _, host := range hosts {
		inv, err := d.inventory(ctx, host, false)
		if err != nil {
			return fmt.Errorf("%s: %w", host, err)
		}
		invs[host] = inv
	}
	target := version
	if target == "" {
		target = rollbackTarget(invs[hosts[0]])
		if target == "" {
			return fmt.Errorf("%s: no earlier release has passed a health check; name one with `qifa rollback <version>`", hosts[0])
		}
	}
	var missing []string
	for _, host := range hosts {
		r, ok := invs[host].find(target)
		switch {
		case !ok:
			missing = append(missing, host)
		case r.Healthy == 0:
			d.log.Printf("warning: %s: release %s never passed a health check", host, target)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("release %s is not on %s; nothing was changed", target, strings.Join(missing, ", "))
	}

	deployment := state.Deployment{
		ID:        fmt.Sprintf("%d-%s-rollback", time.Now().UTC().Unix(), target),
		Service:   d.cfg.Service,
		Version:   target,
		Image:     "native:releases/" + target,
		Status:    state.StatusStarting,
		StartedAt: time.Now().UTC(),
	}
	if err := d.store.AppendDeployment(deployment); err != nil {
		return err
	}
	err := d.rollout(ctx, deployment, hosts, func(ctx context.Context, host string) error {
		if invs[host].Current == target {
			d.log.Printf("%s: %s is already live", host, target)
			return nil
		}
		prev, err := d.ssh.Run(ctx, host, switchScript(d.cfg.Native.Dir, target))
		if err != nil {
			return fmt.Errorf("%s: switch to %s: %w", host, target, err)
		}
		d.log.Printf("%s: current -> releases/%s (was %s)", host, target, firstNonEmpty(prev, "none"))
		if err := d.restartAndCheck(ctx, host); err != nil {
			_ = d.appendEvent(deployment.ID, host, "failed", err.Error())
			return d.recover(ctx, host, strings.TrimSpace(prev), err)
		}
		if _, err := d.ssh.Run(ctx, host, markOKScript(d.cfg.Native.Dir, target)); err != nil {
			return err
		}
		_ = d.appendEvent(deployment.ID, host, "rolled_back", target)
		return nil
	})
	if err != nil {
		return d.fail(deployment, err)
	}
	if err := d.updateStatus(deployment, state.StatusRolledBack); err != nil {
		return err
	}
	d.log.Printf("rolled back to %s", target)
	return nil
}

// Status prints the audit log, then what each host is running. Hosts are
// the source of truth; the log only explains how they got there.
func (d *Deployer) Status(ctx context.Context, out io.Writer) error {
	deployments, _, err := d.store.Snapshot()
	if err != nil {
		return err
	}
	sort.Slice(deployments, func(i, j int) bool { return deployments[i].StartedAt.After(deployments[j].StartedAt) })
	for i, dep := range deployments {
		if i == 10 {
			fmt.Fprintf(out, "... %d older\n", len(deployments)-10)
			break
		}
		fmt.Fprintf(out, "%s %s %s %s\n", dep.StartedAt.Format(time.RFC3339), dep.Service, dep.Version, dep.Status)
	}
	for _, host := range d.hosts() {
		fmt.Fprintf(out, "\n%s\n", host)
		inv, err := d.inventory(ctx, host, true)
		if err != nil {
			fmt.Fprintf(out, "  ERROR %v\n", err)
			continue
		}
		cur, ok := inv.find(inv.Current)
		switch {
		case inv.Current == "":
			fmt.Fprintln(out, "  current  (none — not deployed)")
		case !ok:
			fmt.Fprintf(out, "  current  %s (MISSING: the link points at a release that is not there)\n", inv.Current)
		case cur.Healthy == 0:
			fmt.Fprintf(out, "  current  %s (never passed a health check — interrupted deploy? `qifa deploy` or `qifa rollback`)\n", inv.Current)
		default:
			fmt.Fprintf(out, "  current  %s\n", inv.Current)
		}
		for _, unit := range d.unitsOn(host) {
			fmt.Fprintf(out, "  unit     %s %s\n", unit, inv.Units[unit])
		}
		if inv.Health != "" {
			fmt.Fprintf(out, "  health   %s\n", inv.Health)
		}
		var rels []string
		for _, r := range inv.Releases {
			tag := r.ID
			if r.Healthy > 0 {
				tag += "*"
			}
			rels = append(rels, tag)
		}
		fmt.Fprintf(out, "  releases %s  (* passed a health check; newest first)\n", firstNonEmpty(strings.Join(rels, " "), "(none)"))
		if t := rollbackTarget(inv); t != "" {
			fmt.Fprintf(out, "  rollback %s\n", t)
		}
		if d.routedOn(host) {
			routes, err := d.ssh.Run(ctx, host, proxy.NativeCLI()+" list --json")
			line := ""
			if err == nil {
				line, err = routeLine(routes, d.cfg.Service)
			}
			switch {
			case err != nil:
				fmt.Fprintf(out, "  route    ERROR kamal-proxy: %v\n", err)
			case line == "":
				fmt.Fprintln(out, "  route    (not registered — `qifa deploy` registers it)")
			default:
				fmt.Fprintf(out, "  route    %s\n", line)
			}
		}
		if len(inv.Tmp) > 0 {
			fmt.Fprintf(out, "  leftover %s (half-staged; removed by the next deploy)\n", strings.Join(inv.Tmp, " "))
		}
	}
	return nil
}

// EnvDiff compares the env the config renders now with the env file on each
// host, by key name only: values are secrets and are never printed.
func (d *Deployer) EnvDiff(ctx context.Context, out io.Writer) error {
	values, err := secrets.Collect(d.cfg.Env.Clear, d.cfg.Env.Secret, d.cfg.Env.SecretCommand)
	if err != nil {
		return err
	}
	drift := false
	for _, host := range d.hosts() {
		path := d.cfg.Native.Dir + "/etc/env"
		contents, err := d.ssh.Run(ctx, host, "cat "+q(path))
		if err != nil {
			fmt.Fprintf(out, "%s: cannot read %s: %v\n", host, path, err)
			drift = true
			continue
		}
		live, err := ParseEnv(contents)
		if err != nil {
			fmt.Fprintf(out, "%s: %s: %v\n", host, path, err)
			drift = true
			continue
		}
		lines := diffEnv(values, live)
		fmt.Fprintf(out, "%s:\n", host)
		if len(lines) == 0 {
			fmt.Fprintf(out, "  in sync (%d keys)\n", len(values))
			continue
		}
		drift = true
		for _, l := range lines {
			fmt.Fprintf(out, "  %s\n", l)
		}
	}
	if drift {
		fmt.Fprintln(out, "\nrun `qifa deploy` to apply the config's env (a restart re-reads the file on the host, which only deploy writes)")
	}
	return nil
}

func diffEnv(desired, live map[string]string) []string {
	keys := map[string]bool{}
	for k := range desired {
		keys[k] = true
	}
	for k := range live {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	var lines []string
	for _, key := range sorted {
		want, inConfig := desired[key]
		got, onHost := live[key]
		switch {
		case inConfig && !onHost:
			lines = append(lines, "+ "+key+"  (in config, missing on host)")
		case onHost && !inConfig:
			lines = append(lines, "- "+key+"  (on host, not in config)")
		case want != got:
			lines = append(lines, "~ "+key+"  (value differs)")
		}
	}
	return lines
}

// ParseEnv reads an env file as RenderEnv writes it (and as systemd reads it
// for the forms RenderEnv uses): KEY="value" with \ " $ ` escaped and literal
// newlines inside the quotes, or a bare KEY=value.
func ParseEnv(s string) (map[string]string, error) {
	out := map[string]string{}
	for i := 0; i < len(s); {
		end := strings.IndexByte(s[i:], '\n')
		line := s[i:]
		if end >= 0 {
			line = s[i : i+end]
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			if end < 0 {
				break
			}
			i += end + 1
			continue
		}
		key, rest, ok := strings.Cut(trimmed, "=")
		if !ok || !envKeyRe.MatchString(key) {
			return nil, fmt.Errorf("malformed line %q", abbreviate(trimmed))
		}
		if !strings.HasPrefix(rest, `"`) {
			out[key] = rest
			if end < 0 {
				break
			}
			i += end + 1
			continue
		}
		// Quoted value: scan from just after the opening quote, across
		// newlines, to the closing unescaped quote.
		j := i + strings.Index(s[i:], `"`) + 1
		var v strings.Builder
		closed := false
		for ; j < len(s); j++ {
			c := s[j]
			if c == '\\' && j+1 < len(s) {
				j++
				v.WriteByte(s[j])
				continue
			}
			if c == '"' {
				closed = true
				j++
				break
			}
			v.WriteByte(c)
		}
		if !closed {
			return nil, fmt.Errorf("unterminated quote in %s", key)
		}
		out[key] = v.String()
		next := strings.IndexByte(s[j:], '\n')
		if next < 0 {
			break
		}
		i = j + next + 1
	}
	return out, nil
}

func abbreviate(s string) string {
	if len(s) > 40 {
		return s[:40] + "…"
	}
	return s
}

// Reconcile deploys only when a host differs from the config: the wrong
// release is live, the live release never passed its health check, or a unit
// is not running. Usable from a timer, like the Docker mode's reconcile.
func (d *Deployer) Reconcile(ctx context.Context, out io.Writer, dryRun bool) error {
	v, err := d.resolveVersion()
	if err != nil {
		return err
	}
	var drifts []string
	for _, host := range d.hosts() {
		inv, err := d.inventory(ctx, host, false)
		if err != nil {
			return fmt.Errorf("%s: %w", host, err)
		}
		r, ok := inv.find(inv.Current)
		switch {
		case inv.Current != v:
			drifts = append(drifts, fmt.Sprintf("%s — live release is %s, config is %s", host, firstNonEmpty(inv.Current, "none"), v))
		case !ok || r.Healthy == 0:
			drifts = append(drifts, fmt.Sprintf("%s — live release %s never passed a health check", host, v))
		}
		for _, unit := range d.unitsOn(host) {
			if st := inv.Units[unit]; st != "active" {
				drifts = append(drifts, fmt.Sprintf("%s — %s is %s", host, unit, firstNonEmpty(st, "unknown")))
			}
		}
	}
	if len(drifts) == 0 {
		fmt.Fprintf(out, "in sync: %s at %s\n", d.cfg.Service, v)
		return nil
	}
	for _, line := range drifts {
		fmt.Fprintf(out, "drift: %s\n", line)
	}
	if dryRun {
		fmt.Fprintf(out, "%d difference(s); not converging (--dry-run)\n", len(drifts))
		return nil
	}
	fmt.Fprintf(out, "converging %s to %s\n", d.cfg.Service, v)
	return d.Deploy(ctx)
}

// Doctor runs the deploy preflight on every host and reports it, without
// building anything or taking the lock.
func (d *Deployer) Doctor(ctx context.Context, out io.Writer) error {
	failed := 0
	for _, host := range d.hosts() {
		problems, warnings, err := d.checkHost(ctx, host, "")
		switch {
		case err != nil:
			fmt.Fprintf(out, "%s: FAIL cannot run checks: %v\n", host, err)
			failed++
			continue
		case len(problems) == 0:
			fmt.Fprintf(out, "%s: ok (systemd, dbus, %s, sudo rules, units)\n", host, d.cfg.Native.Dir)
		default:
			failed++
			for _, p := range problems {
				fmt.Fprintf(out, "%s: FAIL %s\n", host, p)
			}
		}
		for _, w := range warnings {
			fmt.Fprintf(out, "%s: warn %s\n", host, w)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d host(s) not ready", failed)
	}
	return nil
}

// Remove needs root (units and sudoers are root's), which deploys never have,
// so it is a separate command run like setup.
func (d *Deployer) Remove(context.Context) error {
	return errors.New("removing a native service deletes its units and sudoers rule, which needs root: " +
		"run `qifa native remove --ssh-user root` (add --purge to also delete releases, shared data and the service account)")
}

// Teardown undoes Setup on every host, as root.
func (d *Deployer) Teardown(ctx context.Context, purge bool) error {
	for _, host := range d.hosts() {
		script := teardownScript(d.cfg.Native.Dir, d.cfg.Native.User, d.sudoersFile(), d.unitsOn(host), purge)
		if d.routedOn(host) {
			// The route goes first, so the proxy stops sending traffic to a
			// port nothing will listen on.
			script = fmt.Sprintf("if %s remove %s >/dev/null 2>&1; then echo \"removed route %s\"; fi\n", proxy.NativeCLI(), q(d.cfg.Service), d.cfg.Service) + script
		}
		wrapped := fmt.Sprintf(`if [ "$(id -u)" = 0 ]; then sh -c %[1]s; else sudo -n sh -c %[1]s; fi`, q(script))
		out, err := d.ssh.Run(ctx, host, wrapped)
		for _, line := range strings.Split(out, "\n") {
			if line != "" {
				d.log.Printf("remove %s: %s", host, line)
			}
		}
		if err != nil {
			return fmt.Errorf("native remove on %s (needs root, or passwordless sudo — try --ssh-user): %w", host, err)
		}
	}
	return nil
}

// routeLine describes the service's route from `kamal-proxy list --json`
// (kamal-proxy v0.10+), or "" when it has none.
func routeLine(listJSON, service string) (string, error) {
	var services map[string]struct {
		Hosts        []string `json:"hosts"`
		PathPrefixes []string `json:"path_prefixes"`
		Targets      []string `json:"targets"`
		State        string   `json:"state"`
		TLS          bool     `json:"tls"`
	}
	if err := json.Unmarshal([]byte(listJSON), &services); err != nil {
		return "", fmt.Errorf("unexpected `kamal-proxy list --json` output: %w", err)
	}
	s, ok := services[service]
	if !ok {
		return "", nil
	}
	parts := append([]string{}, s.Hosts...)
	for _, p := range s.PathPrefixes {
		if p != "/" { // kamal-proxy reports the default prefix as "/"
			parts = append(parts, p)
		}
	}
	from := strings.Join(parts, ",")
	if from == "" {
		from = "*"
	}
	state := s.State
	if s.TLS {
		state += ", tls"
	}
	return fmt.Sprintf("%s -> %s (%s)", from, strings.Join(s.Targets, ","), state), nil
}
