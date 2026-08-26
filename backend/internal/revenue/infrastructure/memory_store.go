// Package infrastructure предоставляет адаптеры модуля выручки.
package infrastructure

import (
	"context"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"lidradar/backend/internal/revenue/application"
	"lidradar/backend/internal/revenue/domain"
)

type idempotency struct {
	hash   [32]byte
	result application.Confirmation
}

// MemoryStore — внутрипроцессный испытательный адаптер. Рабочим командам
// нельзя использовать его: выручка требует транзакционного PostgreSQL.
type MemoryStore struct {
	mu                       sync.Mutex
	opportunities            map[string]bool
	risks, actions, outcomes map[string]application.RelatedFact
	confirmations            []application.Confirmation
	idempotency              map[string]idempotency
	audits                   []application.AuditRecord
}

func NewTestMemoryStore() *MemoryStore {
	return &MemoryStore{
		opportunities: map[string]bool{}, risks: map[string]application.RelatedFact{},
		actions: map[string]application.RelatedFact{}, outcomes: map[string]application.RelatedFact{},
		idempotency: map[string]idempotency{},
	}
}

func scoped(tenant, id string) string { return tenant + "\x00" + id }

func (store *MemoryStore) AddOpportunity(tenant, id string) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.opportunities[scoped(tenant, id)] = true
}

func (store *MemoryStore) AddRisk(tenant, id string, fact application.RelatedFact) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.risks[scoped(tenant, id)] = fact
}

func (store *MemoryStore) AddAction(tenant, id string, fact application.RelatedFact) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.actions[scoped(tenant, id)] = fact
}

func (store *MemoryStore) AddOutcome(tenant, id string, fact application.RelatedFact) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.outcomes[scoped(tenant, id)] = fact
}

func (store *MemoryStore) Confirm(
	_ context.Context,
	confirmation application.Confirmation,
	key string,
	hash [32]byte,
	audit application.AuditRecord,
	window time.Duration,
) (application.Confirmation, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	idempotencyKey := scoped(confirmation.Event.TenantID, key)
	if previous, exists := store.idempotency[idempotencyKey]; exists {
		if previous.hash != hash {
			return application.Confirmation{}, false, application.ErrConflict
		}
		return previous.result, false, nil
	}
	if !store.opportunities[scoped(confirmation.Event.TenantID, confirmation.Event.OpportunityID)] {
		return application.Confirmation{}, false, application.ErrNotFound
	}
	if confirmation.Attribution.Type == domain.AttributionRecovered {
		facts := []application.RelatedFact{
			store.risks[scoped(confirmation.Event.TenantID, confirmation.Attribution.RiskID)],
			store.actions[scoped(confirmation.Event.TenantID, confirmation.Attribution.ActionID)],
			store.outcomes[scoped(confirmation.Event.TenantID, confirmation.Attribution.OutcomeID)],
		}
		for _, fact := range facts {
			if fact.OpportunityID == "" {
				return application.Confirmation{}, false, application.ErrNotFound
			}
			age := confirmation.Event.ConfirmedAt.Sub(fact.At)
			if fact.OpportunityID != confirmation.Event.OpportunityID || fact.At.IsZero() || age < 0 || age > window {
				return application.Confirmation{}, false, application.ErrInvalid
			}
		}
		action := facts[1]
		if action.RiskID != "" && action.RiskID != confirmation.Attribution.RiskID {
			return application.Confirmation{}, false, application.ErrInvalid
		}
	}
	store.confirmations = append(store.confirmations, confirmation)
	store.audits = append(store.audits, audit)
	store.idempotency[idempotencyKey] = idempotency{hash: hash, result: confirmation}
	return confirmation, true, nil
}

func (store *MemoryStore) ConfirmedRecovered(_ context.Context, tenant, currency string) (domain.Money, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	total := decimal.Zero
	for _, confirmation := range store.confirmations {
		if confirmation.Event.TenantID == tenant && confirmation.Event.Currency == currency &&
			confirmation.Event.Status == domain.StatusConfirmed &&
			confirmation.Attribution.Type == domain.AttributionRecovered {
			total = total.Add(confirmation.Event.Amount.Decimal())
		}
	}
	return domain.ParseNonNegativeMoney(total.StringFixed(2))
}

func (store *MemoryStore) Confirmations() []application.Confirmation {
	store.mu.Lock()
	defer store.mu.Unlock()
	return append([]application.Confirmation(nil), store.confirmations...)
}

func (store *MemoryStore) Audits() []application.AuditRecord {
	store.mu.Lock()
	defer store.mu.Unlock()
	return append([]application.AuditRecord(nil), store.audits...)
}

var _ application.Store = (*MemoryStore)(nil)
