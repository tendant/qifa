package app

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/tendant/qifa/internal/native"
)

const nativeUsage = "usage: qifa native setup [--ssh-user <user>] [--deploy-user <user>]"

// runNative handles `qifa native ...`: commands that exist only in native mode.
func runNative(ctx context.Context, args []string, stdout, stderr io.Writer, configFile string) error {
	if len(args) == 0 || args[0] != "setup" {
		return errors.New(nativeUsage)
	}
	sshUser, deployUser := "", ""
	rest := args[1:]
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "--ssh-user", "--deploy-user":
			if i+1 >= len(rest) {
				return fmt.Errorf("%s requires a value", rest[i])
			}
			if rest[i] == "--ssh-user" {
				sshUser = rest[i+1]
			} else {
				deployUser = rest[i+1]
			}
			i++
		default:
			return fmt.Errorf("unknown flag %q\n%s", rest[i], nativeUsage)
		}
	}
	cfg, store, err := loadConfig(configFile)
	if err != nil {
		return err
	}
	if !cfg.IsNative() {
		return errors.New("qifa native setup needs a native: block in the config")
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
	return d.Setup(ctx, deployUser)
}
