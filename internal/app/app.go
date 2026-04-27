package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/gokamal/gocart/internal/config"
	"github.com/gokamal/gocart/internal/deploy"
	"github.com/gokamal/gocart/internal/state"
)

var buildVersion, buildCommit, buildDate string

// SetVersion is called from main with linker-injected build metadata.
func SetVersion(version, commit, date string) {
	buildVersion = version
	buildCommit = commit
	buildDate = date
}

func versionString() string {
	v := buildVersion
	c := buildCommit
	d := buildDate
	if v == "" || c == "" {
		if info, ok := debug.ReadBuildInfo(); ok {
			if v == "" {
				v = info.Main.Version
			}
			for _, s := range info.Settings {
				switch s.Key {
				case "vcs.revision":
					if c == "" {
						c = s.Value
					}
				case "vcs.time":
					if d == "" {
						d = s.Value
					}
				}
			}
		}
	}
	if v == "" {
		v = "(devel)"
	}
	if c == "" {
		c = "unknown"
	}
	if d == "" {
		d = "unknown"
	}
	if len(c) > 12 {
		c = c[:12]
	}
	return fmt.Sprintf("qifa %s (commit %s, built %s)", v, c, d)
}

func Run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		printUsage(stdout)
		return nil
	}

	switch args[0] {
	case "version", "--version", "-v":
		_, err := fmt.Fprintln(stdout, versionString())
		return err
	case "config":
		cfg, err := config.Load("qifa.yaml")
		if err != nil {
			return err
		}
		data, err := cfg.Marshal()
		if err != nil {
			return err
		}
		_, err = stdout.Write(data)
		return err
	case "init":
		path := "qifa.yaml"
		if len(args) > 1 {
			path = args[1]
		}
		_, err := fmt.Fprintf(stdout, "wrote starter config to %s\n", path)
		if err != nil {
			return err
		}
		return config.WriteSample(path)
	case "deploy":
		dryRun := false
		for _, a := range args[1:] {
			if a == "--dry-run" || a == "-n" {
				dryRun = true
			}
		}
		return withRuntime(ctx, stdout, stderr, func(rt *runtime) error {
			if dryRun {
				return rt.deployer.Plan(ctx, stdout)
			}
			return rt.deployer.Deploy(ctx)
		})
	case "rollback":
		version := ""
		if len(args) > 1 {
			version = args[1]
		}
		return withRuntime(ctx, stdout, stderr, func(rt *runtime) error {
			return rt.deployer.Rollback(ctx, version)
		})
	case "stop":
		return withRuntime(ctx, stdout, stderr, func(rt *runtime) error {
			return rt.deployer.Stop(ctx)
		})
	case "start":
		return withRuntime(ctx, stdout, stderr, func(rt *runtime) error {
			return rt.deployer.Start(ctx)
		})
	case "restart":
		return withRuntime(ctx, stdout, stderr, func(rt *runtime) error {
			return rt.deployer.Restart(ctx)
		})
	case "remove":
		return withRuntime(ctx, stdout, stderr, func(rt *runtime) error {
			return rt.deployer.Remove(ctx)
		})
	case "prune":
		return withRuntime(ctx, stdout, stderr, func(rt *runtime) error {
			return rt.deployer.Prune(ctx)
		})
	case "backup":
		return withRuntime(ctx, stdout, stderr, func(rt *runtime) error {
			return rt.deployer.Backup(ctx)
		})
	case "restore":
		var localPath string
		if len(args) > 1 {
			localPath = args[1]
		} else {
			localPath = os.Getenv("RESTORE_FROM")
		}
		if localPath == "" {
			return errors.New("usage: qifa restore <local-file>  (or set RESTORE_FROM=<path>)")
		}
		return withRuntime(ctx, stdout, stderr, func(rt *runtime) error {
			return rt.deployer.Restore(ctx, localPath)
		})
	case "sweep":
		return withRuntime(ctx, stdout, stderr, func(rt *runtime) error {
			return rt.deployer.SweepStaleContainers(ctx)
		})
	case "sync":
		return withRuntime(ctx, stdout, stderr, func(rt *runtime) error {
			return rt.deployer.SyncFiles(ctx)
		})
	case "proxy":
		if len(args) < 2 {
			return errors.New("usage: qifa proxy <boot|start|stop|restart|upgrade|remove|logs|details>")
		}
		verb := args[1]
		rest := args[2:]
		return withRuntime(ctx, stdout, stderr, func(rt *runtime) error {
			switch verb {
			case "boot":
				return rt.deployer.ProxyBoot(ctx)
			case "start":
				return rt.deployer.ProxyStart(ctx)
			case "stop":
				return rt.deployer.ProxyStop(ctx)
			case "restart":
				return rt.deployer.ProxyRestart(ctx)
			case "upgrade":
				return rt.deployer.ProxyUpgrade(ctx)
			case "remove":
				purge := false
				for _, a := range rest {
					if a == "--purge" {
						purge = true
					}
				}
				return rt.deployer.ProxyRemove(ctx, purge)
			case "details":
				return rt.deployer.ProxyDetails(ctx)
			case "logs":
				lines := 200
				follow := false
				for i := 0; i < len(rest); i++ {
					switch rest[i] {
					case "--follow", "-f":
						follow = true
					case "--lines", "-n":
						if i+1 >= len(rest) {
							return errors.New("--lines requires a value")
						}
						n, err := strconv.Atoi(rest[i+1])
						if err != nil {
							return fmt.Errorf("--lines: %w", err)
						}
						lines = n
						i++
					}
				}
				return rt.deployer.ProxyLogs(ctx, lines, follow)
			default:
				return errors.New("usage: qifa proxy <boot|start|stop|restart|upgrade|remove|logs|details>")
			}
		})
	case "lock":
		if len(args) < 2 {
			return errors.New("usage: qifa lock <status|release>")
		}
		switch args[1] {
		case "status":
			return withRuntime(ctx, stdout, stderr, func(rt *runtime) error {
				return rt.deployer.LockStatus(ctx, stdout)
			})
		case "release":
			return withRuntime(ctx, stdout, stderr, func(rt *runtime) error {
				return rt.deployer.LockRelease(ctx)
			})
		default:
			return errors.New("usage: qifa lock <status|release>")
		}
	case "status":
		return withRuntime(ctx, stdout, stderr, func(rt *runtime) error {
			return rt.deployer.Status(ctx, stdout)
		})
	case "logs":
		lines := 200
		follow := false
		rest := args[1:]
		for i := 0; i < len(rest); i++ {
			switch rest[i] {
			case "--follow", "-f":
				follow = true
			case "--lines", "-n":
				if i+1 >= len(rest) {
					return errors.New("--lines requires a value")
				}
				n, err := strconv.Atoi(rest[i+1])
				if err != nil {
					return fmt.Errorf("--lines: %w", err)
				}
				lines = n
				i++
			default:
				return fmt.Errorf("unknown flag %q", rest[i])
			}
		}
		return withRuntime(ctx, stdout, stderr, func(rt *runtime) error {
			return rt.deployer.Logs(ctx, lines, follow, stdout)
		})
	case "app":
		if len(args) < 2 {
			return errors.New("usage: qifa app <exec <command> | containers>")
		}
		switch args[1] {
		case "exec":
			if len(args) < 3 {
				return errors.New("usage: qifa app exec <command>")
			}
			command := strings.Join(args[2:], " ")
			return withRuntime(ctx, stdout, stderr, func(rt *runtime) error {
				return rt.deployer.Exec(ctx, command, stdout)
			})
		case "containers":
			return withRuntime(ctx, stdout, stderr, func(rt *runtime) error {
				return rt.deployer.ListContainers(ctx, stdout)
			})
		case "maintenance":
			message := "Down for maintenance"
			drainTimeout := 30 * time.Second
			rest := args[2:]
			for i := 0; i < len(rest); i++ {
				switch rest[i] {
				case "--message":
					if i+1 >= len(rest) {
						return errors.New("--message requires a value")
					}
					message = rest[i+1]
					i++
				case "--drain-timeout":
					if i+1 >= len(rest) {
						return errors.New("--drain-timeout requires a value")
					}
					d, err := time.ParseDuration(rest[i+1])
					if err != nil {
						return fmt.Errorf("--drain-timeout: %w", err)
					}
					drainTimeout = d
					i++
				default:
					return fmt.Errorf("unknown flag %q", rest[i])
				}
			}
			return withRuntime(ctx, stdout, stderr, func(rt *runtime) error {
				return rt.deployer.Maintenance(ctx, message, drainTimeout)
			})
		case "live":
			return withRuntime(ctx, stdout, stderr, func(rt *runtime) error {
				return rt.deployer.Live(ctx)
			})
		default:
			return errors.New("usage: qifa app <exec <command> | containers | maintenance | live>")
		}
	case "accessory":
		if len(args) < 3 {
			return errors.New("usage: qifa accessory <boot|stop|start|restart|remove|logs|exec> <name> [args...]")
		}
		verb := args[1]
		name := args[2]
		return withRuntime(ctx, stdout, stderr, func(rt *runtime) error {
			switch verb {
			case "boot":
				return rt.deployer.AccessoryBoot(ctx, name)
			case "stop":
				return rt.deployer.AccessoryStop(ctx, name)
			case "start":
				return rt.deployer.AccessoryStart(ctx, name)
			case "restart":
				return rt.deployer.AccessoryRestart(ctx, name)
			case "remove":
				return rt.deployer.AccessoryRemove(ctx, name)
			case "logs":
				return rt.deployer.AccessoryLogs(ctx, name, stdout)
			case "exec":
				if len(args) < 4 {
					return errors.New("usage: qifa accessory exec <name> <command>")
				}
				return rt.deployer.AccessoryExec(ctx, name, strings.Join(args[3:], " "), stdout)
			default:
				return errors.New("usage: qifa accessory <boot|stop|start|restart|remove|logs|exec> <name> [args...]")
			}
		})
	default:
		printUsage(stdout)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

type runtime struct {
	deployer *deploy.Deployer
}

func withRuntime(ctx context.Context, stdout, stderr io.Writer, fn func(*runtime) error) error {
	cfg, err := config.Load("qifa.yaml")
	if err != nil {
		return err
	}
	store, err := state.NewStore(".qifa/state.jsonl")
	if err != nil {
		return err
	}
	d, err := deploy.New(cfg, store, stdout, stderr)
	if err != nil {
		return err
	}
	return fn(&runtime{deployer: d})
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: qifa <command>")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "commands:")
	fmt.Fprintln(w, "  init [path]")
	fmt.Fprintln(w, "  version")
	fmt.Fprintln(w, "  config")
	fmt.Fprintln(w, "  deploy [--dry-run]")
	fmt.Fprintln(w, "  rollback [version]")
	fmt.Fprintln(w, "  stop")
	fmt.Fprintln(w, "  start")
	fmt.Fprintln(w, "  restart")
	fmt.Fprintln(w, "  remove")
	fmt.Fprintln(w, "  prune")
	fmt.Fprintln(w, "  backup")
	fmt.Fprintln(w, "  restore <local-file>")
	fmt.Fprintln(w, "  sweep")
	fmt.Fprintln(w, "  sync")
	fmt.Fprintln(w, "  lock <status|release>")
	fmt.Fprintln(w, "  proxy <boot|start|stop|restart|upgrade|remove [--purge]|logs [--follow] [--lines N]|details>")
	fmt.Fprintln(w, "  status")
	fmt.Fprintln(w, "  logs [--follow] [--lines N]")
	fmt.Fprintln(w, "  app exec <command>")
	fmt.Fprintln(w, "  app containers")
	fmt.Fprintln(w, "  app maintenance [--message <msg>] [--drain-timeout <duration>]")
	fmt.Fprintln(w, "  app live")
	fmt.Fprintln(w, "  accessory <boot|stop|start|restart|remove|logs|exec> <name> [args...]")
}
