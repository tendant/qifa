package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

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
		return withRuntime(ctx, stdout, stderr, func(rt *runtime) error {
			return rt.deployer.Rollback(ctx)
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
	case "status":
		return withRuntime(ctx, stdout, stderr, func(rt *runtime) error {
			return rt.deployer.Status(ctx, stdout)
		})
	case "logs":
		return withRuntime(ctx, stdout, stderr, func(rt *runtime) error {
			return rt.deployer.Logs(ctx, stdout)
		})
	case "app":
		if len(args) < 2 || args[1] != "exec" {
			return errors.New("usage: qifa app exec <command>")
		}
		if len(args) < 3 {
			return errors.New("usage: qifa app exec <command>")
		}
		command := strings.Join(args[2:], " ")
		return withRuntime(ctx, stdout, stderr, func(rt *runtime) error {
			return rt.deployer.Exec(ctx, command, stdout)
		})
	case "accessory":
		if len(args) < 3 {
			return errors.New("usage: qifa accessory <boot|logs> <name>")
		}
		name := args[2]
		return withRuntime(ctx, stdout, stderr, func(rt *runtime) error {
			switch args[1] {
			case "boot":
				return rt.deployer.AccessoryBoot(ctx, name)
			case "logs":
				return rt.deployer.AccessoryLogs(ctx, name, stdout)
			default:
				return errors.New("usage: qifa accessory <boot|logs> <name>")
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
	fmt.Fprintln(w, "  deploy")
	fmt.Fprintln(w, "  rollback")
	fmt.Fprintln(w, "  stop")
	fmt.Fprintln(w, "  start")
	fmt.Fprintln(w, "  restart")
	fmt.Fprintln(w, "  remove")
	fmt.Fprintln(w, "  prune")
	fmt.Fprintln(w, "  status")
	fmt.Fprintln(w, "  logs")
	fmt.Fprintln(w, "  app exec <command>")
	fmt.Fprintln(w, "  accessory boot <name>")
	fmt.Fprintln(w, "  accessory logs <name>")
}
