package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// ansi matches the colour escapes kamal-proxy writes even when its output is
// piped, so the table has to be stripped before it can be read as data.
var ansi = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// Registration is one row of `kamal-proxy list`: what the proxy currently
// sends a hostname to.
type Registration struct {
	Service string
	Host    string
	Target  string // container:port
	State   string
}

// Registered returns what the proxy on host has registered for service, and
// whether it has anything at all. Reconciliation needs this: a container can
// be running and healthy while the proxy still points at its predecessor, and
// from the outside that looks like a deploy that silently did nothing.
//
// It asks for `list --json` (kamal-proxy v0.10+), which is structured, and
// falls back to the coloured display table for an older proxy.
func (k *KamalProxy) Registered(ctx context.Context, host, service string) (Registration, bool, error) {
	var rows map[string]Registration
	if out, err := k.client.Run(ctx, host, k.cli+" list --json"); err == nil {
		rows, err = parseRegistrationsJSON(out)
		if err != nil {
			return Registration{}, false, fmt.Errorf("read proxy routes on %s: %w", host, err)
		}
	} else {
		out, err := k.client.Run(ctx, host, k.cli+" list")
		if err != nil {
			return Registration{}, false, fmt.Errorf("read proxy routes on %s: %w", host, err)
		}
		rows = parseRegistrations(out)
	}
	row, ok := rows[service]
	return row, ok, nil
}

// parseRegistrationsJSON reads `kamal-proxy list --json`: services keyed by
// name, each with lists of hosts and targets.
func parseRegistrationsJSON(out string) (map[string]Registration, error) {
	var services map[string]struct {
		Hosts   []string `json:"hosts"`
		Targets []string `json:"targets"`
		State   string   `json:"state"`
	}
	if err := json.Unmarshal([]byte(out), &services); err != nil {
		return nil, fmt.Errorf("unexpected `kamal-proxy list --json` output: %w", err)
	}
	rows := make(map[string]Registration, len(services))
	for name, s := range services {
		rows[name] = Registration{
			Service: name,
			Host:    strings.Join(s.Hosts, ","),
			Target:  strings.Join(s.Targets, ","),
			State:   s.State,
		}
	}
	return rows, nil
}

// parseRegistrations reads the `kamal-proxy list` table into rows keyed by
// service.
func parseRegistrations(out string) map[string]Registration {
	rows := map[string]Registration{}
	for _, line := range strings.Split(ansi.ReplaceAllString(out, ""), "\n") {
		fields := strings.Fields(line)
		// Service Host Path Target State TLS
		if len(fields) < 5 || fields[0] == "Service" {
			continue
		}
		rows[fields[0]] = Registration{
			Service: fields[0],
			Host:    fields[1],
			Target:  fields[3],
			State:   fields[4],
		}
	}
	return rows
}
