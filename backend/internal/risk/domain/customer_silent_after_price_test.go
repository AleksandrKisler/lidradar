package domain

import (
	"strings"
	"testing"
)

func priceState(t *testing.T, sentAt string, confidence float64) ConversationState {
	t.Helper()
	state := baseState(t, sentAt)
	state.OpportunityStage = "QUALIFYING"
	state.LastMeaningful = DirectionOutgoing
	state.LastOutgoing = &MessageRef{ID: "message-a", At: localTime(t, sentAt)}
	state.Price = &PriceSignal{
		Value: true, Confidence: confidence, AIRunID: "ai-run-price", Amount: "5200", Currency: "RUB",
		EvidenceMessageID: "message-a", EvidenceAt: localTime(t, sentAt),
	}
	return state
}

// Рабочие часы 09:00–21:00 по будням: 24 рабочих часа от понедельника 12:00 —
// это среда 12:00, 48 — пятница 12:00.
func TestCustomerSilentAfterPriceEscalatesThroughBusinessHours(t *testing.T) {
	policy := CustomerSilentAfterPricePolicy{}
	state := priceState(t, "2026-08-17 12:00", 0.96)

	pending, err := policy.Evaluate(state, localTime(t, "2026-08-18 12:00"))
	if err != nil || pending.Finding != nil || pending.Resolve || !pending.NextDueAt.IsZero() ||
		!pending.DueAt.Equal(localTime(t, "2026-08-19 12:00")) || pending.TriggerMessageID != "message-a" {
		t.Fatalf("до срока = %#v, ошибка = %v", pending, err)
	}

	medium, err := policy.Evaluate(state, localTime(t, "2026-08-19 12:00"))
	if err != nil || medium.Finding == nil || medium.Finding.Severity != SeverityMedium ||
		medium.Finding.Source != SourceHybrid || medium.Finding.Confidence == nil || *medium.Finding.Confidence != 0.96 ||
		medium.Finding.AIRunID == nil || *medium.Finding.AIRunID != "ai-run-price" ||
		!medium.Finding.DueAt.Equal(localTime(t, "2026-08-19 12:00")) ||
		!strings.Contains(medium.Finding.Reason, "24 рабочих часов") || !strings.Contains(medium.Finding.Reason, "5200 RUB") {
		t.Fatalf("MEDIUM = %#v, ошибка = %v", medium, err)
	}
	// Первая проверка назначает эскалацию на 48 рабочих часов.
	if !medium.DueAt.Equal(localTime(t, "2026-08-19 12:00")) || !medium.NextDueAt.Equal(localTime(t, "2026-08-21 12:00")) ||
		medium.NextCheckSuffix != EscalationCheckSuffix {
		t.Fatalf("эскалация = %s / %s / %q", medium.DueAt, medium.NextDueAt, medium.NextCheckSuffix)
	}
	if _, err := NewCustomerSilentAfterPrice("risk-a", *medium.Finding, localTime(t, "2026-08-19 12:00")); err != nil {
		t.Fatalf("агрегат R2 не собрался: %v", err)
	}

	high, err := policy.Evaluate(state, localTime(t, "2026-08-21 12:00"))
	if err != nil || high.Finding == nil || high.Finding.Severity != SeverityHigh || !high.NextDueAt.IsZero() ||
		!strings.Contains(high.Finding.Reason, "48 рабочих часов") {
		t.Fatalf("HIGH = %#v, ошибка = %v", high, err)
	}
}

