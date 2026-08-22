// Package infrastructure contains replaceable AI persistence and inference adapters.
package infrastructure

import (
	"context"
	"crypto/subtle"
	"sync"
	"time"

	"lidradar/backend/internal/ai/application"
	"lidradar/backend/internal/ai/domain"
)

type MemoryStore struct {
	mu    sync.Mutex
	nodes map[string]domain.Node
	jobs  map[string]domain.Job
	order []string
	runs  map[string]domain.Run
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{nodes: map[string]domain.Node{}, jobs: map[string]domain.Job{}, runs: map[string]domain.Run{}}
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
	s.runs[r.ID] = r
	return nil
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
