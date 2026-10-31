package fleet

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/tendant/qifa/internal/state"
)

// HostStatus is what `qifa status` shows for one host, in either mode: the
// role processes (containers or units), the versions available on the host,
// the one a rollback would go to, and the proxy route.
type HostStatus struct {
	Host string
	Err  error

	Roles []RoleStatus
	// Health is the HTTP check's answer ("up"/"down"), or "" when none ran.
	Health string
	// Versions are newest first. Healthy marks one known to have passed a
	// health check, when the mode records that (native does).
	Versions []Version
	Rollback string
	Route    string
	Notes    []string
}

type RoleStatus struct {
	Role    string
	Version string // "" when nothing runs
	State   string
	Name    string // container or unit
}

type Version struct {
	Version string
	Healthy bool
}

// WriteHistory prints the latest deployments from the audit log, newest
// first, and the events of the most recent one.
func WriteHistory(out io.Writer, deployments []state.Deployment, events []state.Event, limit int) {
	sort.Slice(deployments, func(i, j int) bool { return deployments[i].StartedAt.After(deployments[j].StartedAt) })
	for i, dep := range deployments {
		if i == limit {
			fmt.Fprintf(out, "... %d older\n", len(deployments)-limit)
			break
		}
		fmt.Fprintf(out, "%s %s %s %s\n", dep.StartedAt.Format(time.RFC3339), dep.Service, dep.Version, dep.Status)
	}
	if len(deployments) == 0 {
		return
	}
	latest := deployments[0].ID
	var mine []state.Event
	for _, e := range events {
		if e.DeploymentID == latest {
			mine = append(mine, e)
		}
	}
	if len(mine) == 0 {
		return
	}
	fmt.Fprintf(out, "\nevents of %s:\n", latest)
	for _, e := range mine {
		where := e.Host
		if e.Role != "" {
			where = e.Role + " " + e.Host
		}
		msg := strings.TrimSpace(strings.SplitN(e.Message, "\n", 2)[0])
		fmt.Fprintf(out, "  %s %s %s %s\n", e.CreatedAt.Format(time.RFC3339), where, e.EventType, msg)
	}
}

// WriteHostStatus prints one host's block of `qifa status`.
func WriteHostStatus(out io.Writer, h HostStatus) {
	fmt.Fprintf(out, "\n%s\n", h.Host)
	if h.Err != nil {
		fmt.Fprintf(out, "  ERROR    %v\n", h.Err)
		return
	}
	for _, r := range h.Roles {
		if r.Version == "" {
			fmt.Fprintf(out, "  %-8s (nothing running)\n", r.Role)
			continue
		}
		fmt.Fprintf(out, "  %-8s %s  %s  %s\n", r.Role, r.Version, r.State, r.Name)
	}
	if h.Health != "" {
		fmt.Fprintf(out, "  health   %s\n", h.Health)
	}
	if len(h.Versions) > 0 {
		marked := false
		names := make([]string, len(h.Versions))
		for i, v := range h.Versions {
			names[i] = v.Version
			if v.Healthy {
				names[i] += "*"
				marked = true
			}
		}
		legend := "(newest first)"
		if marked {
			legend = "(* passed a health check; newest first)"
		}
		fmt.Fprintf(out, "  versions %s  %s\n", strings.Join(names, " "), legend)
	}
	if h.Rollback != "" {
		fmt.Fprintf(out, "  rollback %s\n", h.Rollback)
	}
	if h.Route != "" {
		fmt.Fprintf(out, "  route    %s\n", h.Route)
	}
	for _, n := range h.Notes {
		fmt.Fprintf(out, "  note     %s\n", n)
	}
}