func TestCustomerSilentAfterPriceUsesStageWithoutFact(t *testing.T) {
	state := baseState(t, "2026-08-17 12:00")
	state.OpportunityStage = "PRICE_SENT"
	state.LastMeaningful = DirectionOutgoing
	state.LastOutgoing = &MessageRef{ID: "message-price", At: localTime(t, "2026-08-17 12:00")}
	decision, err := (CustomerSilentAfterPricePolicy{}).Evaluate(state, localTime(t, "2026-08-19 12:00"))
	if err != nil || decision.Finding == nil || decision.Finding.Source != SourceRule ||
		decision.Finding.Confidence != nil || decision.Finding.AIRunID != nil ||
		decision.Finding.TriggerMessageID != "message-price" || decision.Finding.Severity != SeverityMedium {
		t.Fatalf("решение по этапу = %#v, ошибка = %v", decision, err)
	}
	// Клиент ответил последним — молчания нет.
	state.LastMeaningful = DirectionIncoming
	if decision, err := (CustomerSilentAfterPricePolicy{}).Evaluate(state, localTime(t, "2026-08-19 12:00")); err != nil || !decision.Resolve || decision.Finding != nil {
		t.Fatalf("ответ клиента не закрыл риск: %#v, %v", decision, err)
	}
}

func TestCustomerSilentAfterPriceExclusionsAndResolution(t *testing.T) {
	policy := CustomerSilentAfterPricePolicy{}
	at := localTime(t, "2026-08-19 12:00")

	answered := priceState(t, "2026-08-17 12:00", 0.96)
	answered.Price.IncomingAfter = true
	if decision, err := policy.Evaluate(answered, at); err != nil || !decision.Resolve || decision.Finding != nil {
		t.Fatalf("входящее после цены не закрыло риск: %#v, %v", decision, err)
	}
	for _, stage := range []string{"BOOKED", "WON", "LOST", "ARCHIVED"} {
		terminal := priceState(t, "2026-08-17 12:00", 0.96)
		terminal.OpportunityStage = stage
		if decision, err := policy.Evaluate(terminal, at); err != nil || !decision.Resolve || decision.Finding != nil {
			t.Fatalf("этап %s не исключил риск: %#v, %v", stage, decision, err)
		}
	}
	for _, outcome := range []string{"LOST", "NOT_A_LEAD"} {
		rejected := priceState(t, "2026-08-17 12:00", 0.96)
		rejected.LatestOutcome = outcome
		if decision, err := policy.Evaluate(rejected, at); err != nil || !decision.Resolve || decision.Finding != nil {
			t.Fatalf("исход %s не исключил риск: %#v, %v", outcome, decision, err)
		}
	}
	thinking := priceState(t, "2026-08-17 12:00", 0.96)
	thinking.LatestOutcome = "THINKING"
	if decision, err := policy.Evaluate(thinking, at); err != nil || decision.Finding == nil {
		t.Fatalf("исход THINKING ошибочно исключил риск: %#v, %v", decision, err)
	}
	weak := priceState(t, "2026-08-17 12:00", 0.8)
	if decision, err := policy.Evaluate(weak, at); err != nil || decision.Resolve || decision.Finding != nil || !decision.DueAt.IsZero() {
		t.Fatalf("слабый факт изменил домен: %#v, %v", decision, err)
	}
	none := baseState(t, "2026-08-17 12:00")
	none.LastMeaningful = DirectionOutgoing
	none.ActiveRisks = map[Type]ActiveRiskSnapshot{
		TypeCustomerSilentAfterPrice: {TriggerMessageID: "message-old", TriggerAt: localTime(t, "2026-08-10 12:00"), IncomingAfterTrigger: true},
	}
	if decision, err := policy.Evaluate(none, at); err != nil || !decision.Resolve || decision.Finding != nil {
		t.Fatalf("исчезнувший факт с ответом клиента не закрыл риск: %#v, %v", decision, err)
	}
	none.ActiveRisks[TypeCustomerSilentAfterPrice] = ActiveRiskSnapshot{TriggerMessageID: "message-old", TriggerAt: localTime(t, "2026-08-10 12:00")}
	if decision, err := policy.Evaluate(none, at); err != nil || decision.Resolve || decision.Finding != nil {
		t.Fatalf("исчезнувший факт без ответа изменил домен: %#v, %v", decision, err)
	}
}
