package domain

import (
	"testing"
	"time"
)

func feedbackRisk(t *testing.T) Risk {
	t.Helper()
	at := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	risk, err := NewNoResponse("risk-1", Finding{
		TenantID: "tenant", OpportunityID: "opportunity", LocationID: "location", TriggerMessageID: "message",
		Severity: SeverityHigh, PolicyVersion: NoResponsePolicyVersion, ReasonCode: "NO_RESPONSE_THRESHOLD_EXCEEDED",
		Reason: "Бизнес не ответил", Source: SourceRule, DueAt: at.Add(-time.Hour),
	}, at)
	if err != nil {
		t.Fatal(err)
	}
	return risk
}

// Снимок риска и сделки фиксируется в момент вердикта; ложное срабатывание
// требует причины, заметка обрезается и ограничена.
func TestFeedbackCapturesImmutableContextAndRequiresReason(t *testing.T) {
	risk := feedbackRisk(t)
	at := risk.DetectedAt.Add(2 * time.Hour)
	feedback, err := NewFeedback("feedback-1", risk, "QUALIFYING", "user", VerdictFalsePositive, ReasonNotALead, "  клиент спрашивал дорогу  ", true, at)
	if err != nil || feedback.Note != "клиент спрашивал дорогу" || !feedback.CorrectsLead() || !feedback.DatasetEligible ||
		feedback.Context.Type != TypeNoResponse || feedback.Context.Status != StatusOpen || feedback.Context.OpportunityStage != "QUALIFYING" ||
		feedback.Context.PolicyVersion != NoResponsePolicyVersion || feedback.Context.TriggerMessageID != "message" ||
		!feedback.Context.DetectedAt.Equal(risk.DetectedAt) || feedback.OpportunityID != "opportunity" {
		t.Fatalf("обратная связь = %#v, ошибка = %v", feedback, err)
	}
	if _, err := NewFeedback("feedback-2", risk, "QUALIFYING", "user", VerdictFalsePositive, "", "", false, at); err == nil {
		t.Fatal("ложное срабатывание без причины принято")
	}
	if positive, err := NewFeedback("feedback-3", risk, "QUALIFYING", "user", VerdictTruePositive, "", "", false, at); err != nil || positive.CorrectsLead() {
		t.Fatalf("настоящий риск без причины отклонён: %#v, %v", positive, err)
	}
	if _, err := NewFeedback("feedback-4", risk, "QUALIFYING", "user", "MAYBE", ReasonOther, "", false, at); err == nil {
		t.Fatal("неизвестный вердикт принят")
	}
	if _, err := NewFeedback("feedback-5", risk, "QUALIFYING", "user", VerdictFalsePositive, "BORED", "", false, at); err == nil {
		t.Fatal("неизвестная причина принята")
	}
	if _, err := NewFeedback("feedback-6", risk, "", "user", VerdictTruePositive, "", "", false, at); err == nil {
		t.Fatal("снимок без стадии сделки принят")
	}
	rejected := feedbackRisk(t)
	if _, err := NewFeedback("feedback-7", rejected, "QUALIFYING", "user", VerdictTruePositive, ReasonCustomerRejected, "", false, at); err != nil {
		t.Fatalf("причина при настоящем риске отклонена: %v", err)
	}
}

func TestMarkFalsePositiveClosesOnlyActiveRisks(t *testing.T) {
	risk := feedbackRisk(t)
	at := risk.DetectedAt.Add(time.Hour)
	if err := risk.Acknowledge(at); err != nil {
		t.Fatal(err)
	}
	if err := risk.MarkFalsePositive(at.Add(time.Minute)); err != nil || risk.Status != StatusFalsePositive || risk.ResolvedAt == nil ||
		risk.Validate() != nil {
		t.Fatalf("ложное срабатывание не закрыло риск: %#v, %v", risk, err)
	}
	closedAt := *risk.ResolvedAt
	if err := risk.MarkFalsePositive(at.Add(time.Hour)); err != nil || !risk.ResolvedAt.Equal(closedAt) {
		t.Fatalf("повтор изменил закрытый риск: %#v, %v", risk, err)
	}
	resolved := feedbackRisk(t)
	if err := resolved.Resolve(at); err != nil {
		t.Fatal(err)
	}
	if err := resolved.MarkFalsePositive(at.Add(time.Minute)); err != nil || resolved.Status != StatusResolved {
		t.Fatalf("история закрытого риска переписана: %#v, %v", resolved, err)
	}
	if err := risk.MarkFalsePositive(time.Time{}); err == nil {
		t.Fatal("нулевое время принято")
	}
}

// LR-BE-RM-019: precision и доля ложных считаются по оценённым рискам,
// покрытие — по всем; порог 0,5 определяет надёжность.
func TestPrecisionRowMetrics(t *testing.T) {
	row := PrecisionRow{Type: TypeNoResponse, TotalRisks: 4, WithFeedback: 2, TruePositives: 1, FalsePositives: 1}
	if precision := row.Precision(); precision == nil || *precision != 0.5 {
		t.Fatalf("precision = %v", precision)
	}
	if rate := row.FalsePositiveRate(); rate == nil || *rate != 0.5 {
		t.Fatalf("доля ложных = %v", rate)
	}
	if row.CoverageRate() != 0.5 || !row.Reliable() {
		t.Fatalf("покрытие = %v, надёжно = %v", row.CoverageRate(), row.Reliable())
	}
	sparse := PrecisionRow{TotalRisks: 10, WithFeedback: 4, TruePositives: 4}
	if sparse.Reliable() || sparse.CoverageRate() != 0.4 {
		t.Fatalf("низкое покрытие признано надёжным: %#v", sparse)
	}
	empty := PrecisionRow{}
	if empty.Precision() != nil || empty.FalsePositiveRate() != nil || empty.CoverageRate() != 0 || empty.Reliable() {
		t.Fatalf("пустая строка даёт метрики: %#v", empty)
	}
	if len(Types()) != 5 || Types()[0] != TypeNoResponse || Types()[1] != TypeCustomerSilentAfterPrice {
		t.Fatalf("порядок типов ТЗ §27 нарушен: %v", Types())
	}
}
