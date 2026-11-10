// Command cortex provides deterministic code intelligence & change safety for AI coding agents.
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
	defer stop()

	os.Exit(cli.Execute(ctx))
}
