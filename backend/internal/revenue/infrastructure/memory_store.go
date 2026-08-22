// Package infrastructure provides adapters for the revenue application.
package infrastructure

import (
	"context"
	"sync"

	"lidradar/backend/internal/revenue/application"
	"lidradar/backend/internal/revenue/domain"
)

type idempotency struct {
	hash   [32]byte
	result application.Confirmation
}
type MemoryStore struct {
	mu                       sync.Mutex
	opportunities            map[string]bool
	risks, actions, outcomes map[string]application.RelatedFact
	confirmations            []application.Confirmation
	idempotency              map[string]idempotency
	audits                   []application.AuditRecord
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{opportunities: map[string]bool{}, risks: map[string]application.RelatedFact{}, actions: map[string]application.RelatedFact{}, outcomes: map[string]application.RelatedFact{}, idempotency: map[string]idempotency{}}
}
func scoped(tenant, id string) string { return tenant + "\x00" + id }
func (s *MemoryStore) AddOpportunity(tenant, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opportunities[scoped(tenant, id)] = true
}
func (s *MemoryStore) AddRisk(tenant, id string, f application.RelatedFact) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.risks[scoped(tenant, id)] = f
}
func (s *MemoryStore) AddAction(tenant, id string, f application.RelatedFact) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.actions[scoped(tenant, id)] = f
}
func (s *MemoryStore) AddOutcome(tenant, id string, f application.RelatedFact) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outcomes[scoped(tenant, id)] = f
}
func (s *MemoryStore) OpportunityExists(_ context.Context, tenant, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opportunities[scoped(tenant, id)], nil
}
func lookup(values map[string]application.RelatedFact, tenant, id string) (application.RelatedFact, bool, error) {
	v, ok := values[scoped(tenant, id)]
	return v, ok, nil
}
func (s *MemoryStore) Risk(_ context.Context, t, id string) (application.RelatedFact, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return lookup(s.risks, t, id)
}
func (s *MemoryStore) Action(_ context.Context, t, id string) (application.RelatedFact, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return lookup(s.actions, t, id)
}
func (s *MemoryStore) Outcome(_ context.Context, t, id string) (application.RelatedFact, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return lookup(s.outcomes, t, id)
}
func (s *MemoryStore) Confirm(_ context.Context, c application.Confirmation, key string, hash [32]byte, audit application.AuditRecord) (application.Confirmation, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := scoped(c.Event.TenantID, key)
	if old, ok := s.idempotency[k]; ok {
		if old.hash != hash {
			return application.Confirmation{}, false, application.ErrConflict
		}
		return old.result, false, nil
	}
	// A single atomic critical section models the production transaction: event,
	// unique attribution, idempotency response, and audit are committed together.
	s.confirmations = append(s.confirmations, c)
	s.audits = append(s.audits, audit)
	s.idempotency[k] = idempotency{hash, c}
	return c, true, nil
}
func (s *MemoryStore) ConfirmedRecovered(_ context.Context, tenant, currency string) (domain.Money, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var cents int64
	for _, c := range s.confirmations {
		if c.Event.TenantID == tenant && c.Event.Currency == currency && c.Event.Status == "CONFIRMED" && c.Attribution.Type == domain.AttributionRecovered {
			cents += c.Event.Amount.Cents()
		}
	}
	return domain.NewMoneyFromCents(cents)
}
func (s *MemoryStore) Confirmations() []application.Confirmation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]application.Confirmation(nil), s.confirmations...)
}
func (s *MemoryStore) Audits() []application.AuditRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]application.AuditRecord(nil), s.audits...)
}
