// Command api runs the LidRadar HTTP API process.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"lidradar/backend/platform/bootstrap"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	os.Exit(bootstrap.Run(ctx, "lidradar-api", os.Stderr, bootstrap.Wait))
}
