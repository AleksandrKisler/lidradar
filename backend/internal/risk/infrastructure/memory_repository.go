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

// MemoryRepository — потокобезопасный испытательный адаптер. Он намеренно
// исключён из рабочих команд: производственным источником истины остаётся
// PostgreSQL.
type MemoryRepository struct {
	mu    sync.RWMutex
	items map[string]domain.Risk
}

func NewTestMemoryRepository() *MemoryRepository {
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
			Severity: candidate.Severity, PolicyVersion: candidate.PolicyVersion,
			ReasonCode: candidate.ReasonCode, Reason: candidate.Reason, DueAt: candidate.DueAt}
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

// List сохраняет упрощённый детерминированный порядок испытательного адаптера:
// CRITICAL перед HIGH, затем время обнаружения и ID. Полный коммерческий
// приоритет проверяется на PostgreSQL-проекции с Opportunity.
func (r *MemoryRepository) List(ctx context.Context, tenantID string, q application.ListQuery) (application.Page, error) {
	if err := ctx.Err(); err != nil {
		return application.Page{}, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	items := r.allForTenant(tenantID)
	filtered := items[:0]
	for _, item := range items {
		if (q.Status == "" || item.Status == q.Status) &&
			(q.LocationID == "" || item.LocationID == q.LocationID) &&
			(q.Severity == "" || item.Severity == q.Severity) &&
			(q.RiskType == "" || item.Type == q.RiskType) {
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

func (r *MemoryRepository) Summary(ctx context.Context, tenantID string, filters application.Filters) (application.Summary, error) {
	if err := ctx.Err(); err != nil {
		return application.Summary{}, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	summary := application.Summary{PotentialRevenue: "0.00", ConfirmedRecoveredRevenue: "0.00"}
	for _, risk := range r.items {
		if risk.TenantID == tenantID && risk.Active() &&
			(filters.LocationID == "" || risk.LocationID == filters.LocationID) &&
			(filters.Severity == "" || risk.Severity == filters.Severity) &&
			(filters.RiskType == "" || risk.Type == filters.RiskType) {
			summary.OpenRisks++
			if risk.Severity == domain.SeverityCritical {
				summary.CriticalRisks++
			}
		}
	}
	return summary, nil
}

func (r *MemoryRepository) Acknowledge(ctx context.Context, tenantID, riskID string, at time.Time) (application.Mutation, error) {
	return r.mutateByID(ctx, tenantID, riskID, func(risk *domain.Risk) error { return risk.Acknowledge(at) })
}
func (r *MemoryRepository) Resolve(ctx context.Context, tenantID, riskID string, at time.Time) (application.Mutation, error) {
	return r.mutateByID(ctx, tenantID, riskID, func(risk *domain.Risk) error { return risk.Resolve(at) })
}
func (r *MemoryRepository) mutateByID(ctx context.Context, tenantID, riskID string, mutate func(*domain.Risk) error) (application.Mutation, error) {
	if err := ctx.Err(); err != nil {
		return application.Mutation{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, risk := range r.items {
		if risk.TenantID == tenantID && risk.ID == riskID {
			before := risk.Status
			if err := mutate(&risk); err != nil {
				return application.Mutation{}, err
			}
			r.items[key] = risk
			return application.Mutation{Risk: risk, Found: true, Changed: before != risk.Status}, nil
		}
	}
	return application.Mutation{}, nil
}

var _ domain.Repository = (*MemoryRepository)(nil)
var _ application.RadarStore = (*MemoryRepository)(nil)
