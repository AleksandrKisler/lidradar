// Package infrastructure supplies adapters for Risk domain ports.
package infrastructure

import (
	"context"
	"encoding/base64"
	"sort"
	"sync"
	"time"

	"lidradar/backend/internal/risk/application"
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

func (r *MemoryRepository) allForTenant(tenantID string) []domain.Risk {
	items := make([]domain.Risk, 0)
	for _, risk := range r.items {
		if risk.TenantID == tenantID {
			items = append(items, risk)
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Active() != items[j].Active() {
			return items[i].Active()
		}
		if items[i].Severity != items[j].Severity {
			return items[i].Severity == domain.SeverityCritical
		}
		if !items[i].DetectedAt.Equal(items[j].DetectedAt) {
			return items[i].DetectedAt.Before(items[j].DetectedAt)
		}
		return items[i].ID < items[j].ID
	})
	return items
}

// List provides the same deterministic priority order required of the
// PostgreSQL Radar query: active, CRITICAL before HIGH, oldest first, then ID.
func (r *MemoryRepository) List(ctx context.Context, tenantID string, q application.ListQuery) (application.Page, error) {
	if err := ctx.Err(); err != nil {
		return application.Page{}, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	items := r.allForTenant(tenantID)
	filtered := items[:0]
	for _, item := range items {
		if q.Status == "" || item.Status == q.Status {
			filtered = append(filtered, item)
		}
	}
	start := 0
	if q.After != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(q.After)
		if err != nil {
			return application.Page{}, application.ErrInvalidCommand
		}
		found := false
		for i, item := range filtered {
			if item.ID == string(decoded) {
				start = i + 1
				found = true
				break
			}
		}
		if !found {
			return application.Page{}, application.ErrInvalidCommand
		}
	}
	end := start + q.Limit
	if end > len(filtered) {
		end = len(filtered)
	}
	page := application.Page{Items: make([]application.Detail, 0, end-start)}
	for _, item := range filtered[start:end] {
		page.Items = append(page.Items, application.Detail{Risk: item, Actions: []application.Action{}})
	}
	if end < len(filtered) {
		page.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(filtered[end-1].ID))
	}
	return page, nil
}

func (r *MemoryRepository) Get(ctx context.Context, tenantID, riskID string) (application.Detail, bool, error) {
	if err := ctx.Err(); err != nil {
		return application.Detail{}, false, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, risk := range r.items {
		if risk.TenantID == tenantID && risk.ID == riskID {
			return application.Detail{Risk: risk, Actions: []application.Action{}}, true, nil
		}
	}
	return application.Detail{}, false, nil
}

func (r *MemoryRepository) Summary(ctx context.Context, tenantID string) (application.Summary, error) {
	if err := ctx.Err(); err != nil {
		return application.Summary{}, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	summary := application.Summary{PotentialRevenue: "0", ConfirmedRecoveredRevenue: "0"}
	for _, risk := range r.items {
		if risk.TenantID == tenantID && risk.Active() {
			summary.OpenRisks++
			if risk.Severity == domain.SeverityCritical {
				summary.CriticalRisks++
			}
		}
	}
	return summary, nil
}

func (r *MemoryRepository) Acknowledge(ctx context.Context, tenantID, riskID string, at time.Time) (domain.Risk, bool, error) {
	return r.mutateByID(ctx, tenantID, riskID, func(risk *domain.Risk) error { return risk.Acknowledge(at) })
}
func (r *MemoryRepository) Resolve(ctx context.Context, tenantID, riskID string, at time.Time) (domain.Risk, bool, error) {
	return r.mutateByID(ctx, tenantID, riskID, func(risk *domain.Risk) error { return risk.Resolve(at) })
}
func (r *MemoryRepository) mutateByID(ctx context.Context, tenantID, riskID string, mutate func(*domain.Risk) error) (domain.Risk, bool, error) {
	if err := ctx.Err(); err != nil {
		return domain.Risk{}, false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, risk := range r.items {
		if risk.TenantID == tenantID && risk.ID == riskID {
			if err := mutate(&risk); err != nil {
				return domain.Risk{}, false, err
			}
			r.items[key] = risk
			return risk, true, nil
		}
	}
	return domain.Risk{}, false, nil
}

var _ domain.Repository = (*MemoryRepository)(nil)
var _ application.RadarStore = (*MemoryRepository)(nil)
