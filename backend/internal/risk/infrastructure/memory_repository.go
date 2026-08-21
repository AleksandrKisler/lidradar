// Package infrastructure supplies adapters for Risk domain ports.
package infrastructure

import (
	"context"
	"sync"
	"time"

	"lidradar/backend/internal/risk/domain"
)

// MemoryRepository is a concurrency-safe adapter for tests and local wiring.
// PostgreSQL remains the required production source of truth.
type MemoryRepository struct {
	mu    sync.RWMutex
	items map[string]domain.Risk
}

func NewMemoryRepository() *MemoryRepository {
	return &MemoryRepository{items: make(map[string]domain.Risk)}
}

func riskKey(tenantID, opportunityID string, riskType domain.Type) string {
	return tenantID + "\x00" + opportunityID + "\x00" + string(riskType)
}

func (r *MemoryRepository) UpsertActive(ctx context.Context, candidate domain.Risk) (domain.Risk, bool, error) {
	if err := ctx.Err(); err != nil {
		return domain.Risk{}, false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := riskKey(candidate.TenantID, candidate.OpportunityID, candidate.Type)
	if current, ok := r.items[key]; ok && current.Active() {
		finding := domain.Finding{TenantID: candidate.TenantID, OpportunityID: candidate.OpportunityID,
			LocationID: candidate.LocationID, TriggerMessageID: candidate.TriggerMessageID,
			Severity: candidate.Severity, PolicyVersion: candidate.PolicyVersion, Reason: candidate.Reason}
		if err := current.Refresh(finding, candidate.UpdatedAt); err != nil {
			return domain.Risk{}, false, err
		}
		r.items[key] = current
		return current, false, nil
	}
	r.items[key] = candidate
	return candidate, true, nil
}

func (r *MemoryRepository) FindActive(ctx context.Context, tenantID, opportunityID string, riskType domain.Type) (domain.Risk, bool, error) {
	if err := ctx.Err(); err != nil {
		return domain.Risk{}, false, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	risk, ok := r.items[riskKey(tenantID, opportunityID, riskType)]
	return risk, ok && risk.Active(), nil
}

func (r *MemoryRepository) ResolveActive(ctx context.Context, tenantID, opportunityID string, riskType domain.Type, at time.Time) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := riskKey(tenantID, opportunityID, riskType)
	risk, ok := r.items[key]
	if !ok || !risk.Active() {
		return false, nil
	}
	if err := risk.Resolve(at); err != nil {
		return false, err
	}
	r.items[key] = risk
	return true, nil
}

var _ domain.Repository = (*MemoryRepository)(nil)
