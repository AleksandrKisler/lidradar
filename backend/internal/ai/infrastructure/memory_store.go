// Package infrastructure contains replaceable AI persistence and inference adapters.
package infrastructure

import (
	"context"
	"crypto/subtle"
	"fmt"
	"sync"
	"time"

	"lidradar/backend/internal/ai/application"
	"lidradar/backend/internal/ai/domain"
)

type MemoryStore struct {
	mu        sync.Mutex
	nodes     map[string]domain.Node
	jobs      map[string]domain.Job
	order     []string
	runs      map[string]domain.Run
	summaries map[string]domain.ConversationSummary
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{nodes: map[string]domain.Node{}, jobs: map[string]domain.Job{}, runs: map[string]domain.Run{}, summaries: map[string]domain.ConversationSummary{}}
}
func (s *MemoryStore) RegisterNode(_ context.Context, n domain.Node) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nodes[n.ID] = n
	return nil
}
func (s *MemoryStore) AuthenticateNode(_ context.Context, id string, h [32]byte) (domain.Node, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.nodes[id]
	if !ok || subtle.ConstantTimeCompare(n.SecretHash[:], h[:]) != 1 {
		return domain.Node{}, false, nil
	}
	return n, true, nil
}
func (s *MemoryStore) Heartbeat(_ context.Context, id string, now, lease time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.nodes[id]
	if !ok {
		return application.ErrNotFound
	}
	n.Status = domain.NodeReady
	n.LastHeartbeatAt = now
	s.nodes[id] = n
	for k, j := range s.jobs {
		if j.Status == domain.JobRunning && j.ClaimedBy == id && j.LeaseUntil.After(now) {
			j.LeaseUntil = lease
			j.UpdatedAt = now
			s.jobs[k] = j
		}
	}
	return nil
}
func (s *MemoryStore) Enqueue(_ context.Context, j domain.Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[j.ID] = j
	s.order = append(s.order, j.ID)
	return nil
}
func (s *MemoryStore) Claim(_ context.Context, node string, now, lease time.Time) (domain.Job, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range s.order {
		j := s.jobs[id]
		if j.Status == domain.JobQueued || (j.Status == domain.JobRunning && !j.LeaseUntil.After(now)) {
			j.Status = domain.JobRunning
			j.ClaimedBy = node
			j.LeaseUntil = lease
			j.UpdatedAt = now
			s.jobs[id] = j
			return j, true, nil
		}
	}
	return domain.Job{}, false, nil
}
func (s *MemoryStore) Start(_ context.Context, r domain.Run) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[r.JobID]
	if !ok {
		return application.ErrNotFound
	}
	if j.ClaimedBy != r.NodeID || j.Status != domain.JobRunning {
		return application.ErrLeaseLost
	}
	r.BaseConversationRevision = j.BaseConversationRevision
	r.AnalysisThroughMessageID = j.AnalysisThroughMessageID
	r.TenantID, r.ConversationID = j.TenantID, j.ConversationID
	r.ModelVersion, r.PromptVersion, r.SchemaVersion = j.ModelVersion, j.PromptVersion, j.SchemaVersion
	s.runs[r.ID] = r
	return nil
}
func (s *MemoryStore) Run(_ context.Context, id string) (domain.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[id]
	if !ok {
		return domain.Run{}, application.ErrNotFound
	}
	return r, nil
}
func (s *MemoryStore) RecordValidationError(_ context.Context, id, reason string) (domain.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[id]
	if !ok {
		return domain.Run{}, application.ErrNotFound
	}
	r.ValidationError = reason
	s.runs[id] = r
	return r, nil
}

func (s *MemoryStore) SaveSummary(_ context.Context, summary domain.ConversationSummary) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if summary.TenantID == "" || summary.ConversationID == "" {
		return application.ErrInvalid
	}
	s.summaries[summary.TenantID+":"+summary.ConversationID] = summary
	return nil
}

func (s *MemoryStore) RescheduleStale(_ context.Context, oldJobID string, revision int64, messageID string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.jobs[oldJobID]
	if !ok {
		return application.ErrNotFound
	}
	id := oldJobID + ":revision:" + fmt.Sprint(revision)
	if _, exists := s.jobs[id]; exists {
		return nil
	}
	old.ID, old.BaseConversationRevision, old.AnalysisThroughMessageID = id, revision, messageID
	old.Status, old.ClaimedBy, old.LeaseUntil, old.CreatedAt, old.UpdatedAt = domain.JobQueued, "", time.Time{}, now, now
	s.jobs[id] = old
	s.order = append(s.order, id)
	return nil
}

func (s *MemoryStore) Summary(tenantID, conversationID string) (domain.ConversationSummary, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.summaries[tenantID+":"+conversationID]
	return v, ok
}
func (s *MemoryStore) Complete(_ context.Context, node, jobID, runID string, status domain.JobStatus, app domain.ApplicationStatus, value string, currentRevision int64, currentMessage string, now time.Time) (domain.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[jobID]
	if !ok {
		return domain.Run{}, application.ErrNotFound
	}
	r, ok := s.runs[runID]
	if !ok {
		return domain.Run{}, application.ErrNotFound
	}
	if j.ClaimedBy != node || r.NodeID != node || j.Status != domain.JobRunning || !j.LeaseUntil.After(now) {
		return domain.Run{}, application.ErrLeaseLost
	}
	if status == domain.JobSucceeded && app == domain.ApplicationApplied && (r.BaseConversationRevision != currentRevision || r.AnalysisThroughMessageID != currentMessage) {
		app = domain.ApplicationStale
	}
	r.Status = status
	r.ApplicationStatus = app
	if status == domain.JobSucceeded {
		r.Output = value
	} else {
		r.Error = value
	}
	r.CompletedAt = now
	s.runs[runID] = r
	j.Status = status
	j.UpdatedAt = now
	s.jobs[jobID] = j
	return r, nil
}
func (s *MemoryStore) Job(id string) (domain.Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	return j, ok
}
