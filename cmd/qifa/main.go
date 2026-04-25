package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/gokamal/gocart/internal/app"
)

// These can be overridden at build time via `-ldflags "-X main.version=..."`.
// When unset, the runtime falls back to debug.ReadBuildInfo so `go install`
// and `go run` still produce a sensible answer.
var (
	version = ""
	commit  = ""
	date    = ""
)

func main() {
	app.SetVersion(version, commit, date)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	err := app.Run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
