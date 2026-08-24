// Command scheduler enqueues scheduled LidRadar work.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"lidradar/backend/platform/bootstrap"
	platformruntime "lidradar/backend/platform/runtime"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	os.Exit(bootstrap.Run(ctx, "lidradar-scheduler", os.Stderr, platformruntime.WaitForDatabase))
}
