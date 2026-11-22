package app

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/tendant/qifa/internal/config"
)

// runInit writes a starter config that deploys as written: shaped by the
// directory it goes in (its name, its Dockerfile) and by --host and
// --domain. It checks the result loads before writing, and never replaces
// an existing file without --force.
func runInit(args []string, stdout io.Writer) error {
	host, args, err := takeFlag(args, "--host")
	if err != nil {
		return err
	}
	domain, args, err := takeFlag(args, "--domain")
	if err != nil {
		return err
	}
	path := "qifa.yaml"
	force := false
	for _, a := range args {
		switch {
		case a == "--force":
			force = true
		case strings.HasPrefix(a, "-"):
			return fmt.Errorf("unknown flag %q", a)
		default:
			path = a
		}
	}
	if _, err := os.Stat(path); err == nil && !force {
		return fmt.Errorf("%s already exists; not overwriting it (qifa init --force replaces it)", path)
	}

	starter := config.DetectStarter(filepath.Dir(path))
	starter.SetHost(host)
	starter.Domain = strings.ToLower(strings.TrimSpace(domain))
	if err := starter.Check(); err != nil {
		return err
	}

	// Load what would be written before writing it, so init never leaves a
	// config qifa itself rejects.
	scratch, err := os.MkdirTemp("", "qifa-init-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	if err := config.WriteStarter(filepath.Join(scratch, "qifa.yaml"), starter); err != nil {
		return err
	}
	if _, err := config.Load(filepath.Join(scratch, "qifa.yaml")); err != nil {
		return fmt.Errorf("the starter config for these flags does not load: %w", err)
	}
	if err := config.WriteStarter(path, starter); err != nil {
		return err
	}

	fmt.Fprintf(stdout, "wrote starter config to %s\n", path)
	if starter.Build {
		if starter.AppPort != 0 {
			fmt.Fprintf(stdout, "  found a Dockerfile: each host builds it; the app listens on %d (its EXPOSE)\n", starter.AppPort)
		} else {
			fmt.Fprintln(stdout, "  found a Dockerfile: each host builds it; it has no EXPOSE, so set servers.web.app_port to the port your app listens on (80 now)")
		}
		fmt.Fprintln(stdout, "  healthcheck.path is /: set it to a route your app answers with 2xx (such as /up), or the deploy fails")
	}
	if starter.Host != "" {
		who := "as ~/.ssh/config says, or as your local user (--host user@host to choose)"
		if starter.SSHUser != "" {
			who = "as " + starter.SSHUser
		}
		fmt.Fprintf(stdout, "  qifa connects to %s over SSH %s, and needs Docker there; qifa doctor checks both\n", starter.Host, who)
	}
	if starter.Domain != "" {
		fmt.Fprintf(stdout, "  before deploying, point DNS for %s at the host and open ports 80 and 443\n", starter.Domain)
	}
	fmt.Fprintln(stdout, "next: qifa deploy (qifa config shows what qifa reads)")
	return nil
}
