package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/tendant/qifa/internal/native"
)

const nativeUsage = `usage:
  qifa native setup  [--ssh-user <user>] [--deploy-user <user>]
  qifa native remove [--ssh-user <user>] [--purge [--yes]]`

// confirmInput is where `native remove --purge` reads its confirmation.
var confirmInput io.Reader = os.Stdin

// runNative handles `qifa native ...`: the commands that need root on the
// hosts, which ordinary deploys never have.
func runNative(ctx context.Context, args []string, stdout, stderr io.Writer, configFile string) error {
	if len(args) == 0 || (args[0] != "setup" && args[0] != "remove") {
		return errors.New(nativeUsage)
	}
	verb := args[0]
	sshUser, deployUser := "", ""
	purge, yes := false, false
	rest := args[1:]
	for i := 0; i < len(rest); i++ {
		switch flag := rest[i]; {
		case flag == "--ssh-user" || (flag == "--deploy-user" && verb == "setup"):
			if i+1 >= len(rest) {
				return fmt.Errorf("%s requires a value", flag)
			}
			if flag == "--ssh-user" {
				sshUser = rest[i+1]
			} else {
				deployUser = rest[i+1]
			}
			i++
		case flag == "--purge" && verb == "remove":
			purge = true
		case flag == "--yes" && verb == "remove":
			yes = true
		default:
			return fmt.Errorf("unknown flag %q\n%s", flag, nativeUsage)
		}
	}
	if yes && !purge {
		return errors.New("--yes only applies to --purge")
	}
	cfg, store, err := loadConfig(configFile)
	if err != nil {
		return err
	}
	if !cfg.IsNative() {
		return fmt.Errorf("qifa native %s needs a native: block in the config", verb)
	}
	d, err := native.New(cfg, store, stdout, stderr)
	if err != nil {
		return err
	}
	// The deploy user defaults to ssh.user as configured, before any
	// --ssh-user override: setup may connect as root, deploys never do.
	if deployUser == "" {
		deployUser = cfg.SSH.User
	}
	if sshUser != "" {
		d.UseSSHUser(sshUser)
	}
	if verb == "setup" {
		return d.Setup(ctx, deployUser)
	}
	if purge && !yes {
		fmt.Fprintf(stdout, "--purge deletes %s on every host — every release and all shared data — and the %s account.\nType the service name (%s) to confirm: ", cfg.Native.Dir, cfg.Native.User, cfg.Service)
		line, _ := bufio.NewReader(confirmInput).ReadString('\n')
		if strings.TrimSpace(line) != cfg.Service {
			return errors.New("not confirmed; nothing was removed")
		}
	}
	return d.Teardown(ctx, purge)
}

const nativeProxyUsage = `usage (native proxy):
  qifa proxy boot|upgrade [--ssh-user <user>] [--deploy-user <user>]
  qifa proxy remove [--ssh-user <user>] [--purge [--yes]]
  qifa proxy start|stop|restart|details
  qifa proxy logs [--follow] [--lines N]`

// isNative reports whether the config selects native mode, for commands
// whose meaning differs between modes.
func isNative(configFile string) (bool, error) {
	cfg, _, err := loadConfig(configFile)
	if err != nil {
		return false, err
	}
	return cfg.IsNative(), nil
}

// runNativeProxy handles `qifa proxy ...` for a native config: kamal-proxy
// as a systemd service. boot, upgrade and remove need root (--ssh-user).
func runNativeProxy(ctx context.Context, verb string, rest []string, stdout, stderr io.Writer, configFile string) error {
	sshUser, deployUser := "", ""
	purge, yes, follow := false, false, false
	lines := 200
	rootVerb := verb == "boot" || verb == "upgrade" || verb == "remove"
	for i := 0; i < len(rest); i++ {
		switch flag := rest[i]; {
		case (flag == "--ssh-user" || flag == "--deploy-user") && rootVerb:
			if i+1 >= len(rest) {
				return fmt.Errorf("%s requires a value", flag)
			}
			if flag == "--ssh-user" {
				sshUser = rest[i+1]
			} else {
				deployUser = rest[i+1]
			}
			i++
		case flag == "--purge" && verb == "remove":
			purge = true
		case flag == "--yes" && verb == "remove":
			yes = true
		case (flag == "--follow" || flag == "-f") && verb == "logs":
			follow = true
		case (flag == "--lines" || flag == "-n") && verb == "logs":
			if i+1 >= len(rest) {
				return fmt.Errorf("%s requires a value", flag)
			}
			n, err := strconv.Atoi(rest[i+1])
			if err != nil {
				return fmt.Errorf("--lines: %w", err)
			}
			lines = n
			i++
		default:
			return fmt.Errorf("unknown flag %q\n%s", flag, nativeProxyUsage)
		}
	}
	cfg, store, err := loadConfig(configFile)
	if err != nil {
		return err
	}
	d, err := native.New(cfg, store, stdout, stderr)
	if err != nil {
		return err
	}
	if deployUser == "" {
		deployUser = cfg.SSH.User
	}
	if sshUser != "" {
		d.UseSSHUser(sshUser)
	}
	switch verb {
	case "boot", "upgrade":
		return d.ProxyBoot(ctx, deployUser)
	case "remove":
		if purge && !yes {
			fmt.Fprintf(stdout, "--purge deletes kamal-proxy's routes and certificates on every proxy host.\nType \"qifa-proxy\" to confirm: ")
			line, _ := bufio.NewReader(confirmInput).ReadString('\n')
			if strings.TrimSpace(line) != "qifa-proxy" {
				return errors.New("not confirmed; nothing was removed")
			}
		}
		return d.ProxyRemove(ctx, purge)
	case "start", "stop", "restart":
		return d.ProxyCtl(ctx, verb)
	case "logs":
		return d.ProxyLogs(ctx, lines, follow, stdout)
	case "details":
		return d.ProxyDetails(ctx, stdout)
	default:
		return errors.New(nativeProxyUsage)
	}
}
