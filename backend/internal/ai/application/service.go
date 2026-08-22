// Package application coordinates the authenticated, pull-based AI queue.
package application

import (
	"context"
	"crypto/sha256"
	"errors"
	"time"

	"lidradar/backend/internal/ai/domain"
)

var (
	ErrUnauthorized = errors.New("invalid AI node credentials")
	ErrInvalid      = errors.New("invalid AI request")
	ErrNotFound     = errors.New("AI resource not found")
	ErrLeaseLost    = errors.New("AI job lease is not owned by node")
)

const DefaultLease = 120 * time.Second

type IDs interface{ NewID() string }
type Store interface {
	RegisterNode(context.Context, domain.Node) error
	AuthenticateNode(context.Context, string, [32]byte) (domain.Node, bool, error)
	Heartbeat(context.Context, string, time.Time, time.Time) error
	Enqueue(context.Context, domain.Job) error
	Claim(context.Context, string, time.Time, time.Time) (domain.Job, bool, error)
	Start(context.Context, domain.Run) error
	Complete(context.Context, string, string, string, domain.JobStatus, domain.ApplicationStatus, string, int64, string, time.Time) (domain.Run, error)
}

type Service struct {
	store Store
	ids   IDs
	now   func() time.Time
	lease time.Duration
}

func NewService(store Store, ids IDs, now func() time.Time, lease time.Duration) Service {
	if lease <= 0 {
		lease = DefaultLease
	}
	return Service{store: store, ids: ids, now: now, lease: lease}
}

// RegisterNode returns the only plaintext copy of the secret. Persistence gets
// only its SHA-256 digest, allowing later rotation or revocation.
func (s Service) RegisterNode(ctx context.Context, name, secret string) (domain.Node, error) {
	if s.store == nil || s.ids == nil || s.now == nil || name == "" || secret == "" {
		return domain.Node{}, ErrInvalid
	}
	n := domain.Node{ID: s.ids.NewID(), Name: name, SecretHash: sha256.Sum256([]byte(secret)), Status: domain.NodeOffline, CreatedAt: s.now().UTC()}
	return n, s.store.RegisterNode(ctx, n)
}

func (s Service) authenticate(ctx context.Context, id, secret string) error {
	if id == "" || secret == "" || s.store == nil {
		return ErrUnauthorized
	}
	_, ok, err := s.store.AuthenticateNode(ctx, id, sha256.Sum256([]byte(secret)))
	if err != nil {
		return err
	}
	if !ok {
		return ErrUnauthorized
	}
	return nil
}

func (s Service) Heartbeat(ctx context.Context, id, secret string) error {
	if err := s.authenticate(ctx, id, secret); err != nil {
		return err
	}
	now := s.now().UTC()
	return s.store.Heartbeat(ctx, id, now, now.Add(s.lease))
}

type EnqueueCommand struct {
	TenantID, ConversationID, Prompt, AnalysisThroughMessageID string
	BaseConversationRevision                                   int64
}

func (s Service) Enqueue(ctx context.Context, c EnqueueCommand) (domain.Job, error) {
	if s.store == nil || s.ids == nil || s.now == nil || c.TenantID == "" || c.ConversationID == "" || c.Prompt == "" || c.AnalysisThroughMessageID == "" || c.BaseConversationRevision < 1 {
		return domain.Job{}, ErrInvalid
	}
	now := s.now().UTC()
	j := domain.Job{ID: s.ids.NewID(), TenantID: c.TenantID, ConversationID: c.ConversationID, Prompt: c.Prompt, BaseConversationRevision: c.BaseConversationRevision, AnalysisThroughMessageID: c.AnalysisThroughMessageID, Status: domain.JobQueued, CreatedAt: now, UpdatedAt: now}
	return j, s.store.Enqueue(ctx, j)
}

func (s Service) Claim(ctx context.Context, id, secret string) (domain.Job, bool, error) {
	if err := s.authenticate(ctx, id, secret); err != nil {
		return domain.Job{}, false, err
	}
	now := s.now().UTC()
	return s.store.Claim(ctx, id, now, now.Add(s.lease))
}

func (s Service) Started(ctx context.Context, id, secret, jobID string) (domain.Run, error) {
	if err := s.authenticate(ctx, id, secret); err != nil {
		return domain.Run{}, err
	}
	if jobID == "" {
		return domain.Run{}, ErrInvalid
	}
	r := domain.Run{ID: s.ids.NewID(), JobID: jobID, NodeID: id, Status: domain.JobRunning, ApplicationStatus: domain.ApplicationPending, StartedAt: s.now().UTC()}
	return r, s.store.Start(ctx, r)
}

func (s Service) Complete(ctx context.Context, id, secret, jobID, runID, output string, currentRevision int64, currentMessageID string) (domain.Run, error) {
	if err := s.authenticate(ctx, id, secret); err != nil {
		return domain.Run{}, err
	}
	status := domain.ApplicationApplied
	if output == "" {
		status = domain.ApplicationRejected
	}
	return s.store.Complete(ctx, id, jobID, runID, domain.JobSucceeded, status, output, currentRevision, currentMessageID, s.now().UTC())
}

func (s Service) Failed(ctx context.Context, id, secret, jobID, runID, reason string) (domain.Run, error) {
	if err := s.authenticate(ctx, id, secret); err != nil {
		return domain.Run{}, err
	}
	if reason == "" {
		return domain.Run{}, ErrInvalid
	}
	return s.store.Complete(ctx, id, jobID, runID, domain.JobFailed, domain.ApplicationRejected, reason, 0, "", s.now().UTC())
}
