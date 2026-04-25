package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gokamal/gocart/internal/config"
	"github.com/gokamal/gocart/internal/deploy"
	"github.com/gokamal/gocart/internal/state"
)

func Run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		printUsage(stdout)
		return nil
	}

	switch args[0] {
	case "config":
		cfg, err := config.Load("qifa.yml")
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
		path := "qifa.yml"
		if len(args) > 1 {
			path = args[1]
		}
		_, err := fmt.Fprintf(stdout, "wrote starter config to %s\n", path)
		if err != nil {
			return err
		}
		return config.WriteSample(path)
	case "deploy":
		return withRuntime(ctx, stdout, stderr, func(rt *runtime) error {
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
	case "sweep":
		return withRuntime(ctx, stdout, stderr, func(rt *runtime) error {
			return rt.deployer.SweepStaleContainers(ctx)
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
		return withRuntime(ctx, stdout, stderr, func(rt *runtime) error {
			return rt.deployer.Logs(ctx, stdout)
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
	cfg, err := config.Load("qifa.yml")
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
	fmt.Fprintln(w, "  config")
	fmt.Fprintln(w, "  deploy")
	fmt.Fprintln(w, "  rollback [version]")
	fmt.Fprintln(w, "  stop")
	fmt.Fprintln(w, "  start")
	fmt.Fprintln(w, "  restart")
	fmt.Fprintln(w, "  remove")
	fmt.Fprintln(w, "  prune")
	fmt.Fprintln(w, "  sweep")
	fmt.Fprintln(w, "  lock <status|release>")
	fmt.Fprintln(w, "  status")
	fmt.Fprintln(w, "  logs")
	fmt.Fprintln(w, "  app exec <command>")
	fmt.Fprintln(w, "  app containers")
	fmt.Fprintln(w, "  app maintenance [--message <msg>] [--drain-timeout <duration>]")
	fmt.Fprintln(w, "  app live")
	fmt.Fprintln(w, "  accessory <boot|stop|start|restart|remove|logs|exec> <name> [args...]")
}
