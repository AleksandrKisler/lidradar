// Package bootstrap provides the minimal lifecycle shared by LidRadar runtime
// commands. Configuration, logging, and runtime-specific behavior are added by
// their owning platform packages rather than being hidden in command mains.
package bootstrap

import (
	"context"
	"fmt"
	"io"
)

// Workload is the process-specific function run by a LidRadar command.
type Workload func(context.Context) error

// Run executes a workload and translates its result into a process exit code.
// Cancellation is a normal, graceful shutdown.
func Run(ctx context.Context, service string, stderr io.Writer, workload Workload) int {
	if err := workload(ctx); err != nil && ctx.Err() == nil {
		fmt.Fprintf(stderr, "%s: %v\n", service, err)
		return 1
	}

	return 0
}

// Wait keeps a long-running process alive until its context is cancelled.
func Wait(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

// Complete is the workload for a command that has no long-running loop.
func Complete(context.Context) error { return nil }
