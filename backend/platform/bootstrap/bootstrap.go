// Package bootstrap provides the minimal lifecycle shared by LidRadar runtime
// commands. Configuration, logging, and runtime-specific behavior are added by
// their owning platform packages rather than being hidden in command mains.
package bootstrap

import (
	"context"
	"fmt"
	"io"
	"os"

	"lidradar/backend/platform/config"
)

// Workload is the process-specific function run by a LidRadar command.
type Workload func(context.Context, config.Config) error

// Run executes a workload and translates its result into a process exit code.
// Cancellation is a normal, graceful shutdown.
func Run(ctx context.Context, service string, stderr io.Writer, workload Workload) int {
	configuration, err := config.Load(os.LookupEnv)
	if err != nil {
		fmt.Fprintf(stderr, "%s: invalid configuration: %v\n", service, err)
		return 1
	}

	if err := workload(ctx, configuration); err != nil && ctx.Err() == nil {
		fmt.Fprintf(stderr, "%s: %v\n", service, err)
		return 1
	}

	return 0
}

// Wait keeps a long-running process alive until its context is cancelled.
func Wait(ctx context.Context, _ config.Config) error {
	<-ctx.Done()
	return ctx.Err()
}

// Complete is the workload for a command that has no long-running loop.
func Complete(context.Context, config.Config) error { return nil }
