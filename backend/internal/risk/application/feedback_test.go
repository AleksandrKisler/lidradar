package application_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"lidradar/backend/internal/risk/application"
	"lidradar/backend/internal/risk/domain"
)

type feedbackAuthorizer struct{ permissions map[string]bool }

func (auth feedbackAuthorizer) Allowed(_ context.Context, actor, _, permission string) (bool, error) {
	return auth.permissions[actor+":"+permission], nil
}

type feedbackStore struct {
	risk       domain.Risk
	stage      string
	consent    bool
	feedback   []domain.Feedback
	audits     []application.AuditRecord
	precision  []domain.PrecisionRow
	lastWindow [2]time.Time
}

func (store *feedbackStore) RiskForFeedback(_ context.Context, tenantID, riskID string) (domain.Risk, string, bool, error) {
	if store.risk.ID != riskID || store.risk.TenantID != tenantID {
		return domain.Risk{}, "", false, nil
	}
	return store.risk, store.stage, true, nil
}

func (store *feedbackStore) DatasetConsentActive(context.Context, string) (bool, error) {
	return store.consent, nil
}

func (store *feedbackStore) AppendFeedback(_ context.Context, feedback domain.Feedback, audit application.AuditRecord) (domain.Feedback, domain.Risk, bool, error) {
	store.feedback = append(store.feedback, feedback)
	store.audits = append(store.audits, audit)
	changed := false
	if feedback.Verdict == domain.VerdictFalsePositive && store.risk.Active() {
		if err := store.risk.MarkFalsePositive(feedback.CreatedAt); err != nil {
			return domain.Feedback{}, domain.Risk{}, false, err
		}
		changed = true
	}
	return feedback, store.risk, changed, nil
}

func (store *feedbackStore) Precision(_ context.Context, _ string, from, to time.Time) ([]domain.PrecisionRow, error) {
	store.lastWindow = [2]time.Time{from, to}
	return store.precision, nil
}

type feedbackEvents struct{ published []string }

func (events *feedbackEvents) Publish(_, eventType, resourceID string) {
	events.published = append(events.published, eventType+":"+resourceID)
}

type leadCorrector struct {
	closed []string
	err    error
}

func (corrector *leadCorrector) MarkNotALead(_ context.Context, _, _, opportunityID string) error {
	if corrector.err != nil {
		return corrector.err
	}
	corrector.closed = append(corrector.closed, opportunityID)
	return nil
}

type feedbackIDs struct{ n int }

func (ids *feedbackIDs) NewID() (string, error) {
	ids.n++
	return "id-" + strings.Repeat("f", ids.n), nil
}

func feedbackFixture(t *testing.T) (*feedbackStore, *feedbackEvents, *leadCorrector, application.Feedback) {
	t.Helper()
	at := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	risk, err := domain.NewNoResponse("risk-1", domain.Finding{
		TenantID: "tenant", OpportunityID: "opportunity", LocationID: "location", TriggerMessageID: "message",
		Severity: domain.SeverityHigh, PolicyVersion: domain.NoResponsePolicyVersion, ReasonCode: "NO_RESPONSE_THRESHOLD_EXCEEDED",
		Reason: "Бизнес не ответил", Source: domain.SourceRule, DueAt: at.Add(-time.Hour),
	}, at)
	if err != nil {
		t.Fatal(err)
	}
	store := &feedbackStore{risk: risk, stage: "QUALIFYING"}
	events := new(feedbackEvents)
	corrector := new(leadCorrector)
	auth := feedbackAuthorizer{permissions: map[string]bool{
		"manager:" + application.PermissionManage:  true,
		"owner:" + application.PermissionManage:    true,
		"owner:" + application.PermissionAnalytics: true,
	}}
	service := application.NewFeedback(store, auth, new(feedbackIDs), func() time.Time { return at.Add(time.Hour) }).
		WithInvalidator(events).WithLeadCorrector(corrector)
	return store, events, corrector, service
}

// Ложное срабатывание закрывает риск, оповещает Radar и по причине NOT_A_LEAD
// закрывает сделку; снимок и аудит сохраняются в одной операции.
func TestFeedbackFalsePositiveClosesRiskAndCorrectsLead(t *testing.T) {
	store, events, corrector, service := feedbackFixture(t)
	feedback, err := service.Record(context.Background(), "manager", "tenant", "risk-1", application.FeedbackCommand{
		Verdict: "FALSE_POSITIVE", Reason: "NOT_A_LEAD", Note: "спам",
	})
	if err != nil || feedback.Verdict != domain.VerdictFalsePositive || !feedback.CorrectsLead() || feedback.DatasetEligible ||
		feedback.Context.Status != domain.StatusOpen || feedback.Context.OpportunityStage != "QUALIFYING" {
		t.Fatalf("обратная связь = %#v, %v", feedback, err)
	}
	if store.risk.Status != domain.StatusFalsePositive || len(store.audits) != 1 || store.audits[0].Operation != "RISK_FEEDBACK_RECORDED" ||
		store.audits[0].EntityID != feedback.ID || store.audits[0].ActorID != "manager" {
		t.Fatalf("риск %s, аудит %#v", store.risk.Status, store.audits)
	}
	if len(events.published) != 1 || events.published[0] != "risk.false_positive:risk-1" || len(corrector.closed) != 1 || corrector.closed[0] != "opportunity" {
		t.Fatalf("события %v, закрытые сделки %v", events.published, corrector.closed)
	}
	// Повторный вердикт на закрытом риске сохраняется, но риск и Radar не трогает.
	again, err := service.Record(context.Background(), "manager", "tenant", "risk-1", application.FeedbackCommand{Verdict: "TRUE_POSITIVE"})
	if err != nil || again.Context.Status != domain.StatusFalsePositive || len(events.published) != 1 || len(store.feedback) != 2 {
		t.Fatalf("повторная обратная связь: %#v, %v, события %v", again, err, events.published)
	}
}

