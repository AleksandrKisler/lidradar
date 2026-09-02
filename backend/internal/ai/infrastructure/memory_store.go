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

// MemoryStore — внутрипроцессный испытательный адаптер. Рабочим командам нельзя
// использовать его: метаданные AI требуют долговечного хранения в PostgreSQL.
type MemoryStore struct {
	mu        sync.Mutex
	nodes     map[string]domain.Node
	allowed   map[string]struct{}
	nonces    map[string]time.Time
	jobs      map[string]domain.Job
	order     []string
	runs      map[string]domain.Run
	summaries map[string]domain.ConversationSummary
	snapshots map[string]domain.ConversationSnapshot
}

func NewTestMemoryStore() *MemoryStore {
	return &MemoryStore{
		nodes: map[string]domain.Node{}, nonces: map[string]time.Time{},
		allowed: map[string]struct{}{},
		jobs:    map[string]domain.Job{}, runs: map[string]domain.Run{},
		summaries: map[string]domain.ConversationSummary{}, snapshots: map[string]domain.ConversationSnapshot{},
	}
}

func (store *MemoryStore) RegisterNode(_ context.Context, node domain.Node) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.nodes[node.ID] = node
	store.allowed[nodeTenantKey(node.ID, node.TenantID)] = struct{}{}
	return nil
}

func (store *MemoryStore) AllowNodeTenant(_ context.Context, nodeID, tenantID string, _ time.Time) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, ok := store.nodes[nodeID]; !ok {
		return application.ErrNotFound
	}
	store.allowed[nodeTenantKey(nodeID, tenantID)] = struct{}{}
	return nil
}

func (store *MemoryStore) RotateNodeSecret(_ context.Context, id string, digest [32]byte, now time.Time) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	node, ok := store.nodes[id]
	if !ok {
		return application.ErrNotFound
	}
	if node.Status == domain.NodeRevoked {
		return application.ErrConflict
	}
	node.SecretHash = digest
	node.Status, node.AvailableSlots, node.ModelVersion = domain.NodeOffline, 0, ""
	node.UpdatedAt = now
	store.nodes[id] = node
	return nil
}

func (store *MemoryStore) RevokeNode(_ context.Context, id string, now time.Time) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	node, ok := store.nodes[id]
	if !ok {
		return application.ErrNotFound
	}
	if node.Status == domain.NodeRevoked {
		return nil
	}
	node.Status, node.AvailableSlots, node.ModelVersion = domain.NodeRevoked, 0, ""
	node.RevokedAt, node.UpdatedAt = now, now
	store.nodes[id] = node
	return nil
}

func (store *MemoryStore) AuthenticateNode(_ context.Context, id string, digest [32]byte) (domain.Node, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	node, ok := store.nodes[id]
	if !ok || subtle.ConstantTimeCompare(node.SecretHash[:], digest[:]) != 1 {
		return domain.Node{}, false, nil
	}
	return node, true, nil
}

func (store *MemoryStore) UseRequestNonce(_ context.Context, nodeID, requestID string, now, expiresAt time.Time) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	for key, expiry := range store.nonces {
		if !expiry.After(now) {
			delete(store.nonces, key)
		}
	}
	key := nodeID + ":" + requestID
	if _, exists := store.nonces[key]; exists {
		return application.ErrReplay
	}
	store.nonces[key] = expiresAt
	return nil
}

func (store *MemoryStore) Heartbeat(
	_ context.Context,
	id string,
	status domain.NodeStatus,
	modelVersion string,
	availableSlots int,
	now, leaseUntil time.Time,
) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	node, ok := store.nodes[id]
	if !ok {
		return application.ErrNotFound
	}
	if node.Status == domain.NodeRevoked {
		return application.ErrUnauthorized
	}
	node.Status, node.ModelVersion, node.AvailableSlots = status, modelVersion, availableSlots
	node.LastHeartbeatAt, node.UpdatedAt = now, now
	store.nodes[id] = node
	for jobID, job := range store.jobs {
		if (job.Status == domain.JobLeased || job.Status == domain.JobRunning) &&
			job.ClaimedBy == id && job.LeaseUntil.After(now) {
			job.LeaseUntil, job.UpdatedAt = leaseUntil, now
			store.jobs[jobID] = job
		}
	}
	return nil
}

