package main

import (
	"context"
	"fmt"
	"os"

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
	if err := app.Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
