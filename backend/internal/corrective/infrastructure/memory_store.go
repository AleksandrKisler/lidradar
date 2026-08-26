// Package infrastructure содержит адаптеры хранения корректирующих фактов.
package infrastructure

import (
	"context"
	"sync"

	"lidradar/backend/internal/corrective/application"
	"lidradar/backend/internal/corrective/domain"
)

type idem struct {
	hash    [32]byte
	action  *domain.Action
	outcome *domain.Outcome
}

type memoryRisk struct {
	opportunityID string
	riskType      string
}

// MemoryStore — внутрипроцессный испытательный адаптер. Рабочим командам нельзя
// использовать его: корректирующие факты требуют транзакционного PostgreSQL.
type MemoryStore struct {
	mu              sync.Mutex
	risks           map[string]memoryRisk
	opportunities   map[string]bool
	recommendations map[string]domain.Recommendation
	actions         []domain.Action
	outcomes        []domain.Outcome
	idempotency     map[string]idem
	audits          []application.AuditRecord
}

func NewTestMemoryStore() *MemoryStore {
	return &MemoryStore{risks: map[string]memoryRisk{}, opportunities: map[string]bool{}, recommendations: map[string]domain.Recommendation{}, idempotency: map[string]idem{}}
}
func scoped(tenant, id string) string { return tenant + "\x00" + id }
func (s *MemoryStore) AddRisk(tenant, risk, opportunity string) {
	s.AddRiskType(tenant, risk, opportunity, "NO_RESPONSE")
}
func (s *MemoryStore) AddRiskType(tenant, risk, opportunity, riskType string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.risks[scoped(tenant, risk)] = memoryRisk{opportunityID: opportunity, riskType: riskType}
	s.opportunities[scoped(tenant, opportunity)] = true
}
func (s *MemoryStore) Risk(_ context.Context, tenant, risk string) (application.RiskReference, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.risks[scoped(tenant, risk)]
	return application.RiskReference{OpportunityID: v.opportunityID, Type: v.riskType}, ok, nil
}
func (s *MemoryStore) OpportunityExists(_ context.Context, tenant, opportunity string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opportunities[scoped(tenant, opportunity)], nil
}
func (s *MemoryStore) EnsureRecommendation(_ context.Context, r domain.Recommendation) (domain.Recommendation, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := scoped(r.TenantID, r.RiskID)
	if old, ok := s.recommendations[k]; ok {
		return old, false, nil
	}
	s.recommendations[k] = r
	return r, true, nil
}
func (s *MemoryStore) AppendAction(_ context.Context, a domain.Action, key string, hash [32]byte, audit application.AuditRecord) (domain.Action, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := scoped(a.TenantID, "action:"+key)
	if old, ok := s.idempotency[k]; ok {
		if old.hash != hash {
			return domain.Action{}, false, application.ErrConflict
		}
		return *old.action, false, nil
	}
	s.actions = append(s.actions, a)
	s.audits = append(s.audits, audit)
	copy := a
	s.idempotency[k] = idem{hash: hash, action: &copy}
	return a, true, nil
}
func (s *MemoryStore) AppendOutcome(_ context.Context, o domain.Outcome, key string, hash [32]byte, audit application.AuditRecord) (domain.Outcome, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := scoped(o.TenantID, "outcome:"+key)
	if old, ok := s.idempotency[k]; ok {
		if old.hash != hash {
			return domain.Outcome{}, false, application.ErrConflict
		}
		return *old.outcome, false, nil
	}
	s.outcomes = append(s.outcomes, o)
	s.audits = append(s.audits, audit)
	copy := o
	s.idempotency[k] = idem{hash: hash, outcome: &copy}
	return o, true, nil
}
func (s *MemoryStore) Actions() []domain.Action {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.Action(nil), s.actions...)
}
func (s *MemoryStore) Outcomes() []domain.Outcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.Outcome(nil), s.outcomes...)
}
func (s *MemoryStore) Audits() []application.AuditRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]application.AuditRecord(nil), s.audits...)
}