func (store *MemoryStore) Enqueue(_ context.Context, job domain.Job) (domain.Job, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, existing := range store.jobs {
		if existing.TenantID == job.TenantID && existing.JobType == job.JobType &&
			existing.EntityType == job.EntityType && existing.ConversationID == job.ConversationID &&
			existing.BaseConversationRevision == job.BaseConversationRevision &&
			existing.ModelVersion == job.ModelVersion && existing.SchemaVersion == job.SchemaVersion &&
			existing.PromptVersion == job.PromptVersion {
			if existing.Prompt != job.Prompt {
				return domain.Job{}, application.ErrConflict
			}
			return existing, nil
		}
	}
	store.jobs[job.ID] = job
	store.order = append(store.order, job.ID)
	store.snapshots[snapshotKey(job.TenantID, job.ConversationID)] = domain.ConversationSnapshot{
		Revision: job.BaseConversationRevision, LastMessageID: job.AnalysisThroughMessageID,
	}
	return job, nil
}

func (store *MemoryStore) Claim(_ context.Context, nodeID string, now, leaseUntil time.Time) (domain.Job, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	node, ok := store.nodes[nodeID]
	if !ok {
		return domain.Job{}, false, application.ErrNotFound
	}
	if node.Status != domain.NodeReady || node.AvailableSlots < 1 ||
		node.LastHeartbeatAt.IsZero() || now.Sub(node.LastHeartbeatAt) > application.NodeUnavailableAfter {
		return domain.Job{}, false, nil
	}
	for _, jobID := range store.order {
		job := store.jobs[jobID]
		if _, allowed := store.allowed[nodeTenantKey(nodeID, job.TenantID)]; !allowed {
			continue
		}
		if job.ModelVersion != node.ModelVersion {
			continue
		}
		available := (job.Status == domain.JobPending || job.Status == domain.JobRetry) && !job.AvailableAt.After(now)
		expired := (job.Status == domain.JobLeased || job.Status == domain.JobRunning) && !job.LeaseUntil.After(now)
		if !available && !expired {
			continue
		}
		if job.Attempts >= job.MaxAttempts {
			job.Status, job.LastErrorCode = domain.JobDead, "LEASE_EXPIRED_MAX_ATTEMPTS"
			job.ClaimedBy, job.LeaseUntil, job.CompletedAt, job.UpdatedAt = "", time.Time{}, now, now
			store.jobs[jobID] = job
			continue
		}
		if expired {
			for runID, run := range store.runs {
				if run.JobID == job.ID && run.Status == domain.RunRunning {
					run.Status, run.ApplicationStatus = domain.RunFailed, domain.ApplicationRejected
					run.ErrorCode, run.CompletedAt = "LEASE_EXPIRED", now
					store.runs[runID] = run
				}
			}
		}
		job.Status, job.ClaimedBy, job.LeaseUntil = domain.JobLeased, nodeID, leaseUntil
		job.Attempts++
		job.UpdatedAt = now
		store.jobs[jobID] = job
		return job, true, nil
	}
	return domain.Job{}, false, nil
}

func (store *MemoryStore) Start(_ context.Context, run domain.Run, now time.Time) (domain.Run, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	job, ok := store.jobs[run.JobID]
	if !ok {
		return domain.Run{}, application.ErrNotFound
	}
	if job.ClaimedBy != run.NodeID || (job.Status != domain.JobLeased && job.Status != domain.JobRunning) || !job.LeaseUntil.After(now) {
		return domain.Run{}, application.ErrLeaseLost
	}
	for _, existing := range store.runs {
		if existing.JobID == job.ID && existing.Status == domain.RunRunning {
			if existing.NodeID == run.NodeID {
				return existing, nil
			}
			return domain.Run{}, application.ErrLeaseLost
		}
	}
	run.BaseConversationRevision = job.BaseConversationRevision
	run.AnalysisThroughMessageID = job.AnalysisThroughMessageID
	run.TenantID, run.ConversationID = job.TenantID, job.ConversationID
	run.ModelVersion, run.PromptVersion, run.SchemaVersion = job.ModelVersion, job.PromptVersion, job.SchemaVersion
	store.runs[run.ID] = run
	job.Status, job.UpdatedAt = domain.JobRunning, now
	store.jobs[job.ID] = job
	return run, nil
}

