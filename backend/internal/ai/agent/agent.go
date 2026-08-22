// Package agent implements the disposable outbound AI worker loop.
package agent

import (
	"context"
	"errors"
	"time"

	"lidradar/backend/internal/ai/domain"
)

type Cloud interface {
	Heartbeat(context.Context) error
	Claim(context.Context) (domain.Job, bool, error)
	Started(context.Context, string) (string, error)
	Complete(context.Context, string, string, string, int64, string) error
	Failed(context.Context, string, string, string) error
}
type Provider interface {
	Infer(context.Context, string) (string, error)
}
type Runner struct {
	Cloud        Cloud
	Provider     Provider
	PollInterval time.Duration
}

// Run keeps no durable customer data. On restart it simply heartbeats and
// resumes polling; abandoned work is reclaimed by the cloud after lease expiry.
func (r Runner) Run(ctx context.Context) error {
	if r.Cloud == nil || r.Provider == nil {
		return errors.New("AI cloud and provider are required")
	}
	if r.PollInterval <= 0 {
		r.PollInterval = time.Second
	}
	t := time.NewTicker(r.PollInterval)
	defer t.Stop()
	for {
		if err := r.step(ctx); err != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}
func (r Runner) step(ctx context.Context) error {
	if err := r.Cloud.Heartbeat(ctx); err != nil {
		return err
	}
	j, ok, err := r.Cloud.Claim(ctx)
	if err != nil || !ok {
		return err
	}
	run, err := r.Cloud.Started(ctx, j.ID)
	if err != nil {
		return err
	}
	out, err := r.Provider.Infer(ctx, j.Prompt)
	if err != nil {
		return r.Cloud.Failed(ctx, j.ID, run, err.Error())
	}
	return r.Cloud.Complete(ctx, j.ID, run, out, j.BaseConversationRevision, j.AnalysisThroughMessageID)
}
