// Package application согласует подтверждение выручки и её атрибуцию.
package application

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"lidradar/backend/internal/revenue/domain"
)

var (
	ErrForbidden = errors.New("нет разрешения на работу с выручкой")
	ErrNotFound  = errors.New("связанный объект выручки не найден")
	ErrInvalid   = errors.New("некорректная команда выручки")
	ErrConflict  = errors.New("конфликт ключа идемпотентности")
	// ErrRecoveredAlreadyAttributed: у Opportunity уже есть атрибуция RECOVERED.
	// Ограничение держит PostgreSQL (LR-BE-RM-001); при оплате частями
	// следующие события подтверждаются как ORGANIC.
	ErrRecoveredAlreadyAttributed = errors.New("возвращённая выручка по этой возможности уже учтена")
)

const (
	PermissionConfirm = "revenue.confirm"
	PermissionRead    = "revenue.read"
	AttributionWindow = 30 * 24 * time.Hour
)

type Authorizer interface {
	Allowed(context.Context, string, string, string) (bool, error)
}

type IDs interface{ NewID() (string, error) }

type Invalidator interface {
	Publish(tenantID, eventType, resourceID string)
}

// RelatedFact используется испытательным хранилищем для воспроизведения
// межобъектных ограничений PostgreSQL.
type RelatedFact struct {
	OpportunityID string
	RiskID        string
	At            time.Time
}

type AuditRecord struct {
	ID, TenantID, ActorID, Operation, ResourceType, ResourceID string
	At                                                         time.Time
}

type Confirmation struct {
	Event       domain.RevenueEvent       `json:"revenue"`
	Attribution domain.RevenueAttribution `json:"attribution"`
}

// Store обязан сначала разрешить ключ идемпотентности и только для новой
// команды проверять авторитетную цепочку Risk → Action → Outcome. Благодаря
// этому точный повтор остаётся допустимым после истечения 30-дневного окна.
type Store interface {
	Confirm(context.Context, Confirmation, string, [32]byte, AuditRecord, time.Duration) (Confirmation, bool, error)
	ConfirmedRecovered(context.Context, string, string) (domain.Money, error)
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

// WithInvalidator включает короткий сигнал для перечитывания Radar после
// успешной записи возвращённой выручки.
func (service Service) WithInvalidator(events Invalidator) Service {
	service.events = events
	return service
}

type ConfirmCommand struct {
	Amount, Currency            string
	Type                        domain.AttributionType
	RiskID, ActionID, OutcomeID string
}

func (service Service) Confirm(
	ctx context.Context,
	actor, tenant, opportunity, key string,
	command ConfirmCommand,
) (Confirmation, bool, error) {
	if err := service.permit(ctx, actor, tenant, PermissionConfirm); err != nil {
		return Confirmation{}, false, err
	}
	if !validIdempotencyKey(key) || opportunity == "" || service.store == nil || service.ids == nil || service.now == nil {
		return Confirmation{}, false, ErrInvalid
	}
	now := service.now().UTC()
	eventID, err := service.ids.NewID()
	if err != nil {
		return Confirmation{}, false, fmt.Errorf("создание идентификатора события выручки: %w", err)
	}
	attributionID, err := service.ids.NewID()
	if err != nil {
		return Confirmation{}, false, fmt.Errorf("создание идентификатора атрибуции: %w", err)
	}
	auditID, err := service.ids.NewID()
	if err != nil {
		return Confirmation{}, false, fmt.Errorf("создание идентификатора аудита выручки: %w", err)
	}
	event, err := domain.NewConfirmedEvent(
		eventID, tenant, opportunity, command.Amount, command.Currency, actor, now,
	)
	if err != nil {
		return Confirmation{}, false, ErrInvalid
	}
	attribution, err := domain.NewAttribution(
		attributionID, event, command.Type,
		command.RiskID, command.ActionID, command.OutcomeID, now,
	)
	if err != nil {
		return Confirmation{}, false, ErrInvalid
	}
	hash := sha256.Sum256([]byte(strings.Join([]string{
		actor, opportunity, event.Amount.String(), event.Currency,
		string(command.Type), command.RiskID, command.ActionID, command.OutcomeID,
	}, "\x00")))
	confirmation := Confirmation{Event: event, Attribution: attribution}
	stored, created, err := service.store.Confirm(ctx, confirmation, key, hash, AuditRecord{
		ID: auditID, TenantID: tenant, ActorID: actor,
		Operation: "REVENUE_CONFIRMED", ResourceType: "REVENUE_EVENT",
		ResourceID: event.ID, At: now,
	}, AttributionWindow)
	if err == nil && created && command.Type == domain.AttributionRecovered && service.events != nil {
		service.events.Publish(tenant, "risk.changed", command.RiskID)
	}
	return stored, created, err
}

func (service Service) ConfirmedRecovered(ctx context.Context, actor, tenant, currency string) (domain.Money, error) {
	if err := service.permit(ctx, actor, tenant, PermissionRead); err != nil {
		return domain.Money{}, err
	}
	if service.store == nil {
		return domain.Money{}, ErrInvalid
	}
	currency = strings.ToUpper(strings.TrimSpace(currency))
	if !domain.ValidCurrency(currency) {
		return domain.Money{}, ErrInvalid
	}
	return service.store.ConfirmedRecovered(ctx, tenant, currency)
}

func (service Service) permit(ctx context.Context, actor, tenant, permission string) error {
	if actor == "" || tenant == "" || service.auth == nil {
		return ErrForbidden
	}
	allowed, err := service.auth.Allowed(ctx, actor, tenant, permission)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrForbidden
	}
	return nil
}

func validIdempotencyKey(key string) bool {
	return key != "" && key == strings.TrimSpace(key) && len([]rune(key)) <= 255
}