func (store *MemoryStore) Run(_ context.Context, id string) (domain.Run, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	run, ok := store.runs[id]
	if !ok {
		return domain.Run{}, application.ErrNotFound
	}
	return run, nil
}

func (store *MemoryStore) ConversationSnapshot(_ context.Context, tenantID, conversationID string) (domain.ConversationSnapshot, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	snapshot, ok := store.snapshots[snapshotKey(tenantID, conversationID)]
	if !ok {
		return domain.ConversationSnapshot{}, application.ErrNotFound
	}
	return snapshot, nil
}

func (store *MemoryStore) Finalize(_ context.Context, final application.Finalization) (domain.Run, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	job, ok := store.jobs[final.JobID]
	if !ok {
		return domain.Run{}, application.ErrNotFound
	}
	run, ok := store.runs[final.RunID]
	if !ok {
		return domain.Run{}, application.ErrNotFound
	}
	if job.ClaimedBy != final.NodeID || run.NodeID != final.NodeID || job.Status != domain.JobRunning || !job.LeaseUntil.After(final.CompletedAt) {
		return domain.Run{}, application.ErrLeaseLost
	}
	if final.Summary != nil {
		snapshot, exists := store.snapshots[snapshotKey(run.TenantID, run.ConversationID)]
		if !exists || snapshot.Revision != run.BaseConversationRevision || snapshot.LastMessageID != run.AnalysisThroughMessageID {
			return domain.Run{}, application.ErrFreshnessChanged
		}
	}
	run.Status, run.ApplicationStatus, run.CompletedAt = final.RunStatus, final.ApplicationStatus, final.CompletedAt
	run.Output, run.ErrorCode, run.ValidationError = final.Output, final.ErrorCode, final.ValidationError
	store.runs[run.ID] = run
	if final.RunStatus == domain.RunSucceeded {
		job.Status, job.CompletedAt = domain.JobSucceeded, final.CompletedAt
	} else if job.Attempts < job.MaxAttempts {
		job.Status, job.AvailableAt = domain.JobRetry, final.CompletedAt.Add(5*time.Second)
		job.LastErrorCode = final.ErrorCode
	} else {
		job.Status, job.CompletedAt, job.LastErrorCode = domain.JobDead, final.CompletedAt, final.ErrorCode
	}
	job.ClaimedBy, job.LeaseUntil, job.UpdatedAt = "", time.Time{}, final.CompletedAt
	store.jobs[job.ID] = job
	if final.Summary != nil {
		store.summaries[snapshotKey(final.Summary.TenantID, final.Summary.ConversationID)] = *final.Summary
	}
	if final.Replacement != nil {
		if _, exists := store.jobs[final.Replacement.ID]; !exists {
			store.jobs[final.Replacement.ID] = *final.Replacement
			store.order = append(store.order, final.Replacement.ID)
		}
	}
	return run, nil
}

func (store *MemoryStore) Summary(tenantID, conversationID string) (domain.ConversationSummary, bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	value, ok := store.summaries[snapshotKey(tenantID, conversationID)]
	return value, ok
}

func (store *MemoryStore) Job(id string) (domain.Job, bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	job, ok := store.jobs[id]
	return job, ok
}

func (store *MemoryStore) SetConversationSnapshot(tenantID, conversationID string, snapshot domain.ConversationSnapshot) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.snapshots[snapshotKey(tenantID, conversationID)] = snapshot
}

func snapshotKey(tenantID, conversationID string) string { return tenantID + ":" + conversationID }
func nodeTenantKey(nodeID, tenantID string) string       { return nodeID + ":" + tenantID }
