package application_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"lidradar/backend/internal/analytics/application"
	"lidradar/backend/internal/analytics/domain"
)

type authorizer map[string]bool

func (auth authorizer) Allowed(_ context.Context, actor, _, permission string) (bool, error) {
	return auth[actor+":"+permission], nil
}

type store struct {
	organization application.Organization
	found        bool
	period       domain.Period
	currency     string
}

func (s *store) Organization(context.Context, string) (application.Organization, bool, error) {
	return s.organization, s.found, nil
}

func (s *store) Summary(_ context.Context, _ string, period domain.Period, currency string) (domain.Summary, error) {
	s.period, s.currency = period, currency
	return domain.Summary{
		Messages: domain.Messages{Total: 3, Incoming: 2, Outgoing: 1},
		Risks:    domain.Risks{ByType: []domain.RiskTypeMetrics{{RiskType: "NO_RESPONSE", RiskCounters: domain.RiskCounters{Detected: 1, Acted: 1}}}},
		Revenue:  domain.Revenue{Potential: "5000.00", Confirmed: "47000.00", ConfirmedRecovered: "47000.00", ConfirmedPayments: 1},
	}, nil
}

func TestSummaryRequiresOwnerPermissionAndValidPeriod(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	repository := &store{organization: application.Organization{Timezone: "Europe/Moscow", Currency: "RUB"}, found: true}
	service := application.NewService(repository, authorizer{"owner:analytics.read": true}, func() time.Time { return now })
	if _, err := service.Summary(context.Background(), "manager", "tenant", "", ""); !errors.Is(err, application.ErrForbidden) {
		t.Fatalf("менеджер получил аналитику: %v", err)
	}
	if _, err := service.Summary(context.Background(), "owner", "tenant", "2026-09-10", "2026-09-01"); !errors.Is(err, application.ErrInvalid) {
		t.Fatalf("обратный период принят: %v", err)
	}
	summary, err := service.Summary(context.Background(), "owner", "tenant", " 2026-08-01 ", "2026-08-31")
	if err != nil || summary.Period.FromDate != "2026-08-01" || summary.Period.Timezone != "Europe/Moscow" ||
		!summary.Period.From.Equal(time.Date(2026, 7, 31, 21, 0, 0, 0, time.UTC)) || repository.currency != "RUB" ||
		!repository.period.To.Equal(time.Date(2026, 8, 31, 21, 0, 0, 0, time.UTC)) {
		t.Fatalf("сводка = %#v, ошибка = %v, окно хранилища = %#v", summary, err, repository.period)
	}
	if len(summary.Risks.ByType) != 5 || summary.Risks.Detected != 1 || summary.Risks.Acted != 1 || summary.Revenue.Currency != "RUB" ||
		summary.Messages.Total != 3 || summary.Revenue.ConfirmedRecovered != "47000.00" {
		t.Fatalf("нормализация сводки = %#v", summary)
	}
	missing := application.NewService(&store{}, authorizer{"owner:analytics.read": true}, func() time.Time { return now })
	if _, err := missing.Summary(context.Background(), "owner", "tenant", "", ""); !errors.Is(err, application.ErrNotFound) {
		t.Fatalf("организация без строки: %v", err)
	}
	broken := application.NewService(&store{organization: application.Organization{Timezone: "Mars/Olympus", Currency: "RUB"}, found: true},
		authorizer{"owner:analytics.read": true}, func() time.Time { return now })
	if _, err := broken.Summary(context.Background(), "owner", "tenant", "", ""); !errors.Is(err, application.ErrInvalid) {
		t.Fatalf("неизвестный часовой пояс: %v", err)
	}
}
