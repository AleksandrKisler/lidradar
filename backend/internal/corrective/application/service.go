// Package application coordinates corrective records without coupling their
// domain to HTTP or PostgreSQL.
package application

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"lidradar/backend/internal/corrective/domain"
)

var (
	ErrForbidden = errors.New("forbidden")
	ErrNotFound  = errors.New("resource not found")
	ErrInvalid   = errors.New("invalid request")
	ErrConflict  = errors.New("idempotency conflict")
)

const PermissionManage = "risks.manage"

type Authorizer interface {
	Allowed(context.Context, string, string, string) (bool, error)
}
type IDs interface{ NewID() string }
type AuditRecord struct {
	TenantID, ActorID, Operation, ResourceID string
	At                                       time.Time
}

type Store interface {
	RiskOpportunity(ctx context.Context, tenantID, riskID string) (string, bool, error)
	OpportunityExists(ctx context.Context, tenantID, opportunityID string) (bool, error)
	EnsureRecommendation(ctx context.Context, recommendation domain.Recommendation) (domain.Recommendation, bool, error)
	AppendAction(ctx context.Context, action domain.Action, key string, requestHash [32]byte, audit AuditRecord) (domain.Action, bool, error)
	AppendOutcome(ctx context.Context, outcome domain.Outcome, key string, requestHash [32]byte, audit AuditRecord) (domain.Outcome, bool, error)
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

func (s Service) permit(ctx context.Context, actor, tenant string) error {
	if actor == "" || tenant == "" || s.auth == nil {
		return ErrForbidden
	}
	ok, err := s.auth.Allowed(ctx, actor, tenant, PermissionManage)
	if err != nil {
		return err
	}
	if !ok {
		return ErrForbidden
	}
	return nil
}

var templates = map[string]string{
	"NO_RESPONSE":           "Ответить клиенту сейчас.",
	"BOOKING_NOT_CONFIRMED": "Предложить клиенту конкретный свободный слот.",
	"FOLLOW_UP_CANDIDATE":   "Уточнить, остаётся ли услуга актуальной.",
}

// EnsureRecommendation supplies a deterministic useful fallback and never
// invokes AI. The store enforces one template recommendation per tenant/risk.
func (s Service) EnsureRecommendation(ctx context.Context, actor, tenant, riskID, riskType string) (domain.Recommendation, error) {
	if err := s.permit(ctx, actor, tenant); err != nil {
		return domain.Recommendation{}, err
	}
	text, ok := templates[riskType]
	if !ok || riskID == "" || s.store == nil || s.ids == nil || s.now == nil {
		return domain.Recommendation{}, ErrInvalid
	}
	if _, found, err := s.store.RiskOpportunity(ctx, tenant, riskID); err != nil {
		return domain.Recommendation{}, err
	} else if !found {
		return domain.Recommendation{}, ErrNotFound
	}
	recommendation, err := domain.NewRecommendation(s.ids.NewID(), tenant, riskID, text, s.now())
	if err != nil {
		return domain.Recommendation{}, ErrInvalid
	}
	result, _, err := s.store.EnsureRecommendation(ctx, recommendation)
	return result, err
}

func (s Service) AddAction(ctx context.Context, actor, tenant, riskID, key string, kind domain.ActionType, note string) (domain.Action, bool, error) {
	if err := s.permit(ctx, actor, tenant); err != nil {
		return domain.Action{}, false, err
	}
	if key == "" || s.store == nil || s.ids == nil || s.now == nil {
		return domain.Action{}, false, ErrInvalid
	}
	if _, found, err := s.store.RiskOpportunity(ctx, tenant, riskID); err != nil {
		return domain.Action{}, false, err
	} else if !found {
		return domain.Action{}, false, ErrNotFound
	}
	action, err := domain.NewAction(s.ids.NewID(), tenant, riskID, actor, kind, note, s.now())
	if err != nil {
		return domain.Action{}, false, ErrInvalid
	}
	hash := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s", riskID, kind, note)))
	return s.store.AppendAction(ctx, action, key, hash, AuditRecord{tenant, actor, "ACTION_CREATED", action.ID, action.CreatedAt})
}

func (s Service) AddOutcome(ctx context.Context, actor, tenant, opportunityID, key string, status domain.OutcomeStatus, note string) (domain.Outcome, bool, error) {
	if err := s.permit(ctx, actor, tenant); err != nil {
		return domain.Outcome{}, false, err
	}
	if key == "" || s.store == nil || s.ids == nil || s.now == nil {
		return domain.Outcome{}, false, ErrInvalid
	}
	if found, err := s.store.OpportunityExists(ctx, tenant, opportunityID); err != nil {
		return domain.Outcome{}, false, err
	} else if !found {
		return domain.Outcome{}, false, ErrNotFound
	}
	outcome, err := domain.NewOutcome(s.ids.NewID(), tenant, opportunityID, actor, status, note, s.now())
	if err != nil {
		return domain.Outcome{}, false, ErrInvalid
	}
	hash := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s", opportunityID, status, note)))
	return s.store.AppendOutcome(ctx, outcome, key, hash, AuditRecord{tenant, actor, "OUTCOME_CREATED", outcome.ID, outcome.CreatedAt})
}
