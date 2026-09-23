// Package application выдаёт сводку аналитики владельцу организации.
package application

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"lidradar/backend/internal/analytics/domain"
)

var (
	ErrForbidden = errors.New("нет разрешения на аналитику")
	ErrNotFound  = errors.New("организация не найдена")
	ErrInvalid   = errors.New("некорректный запрос аналитики")
)

const PermissionRead = "analytics.read"

type Authorizer interface {
	Allowed(ctx context.Context, actorID, tenantID, permission string) (bool, error)
}

// Organization — часовой пояс границ окна и валюта денежных показателей.
type Organization struct {
	Timezone, Currency string
}

type Store interface {
	Organization(ctx context.Context, tenantID string) (Organization, bool, error)
	// Summary читает все показатели окна из необработанных фактов одним
	// согласованным снимком; разрез по типам риска, дневной ряд и разбивка
	// атрибуций могут быть неполными — сервис дополняет их нулями.
	Summary(ctx context.Context, tenantID string, period domain.Period, currency string) (domain.Summary, error)
	// Payments отдаёт подтверждённые события окна от новых к старым.
	Payments(ctx context.Context, tenantID string, period domain.Period, limit int, cursor *domain.PaymentCursor) ([]domain.Payment, bool, error)
}

const (
	defaultPaymentsLimit = 50
	maximumPaymentsLimit = 100
)

// PaymentPage — страница последних оплат окна с непрозрачным курсором,
// привязанным к окну.
type PaymentPage struct {
	Period     domain.Period    `json:"period"`
	Items      []domain.Payment `json:"items"`
	NextCursor *string          `json:"nextCursor"`
}

type Service struct {
	store Store
	auth  Authorizer
	now   func() time.Time
}

func NewService(store Store, auth Authorizer, now func() time.Time) Service {
	return Service{store: store, auth: auth, now: now}
}

// Summary считает сводку за окно календарных дат в часовом поясе организации.
func (service Service) Summary(ctx context.Context, actor, tenant, fromDate, toDate string) (domain.Summary, error) {
	if service.store == nil || service.auth == nil || service.now == nil || actor == "" || tenant == "" {
		return domain.Summary{}, ErrInvalid
	}
	allowed, err := service.auth.Allowed(ctx, actor, tenant, PermissionRead)
	if err != nil {
		return domain.Summary{}, err
	}
	if !allowed {
		return domain.Summary{}, ErrForbidden
	}
	organization, found, err := service.store.Organization(ctx, tenant)
	if err != nil {
		return domain.Summary{}, err
	}
	if !found {
		return domain.Summary{}, ErrNotFound
	}
	location, err := time.LoadLocation(organization.Timezone)
	if err != nil {
		return domain.Summary{}, fmt.Errorf("%w: часовой пояс организации", ErrInvalid)
	}
	period, err := domain.ResolvePeriod(strings.TrimSpace(fromDate), strings.TrimSpace(toDate), service.now(), location)
	if err != nil {
		return domain.Summary{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	summary, err := service.store.Summary(ctx, tenant, period, organization.Currency)
	if err != nil {
		return domain.Summary{}, err
	}
	summary.Period = period
	summary.Risks = domain.RisksFromTypes(summary.Risks.ByType)
	summary.Revenue.Currency = organization.Currency
	summary.Series, err = domain.FillSeries(period, summary.Series)
	if err != nil {
		return domain.Summary{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	summary.Attribution = domain.AttributionFromRows(summary.Attribution)
	return summary, nil
}

// Payments перечисляет подтверждённые оплаты окна от новых к старым. Окно
// задаётся так же, как у сводки; курсор действует только внутри своего окна.
func (service Service) Payments(ctx context.Context, actor, tenant, fromDate, toDate string, limit int, cursor string) (PaymentPage, error) {
	if service.store == nil || service.auth == nil || service.now == nil || actor == "" || tenant == "" {
		return PaymentPage{}, ErrInvalid
	}
	allowed, err := service.auth.Allowed(ctx, actor, tenant, PermissionRead)
	if err != nil {
		return PaymentPage{}, err
	}
	if !allowed {
		return PaymentPage{}, ErrForbidden
	}
	if limit == 0 {
		limit = defaultPaymentsLimit
	}
	if limit < 1 || limit > maximumPaymentsLimit {
		return PaymentPage{}, ErrInvalid
	}
	organization, found, err := service.store.Organization(ctx, tenant)
	if err != nil {
		return PaymentPage{}, err
	}
	if !found {
		return PaymentPage{}, ErrNotFound
	}
	location, err := time.LoadLocation(organization.Timezone)
	if err != nil {
		return PaymentPage{}, fmt.Errorf("%w: часовой пояс организации", ErrInvalid)
	}
	period, err := domain.ResolvePeriod(strings.TrimSpace(fromDate), strings.TrimSpace(toDate), service.now(), location)
	if err != nil {
		return PaymentPage{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	pageCursor, err := decodePaymentCursor(cursor, period)
	if err != nil {
		return PaymentPage{}, err
	}
	items, more, err := service.store.Payments(ctx, tenant, period, limit, pageCursor)
	if err != nil {
		return PaymentPage{}, err
	}
	if items == nil {
		items = []domain.Payment{}
	}
	page := PaymentPage{Period: period, Items: items}
	if more && len(items) > 0 {
		last := items[len(items)-1]
		page.NextCursor = encodePaymentCursor(last.ConfirmedAt, last.EventID, period)
	}
	return page, nil
}

type encodedPaymentCursor struct {
	At     string `json:"at"`
	ID     string `json:"id"`
	Period string `json:"p"`
}

func periodKey(period domain.Period) string { return period.FromDate + ".." + period.ToDate }

func encodePaymentCursor(at time.Time, id string, period domain.Period) *string {
	encoded, _ := json.Marshal(encodedPaymentCursor{At: at.UTC().Format(time.RFC3339Nano), ID: id, Period: periodKey(period)})
	value := base64.RawURLEncoding.EncodeToString(encoded)
	return &value
}

func decodePaymentCursor(cursor string, period domain.Period) (*domain.PaymentCursor, error) {
	if cursor == "" {
		return nil, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return nil, ErrInvalid
	}
	var value encodedPaymentCursor
	if json.Unmarshal(decoded, &value) != nil || value.ID == "" || value.Period != periodKey(period) {
		return nil, ErrInvalid
	}
	at, err := time.Parse(time.RFC3339Nano, value.At)
	if err != nil {
		return nil, ErrInvalid
	}
	return &domain.PaymentCursor{At: at.UTC(), ID: value.ID}, nil
}
