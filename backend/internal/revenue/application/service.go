// Package application coordinates revenue confirmation and attribution.
package application

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"time"

	"lidradar/backend/internal/revenue/domain"
)

var (
	ErrForbidden = errors.New("forbidden")
	ErrNotFound  = errors.New("resource not found")
	ErrInvalid   = errors.New("invalid request")
	ErrConflict  = errors.New("idempotency conflict")
)

const PermissionConfirm = "revenue.confirm"
const AttributionWindow = 30 * 24 * time.Hour

type Authorizer interface {
	Allowed(context.Context, string, string, string) (bool, error)
}
type IDs interface{ NewID() string }
type RelatedFact struct {
	OpportunityID string
	At            time.Time
}
type AuditRecord struct {
	TenantID, ActorID, Operation, ResourceID string
	At                                       time.Time
}
type Confirmation struct {
	Event       domain.RevenueEvent       `json:"revenue"`
	Attribution domain.RevenueAttribution `json:"attribution"`
}

type Store interface {
	OpportunityExists(context.Context, string, string) (bool, error)
	Risk(context.Context, string, string) (RelatedFact, bool, error)
	Action(context.Context, string, string) (RelatedFact, bool, error)
	Outcome(context.Context, string, string) (RelatedFact, bool, error)
	Confirm(context.Context, Confirmation, string, [32]byte, AuditRecord) (Confirmation, bool, error)
	ConfirmedRecovered(context.Context, string, string) (domain.Money, error)
}

type Service struct {
	store Store
	auth  Authorizer
	ids   IDs
	now   func() time.Time
}

func NewService(store Store, auth Authorizer, ids IDs, now func() time.Time) Service {
	return Service{store, auth, ids, now}
}

type ConfirmCommand struct {
	Amount, Currency            string
	Type                        domain.AttributionType
	RiskID, ActionID, OutcomeID string
}

func (s Service) Confirm(ctx context.Context, actor, tenant, opportunity, key string, cmd ConfirmCommand) (Confirmation, bool, error) {
	if actor == "" || tenant == "" || s.auth == nil {
		return Confirmation{}, false, ErrForbidden
	}
	ok, err := s.auth.Allowed(ctx, actor, tenant, PermissionConfirm)
	if err != nil {
		return Confirmation{}, false, err
	}
	if !ok {
		return Confirmation{}, false, ErrForbidden
	}
	if key == "" || opportunity == "" || s.store == nil || s.ids == nil || s.now == nil {
		return Confirmation{}, false, ErrInvalid
	}
	found, err := s.store.OpportunityExists(ctx, tenant, opportunity)
	if err != nil {
		return Confirmation{}, false, err
	}
	if !found {
		return Confirmation{}, false, ErrNotFound
	}
	now := s.now().UTC()
	event, err := domain.NewConfirmedEvent(s.ids.NewID(), tenant, opportunity, cmd.Amount, cmd.Currency, actor, now)
	if err != nil {
		return Confirmation{}, false, ErrInvalid
	}
	if cmd.Type == domain.AttributionRecovered {
		facts := []struct {
			id     string
			lookup func(context.Context, string, string) (RelatedFact, bool, error)
		}{{cmd.RiskID, s.store.Risk}, {cmd.ActionID, s.store.Action}, {cmd.OutcomeID, s.store.Outcome}}
		for _, related := range facts {
			if related.id == "" {
				return Confirmation{}, false, ErrInvalid
			}
			fact, exists, lookupErr := related.lookup(ctx, tenant, related.id)
			if lookupErr != nil {
				return Confirmation{}, false, lookupErr
			}
			if !exists {
				return Confirmation{}, false, ErrNotFound
			}
			if fact.OpportunityID != opportunity || fact.At.IsZero() || fact.At.After(now) || now.Sub(fact.At) > AttributionWindow {
				return Confirmation{}, false, ErrInvalid
			}
		}
	}
	attribution, err := domain.NewAttribution(s.ids.NewID(), event, cmd.Type, cmd.RiskID, cmd.ActionID, cmd.OutcomeID, now)
	if err != nil {
		return Confirmation{}, false, ErrInvalid
	}
	hash := sha256.Sum256([]byte(strings.Join([]string{opportunity, event.Amount.String(), event.Currency, string(cmd.Type), cmd.RiskID, cmd.ActionID, cmd.OutcomeID}, "\x00")))
	confirmation := Confirmation{event, attribution}
	return s.store.Confirm(ctx, confirmation, key, hash, AuditRecord{tenant, actor, "REVENUE_CONFIRMED", event.ID, now})
}

func (s Service) ConfirmedRecovered(ctx context.Context, actor, tenant, currency string) (domain.Money, error) {
	if actor == "" || tenant == "" || s.auth == nil || s.store == nil {
		return domain.Money{}, ErrForbidden
	}
	ok, err := s.auth.Allowed(ctx, actor, tenant, "revenue.read")
	if err != nil {
		return domain.Money{}, err
	}
	if !ok {
		return domain.Money{}, ErrForbidden
	}
	currency = strings.ToUpper(strings.TrimSpace(currency))
	if len(currency) != 3 {
		return domain.Money{}, ErrInvalid
	}
	return s.store.ConfirmedRecovered(ctx, tenant, currency)
}
