// Package fleet holds what the Docker and native deployers share: which
// hosts and roles a config spans and in what order, how a rollout walks them,
// how the audit log records it, and the small pieces of output both print.
// Keeping one copy is what keeps the two modes behaving alike.
package fleet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/tendant/qifa/internal/config"
	"github.com/tendant/qifa/internal/state"
)

// OrderedRoles returns role names with web first, then the rest sorted, the
// order every deploy, rollback and status walks them in.
func OrderedRoles(servers map[string]config.Server) []string {
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

// Hosts lists each host of the roles once, in role order.
func Hosts(servers map[string]config.Server) []string {
	seen := map[string]bool{}
	var out []string
	for _, role := range OrderedRoles(servers) {
		for _, h := range servers[role].Hosts {
			if h != "" && !seen[h] {
				seen[h] = true
				out = append(out, h)
			}
		}
	}
	return out
}

// RolesOn lists the roles that run on host, in role order.
func RolesOn(servers map[string]config.Server, host string) []string {
	var roles []string
	for _, role := range OrderedRoles(servers) {
		if slices.Contains(servers[role].Hosts, host) {
			roles = append(roles, role)
		}
	}
	return roles
}

// Batches runs fn over hosts in rollout.batch_size batches: the hosts of a
// batch in parallel, batches one after another with rollout.batch_wait
// between them, stopping after the first batch with a failure. logf, if set,
// is told how many hosts that skipped.
func Batches(ctx context.Context, hosts []string, rollout config.Rollout, logf func(string, ...any), fn func(ctx context.Context, host string) error) error {
	size := 1
	if rollout.BatchSize != nil {
		size = *rollout.BatchSize
	}
	if size <= 0 || size > len(hosts) {
		size = len(hosts)
	}
	for i := 0; i < len(hosts); i += size {
		batch := hosts[i:min(i+size, len(hosts))]
		if i > 0 && rollout.BatchWait > 0 {
			select {
			case <-time.After(rollout.BatchWait):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		errs := make([]error, len(batch))
		if len(batch) == 1 {
			errs[0] = fn(ctx, batch[0])
		} else {
			var wg sync.WaitGroup
			for j, host := range batch {
				wg.Go(func() { errs[j] = fn(ctx, host) })
			}
			wg.Wait()
		}
		if err := errors.Join(errs...); err != nil {
			if rest := len(hosts) - (i + len(batch)); rest > 0 && logf != nil {
				logf("skipping %d remaining host(s)", rest)
			}
			return err
		}
	}
	return nil
}

// Audit records a deployment's progress in the append-only log.
type Audit struct{ Store *state.Store }

// Status appends the deployment with its next status, stamping the finish
// time on a terminal one.
func (a Audit) Status(dep state.Deployment, next state.Status) error {
	dep.Status = next
	if next == state.StatusSucceeded || next == state.StatusFailed || next == state.StatusRolledBack {
		now := time.Now().UTC()
		dep.FinishedAt = &now
	}
	return a.Store.AppendDeployment(dep)
}

// Fail marks the deployment failed and returns cause.
func (a Audit) Fail(dep state.Deployment, cause error) error {
	_ = a.Status(dep, state.StatusFailed)
	return cause
}

// Event appends one event of a deployment on a host.
func (a Audit) Event(deploymentID, host, role, eventType, message string) error {
	return a.Store.AppendEvent(state.Event{
		ID:           fmt.Sprintf("%d", time.Now().UnixNano()),
		DeploymentID: deploymentID,
		Host:         host,
		Role:         role,
		EventType:    eventType,
		Message:      message,
		CreatedAt:    time.Now().UTC(),
	})
}

// DiffEnv names the keys that differ between the env the config renders and
// the env on a host. Values are secrets and never appear.
func DiffEnv(desired, live map[string]string) []string {
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

// WriteLockStatus prints the deploy lock's holder per host.
func WriteLockStatus(out io.Writer, hosts []string, holders map[string]string) {
	for _, host := range hosts {
		if h := holders[host]; h != "" {
			fmt.Fprintf(out, "%s\t%s\n", host, h)
		} else {
			fmt.Fprintf(out, "%s\t(free)\n", host)
		}
	}
}

// PrefixWriter writes whole lines to Out, each starting with Prefix. Writers
// sharing Mu never interleave inside a line, so several hosts can stream into
// one terminal. Flush writes a trailing partial line.
type PrefixWriter struct {
	Prefix string
	Out    io.Writer
	Mu     *sync.Mutex
	buf    []byte
}

func (w *PrefixWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			return len(p), nil
		}
		w.emit(w.buf[:i+1])
		w.buf = w.buf[i+1:]
	}
}

func (w *PrefixWriter) Flush() {
	if len(w.buf) > 0 {
		w.emit(append(w.buf, '\n'))
		w.buf = nil
	}
}

func (w *PrefixWriter) emit(line []byte) {
	w.Mu.Lock()
	defer w.Mu.Unlock()
	io.WriteString(w.Out, w.Prefix)
	w.Out.Write(line)
}
