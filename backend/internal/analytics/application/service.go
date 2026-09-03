// Package application выдаёт сводку аналитики владельцу организации.
package application

import (
	"context"
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
	// согласованным снимком; разрез по типам риска может быть неполным.
	Summary(ctx context.Context, tenantID string, period domain.Period, currency string) (domain.Summary, error)
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
	return summary, nil
}
