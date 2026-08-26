// Package application согласует корректирующие факты, не связывая предметную
// область с HTTP или PostgreSQL.
package application

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"lidradar/backend/internal/corrective/domain"
)

var (
	ErrForbidden = errors.New("нет разрешения на корректирующую операцию")
	ErrNotFound  = errors.New("связанный объект не найден")
	ErrInvalid   = errors.New("некорректная корректирующая команда")
	ErrConflict  = errors.New("конфликт ключа идемпотентности")
)

const PermissionManage = "risks.manage"

type Authorizer interface {
	Allowed(context.Context, string, string, string) (bool, error)
}
type IDs interface{ NewID() (string, error) }

type Invalidator interface {
	Publish(tenantID, eventType, resourceID string)
}

type RiskReference struct {
	OpportunityID string
	Type          string
}

type AuditRecord struct {
	ID, TenantID, ActorID, Operation, ResourceType, ResourceID string
	At                                                         time.Time
}

type Store interface {
	Risk(ctx context.Context, tenantID, riskID string) (RiskReference, bool, error)
	OpportunityExists(ctx context.Context, tenantID, opportunityID string) (bool, error)
	EnsureRecommendation(ctx context.Context, recommendation domain.Recommendation) (domain.Recommendation, bool, error)
	AppendAction(ctx context.Context, action domain.Action, key string, requestHash [32]byte, audit AuditRecord) (domain.Action, bool, error)
	AppendOutcome(ctx context.Context, outcome domain.Outcome, key string, requestHash [32]byte, audit AuditRecord) (domain.Outcome, bool, error)
}

type Service struct {
	store  Store
	auth   Authorizer
	ids    IDs
	now    func() time.Time
	events Invalidator
}

func NewService(store Store, auth Authorizer, ids IDs, now func() time.Time) Service {
	return Service{store: store, auth: auth, ids: ids, now: now}
}

// WithInvalidator включает краткий сигнал после долговечной записи. Сигнал не
// является источником истины: клиент после него перечитывает REST-модель.
func (s Service) WithInvalidator(events Invalidator) Service {
	s.events = events
	return s
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
	"NO_RESPONSE":                 "Ответить клиенту сейчас.",
	"BOOKING_NOT_CONFIRMED":       "Предложить клиенту конкретный свободный слот.",
	"PROMISE_NOT_FULFILLED":       "Выполнить обещанное клиенту или сообщить новый точный срок.",
	"CUSTOMER_SILENT_AFTER_PRICE": "Напомнить клиенту о предложении и уточнить, остались ли вопросы.",
	"FOLLOW_UP_CANDIDATE":         "Уточнить, остаётся ли услуга актуальной.",
}

// EnsureRecommendation создаёт полезную шаблонную рекомендацию и никогда не
// обращается к AI. Тип риска читается из авторитетной записи, а не от клиента.
func (s Service) EnsureRecommendation(ctx context.Context, actor, tenant, riskID string) (domain.Recommendation, error) {
	if err := s.permit(ctx, actor, tenant); err != nil {
		return domain.Recommendation{}, err
	}
	if riskID == "" || s.store == nil || s.ids == nil || s.now == nil {
		return domain.Recommendation{}, ErrInvalid
	}
	risk, found, err := s.store.Risk(ctx, tenant, riskID)
	if err != nil {
		return domain.Recommendation{}, err
	}
	if !found {
		return domain.Recommendation{}, ErrNotFound
	}
	text, ok := templates[risk.Type]
	if !ok {
		return domain.Recommendation{}, ErrInvalid
	}
	recommendationID, err := s.ids.NewID()
	if err != nil {
		return domain.Recommendation{}, fmt.Errorf("создание идентификатора рекомендации: %w", err)
	}
	recommendation, err := domain.NewRecommendation(recommendationID, tenant, riskID, text, s.now())
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
	if !validIdempotencyKey(key) || s.store == nil || s.ids == nil || s.now == nil {
		return domain.Action{}, false, ErrInvalid
	}
	if _, found, err := s.store.Risk(ctx, tenant, riskID); err != nil {
		return domain.Action{}, false, err
	} else if !found {
		return domain.Action{}, false, ErrNotFound
	}
	actionID, err := s.ids.NewID()
	if err != nil {
		return domain.Action{}, false, fmt.Errorf("создание идентификатора действия: %w", err)
	}
	auditID, err := s.ids.NewID()
	if err != nil {
		return domain.Action{}, false, fmt.Errorf("создание идентификатора аудита: %w", err)
	}
	action, err := domain.NewAction(actionID, tenant, riskID, actor, kind, note, s.now())
	if err != nil {
		return domain.Action{}, false, ErrInvalid
	}
	hash := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%s", actor, riskID, kind, action.Note)))
	stored, created, err := s.store.AppendAction(ctx, action, key, hash, AuditRecord{
		ID: auditID, TenantID: tenant, ActorID: actor, Operation: "ACTION_RECORDED",
		ResourceType: "ACTION", ResourceID: action.ID, At: action.CreatedAt,
	})
	if err == nil && created && s.events != nil {
		s.events.Publish(tenant, "risk.changed", riskID)
	}
	return stored, created, err
}

func (s Service) AddOutcome(ctx context.Context, actor, tenant, opportunityID, key string, status domain.OutcomeStatus, note string) (domain.Outcome, bool, error) {
	if err := s.permit(ctx, actor, tenant); err != nil {
		return domain.Outcome{}, false, err
	}
	if !validIdempotencyKey(key) || s.store == nil || s.ids == nil || s.now == nil {
		return domain.Outcome{}, false, ErrInvalid
	}
	if found, err := s.store.OpportunityExists(ctx, tenant, opportunityID); err != nil {
		return domain.Outcome{}, false, err
	} else if !found {
		return domain.Outcome{}, false, ErrNotFound
	}
	outcomeID, err := s.ids.NewID()
	if err != nil {
		return domain.Outcome{}, false, fmt.Errorf("создание идентификатора исхода: %w", err)
	}
	auditID, err := s.ids.NewID()
	if err != nil {
		return domain.Outcome{}, false, fmt.Errorf("создание идентификатора аудита: %w", err)
	}
	outcome, err := domain.NewOutcome(outcomeID, tenant, opportunityID, actor, status, note, s.now())
	if err != nil {
		return domain.Outcome{}, false, ErrInvalid
	}
	hash := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%s", actor, opportunityID, status, outcome.Note)))
	stored, created, err := s.store.AppendOutcome(ctx, outcome, key, hash, AuditRecord{
		ID: auditID, TenantID: tenant, ActorID: actor, Operation: "OUTCOME_RECORDED",
		ResourceType: "OUTCOME", ResourceID: outcome.ID, At: outcome.CreatedAt,
	})
	return stored, created, err
}

func validIdempotencyKey(key string) bool {
	return key != "" && key == strings.TrimSpace(key) && len([]rune(key)) <= 255
}
