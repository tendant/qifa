package proxy

import (
	"context"
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
func (k *KamalProxy) Registered(ctx context.Context, host, service string) (Registration, bool, error) {
	out, err := k.client.Run(ctx, host, "docker exec "+shellQuote(proxyContainerName)+" kamal-proxy list")
	if err != nil {
		return Registration{}, false, fmt.Errorf("read proxy routes on %s: %w", host, err)
	}
	row, ok := parseRegistrations(out)[service]
	return row, ok, nil
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