func TestFeedbackValidationPermissionsAndConsent(t *testing.T) {
	store, _, corrector, service := feedbackFixture(t)
	if _, err := service.Record(context.Background(), "stranger", "tenant", "risk-1", application.FeedbackCommand{Verdict: "TRUE_POSITIVE"}); !errors.Is(err, application.ErrForbidden) {
		t.Fatalf("посторонний записал обратную связь: %v", err)
	}
	if _, err := service.Record(context.Background(), "manager", "tenant", "risk-9", application.FeedbackCommand{Verdict: "TRUE_POSITIVE"}); !errors.Is(err, application.ErrNotFound) {
		t.Fatalf("несуществующий риск: %v", err)
	}
	if _, err := service.Record(context.Background(), "manager", "tenant", "risk-1", application.FeedbackCommand{Verdict: "FALSE_POSITIVE"}); !errors.Is(err, application.ErrInvalidCommand) {
		t.Fatalf("ложное срабатывание без причины принято: %v", err)
	}
	if len(store.feedback) != 0 || len(corrector.closed) != 0 {
		t.Fatalf("отклонённые команды оставили след: %#v, %v", store.feedback, corrector.closed)
	}
	store.consent = true
	feedback, err := service.Record(context.Background(), "owner", "tenant", "risk-1", application.FeedbackCommand{
		Verdict: "TRUE_POSITIVE", Reason: "OTHER",
	})
	if err != nil || !feedback.DatasetEligible || store.risk.Status != domain.StatusOpen {
		t.Fatalf("обратная связь при согласии: %#v, %v, риск %s", feedback, err, store.risk.Status)
	}
	corrector.err = errors.New("сделка недоступна")
	if _, err := service.Record(context.Background(), "owner", "tenant", "risk-1", application.FeedbackCommand{
		Verdict: "FALSE_POSITIVE", Reason: "NOT_A_LEAD",
	}); !errors.Is(err, application.ErrLeadCorrection) {
		t.Fatalf("сбой закрытия сделки скрыт: %v", err)
	}
}

// Отчёт содержит все пять типов, окно по умолчанию — с начала до сейчас, а
// доступ ограничен разрешением analytics.read.
func TestPrecisionReportCoversEveryTypeAndRequiresAnalytics(t *testing.T) {
	store, _, _, service := feedbackFixture(t)
	store.precision = []domain.PrecisionRow{
		{Type: domain.TypeNoResponse, TotalRisks: 4, WithFeedback: 3, TruePositives: 2, FalsePositives: 1},
	}
	if _, err := service.Precision(context.Background(), "manager", "tenant", time.Time{}, time.Time{}); !errors.Is(err, application.ErrForbidden) {
		t.Fatalf("менеджер получил метрики: %v", err)
	}
	report, err := service.Precision(context.Background(), "owner", "tenant", time.Time{}, time.Time{})
	if err != nil || len(report.Items) != 5 || report.MinimumCoverage != domain.MilestoneCoverage ||
		!store.lastWindow[0].Equal(time.Unix(0, 0)) || !store.lastWindow[1].Equal(time.Date(2026, 9, 3, 13, 0, 0, 0, time.UTC)) {
		t.Fatalf("отчёт = %#v, ошибка = %v, окно = %v", report, err, store.lastWindow)
	}
	first := report.Items[0]
	if first.RiskType != domain.TypeNoResponse || first.Precision == nil || *first.Precision < 0.66 || *first.Precision > 0.67 ||
		first.FalsePositiveRate == nil || first.CoverageRate != 0.75 || !first.Reliable {
		t.Fatalf("строка NO_RESPONSE = %#v", first)
	}
	if empty := report.Items[4]; empty.RiskType != domain.TypeFollowUpCandidate || empty.Precision != nil || empty.Reliable || empty.TotalRisks != 0 {
		t.Fatalf("пустой тип = %#v", empty)
	}
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if _, err := service.Precision(context.Background(), "owner", "tenant", from, from); !errors.Is(err, application.ErrInvalidCommand) {
		t.Fatalf("пустое окно принято: %v", err)
	}
}
