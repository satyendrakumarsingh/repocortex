// Command repocortex provides deterministic code intelligence & change safety for AI coding agents.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/satyendrakumarsingh/repocortex/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Execute(ctx)
	stop()
	os.Exit(code)
}
