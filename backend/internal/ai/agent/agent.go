// Package agent implements the disposable outbound AI worker loop.
package agent

import (
	"context"
	"errors"
	"time"

	"lidradar/backend/internal/ai/domain"
)

type Cloud interface {
	Heartbeat(context.Context, domain.NodeStatus, string, int) error
	Claim(context.Context) (domain.Job, bool, error)
	Started(context.Context, string) (string, error)
	Complete(context.Context, string, string, string) error
	Failed(context.Context, string, string, string) error
}

type Provider interface {
	Ready(context.Context) error
	Infer(context.Context, string) (string, error)
}

type Runner struct {
	Cloud             Cloud
	Provider          Provider
	ModelVersion      string
	PollInterval      time.Duration
	HeartbeatInterval time.Duration
	OnError           func(error)
}

// Run keeps no durable customer data. Heartbeat continues while inference is
// running, renewing the current lease; max_inflight remains one.
func (runner Runner) Run(ctx context.Context) error {
	if runner.Cloud == nil || runner.Provider == nil || runner.ModelVersion == "" {
		return errors.New("AI cloud, provider and model version are required")
	}
	if runner.PollInterval <= 0 {
		runner.PollInterval = time.Second
	}
	if runner.HeartbeatInterval <= 0 {
		runner.HeartbeatInterval = 10 * time.Second
	}
	pollTicker := time.NewTicker(runner.PollInterval)
	heartbeatTicker := time.NewTicker(runner.HeartbeatInterval)
	defer pollTicker.Stop()
	defer heartbeatTicker.Stop()

	busy := false
	ready := runner.providerReady(ctx)
	runner.heartbeat(ctx, ready, busy)
	finished := make(chan error, 1)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-finished:
			busy = false
			if err != nil {
				runner.report(err)
			}
			runner.heartbeat(ctx, ready, busy)
		case <-heartbeatTicker.C:
			ready = runner.providerReady(ctx)
			runner.heartbeat(ctx, ready, busy)
		case <-pollTicker.C:
			if busy || !ready {
				continue
			}
			job, found, err := runner.Cloud.Claim(ctx)
			if err != nil {
				runner.report(err)
				continue
			}
			if !found {
				continue
			}
			busy = true
			runner.heartbeat(ctx, ready, busy)
			go func() { finished <- runner.process(ctx, job) }()
		}
	}
}

func (runner Runner) process(ctx context.Context, job domain.Job) error {
	runID, err := runner.Cloud.Started(ctx, job.ID)
	if err != nil {
		return err
	}
	output, err := runner.Provider.Infer(ctx, job.Prompt)
	if err != nil {
		if failedErr := runner.Cloud.Failed(ctx, job.ID, runID, "PROVIDER_INFERENCE_FAILED"); failedErr != nil {
			return failedErr
		}
		return err
	}
	return runner.Cloud.Complete(ctx, job.ID, runID, output)
}

func (runner Runner) providerReady(ctx context.Context) bool {
	if err := runner.Provider.Ready(ctx); err != nil {
		runner.report(err)
		return false
	}
	return true
}

func (runner Runner) heartbeat(ctx context.Context, ready, busy bool) {
	status := domain.NodeOffline
	slots := 0
	if ready {
		status = domain.NodeReady
		if !busy {
			slots = 1
		}
	}
	if err := runner.Cloud.Heartbeat(ctx, status, runner.ModelVersion, slots); err != nil {
		runner.report(err)
	}
}

func (runner Runner) report(err error) {
	if err != nil && runner.OnError != nil {
		runner.OnError(err)
	}
}
