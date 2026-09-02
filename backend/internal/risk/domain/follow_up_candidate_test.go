package domain

import (
	"strings"
	"testing"
)

func hesitationState(t *testing.T, sentAt string, confidence float64) ConversationState {
	t.Helper()
	state := baseState(t, sentAt)
	state.OpportunityStage = "PRICE_SENT"
	state.LastIncoming = &MessageRef{ID: "message-a", At: localTime(t, sentAt)}
	state.FollowUp = &FollowUpSignal{
		Value: true, Confidence: confidence, AIRunID: "ai-run-follow-up",
		EvidenceMessageID: "message-a", EvidenceAt: localTime(t, sentAt),
	}
	return state
}

// Рабочие часы 09:00–21:00 по будням: 24 рабочих часа от понедельника 12:00 —
// это среда 12:00.
func TestFollowUpCandidateOpensMediumAfterBusinessDay(t *testing.T) {
	policy := FollowUpCandidatePolicy{}
	state := hesitationState(t, "2026-08-17 12:00", 0.92)

	pending, err := policy.Evaluate(state, localTime(t, "2026-08-18 12:00"))
	if err != nil || pending.Finding != nil || pending.Resolve || !pending.NextDueAt.IsZero() ||
		!pending.DueAt.Equal(localTime(t, "2026-08-19 12:00")) || pending.TriggerMessageID != "message-a" {
		t.Fatalf("до срока = %#v, ошибка = %v", pending, err)
	}
	// Ответ компании «ждём вас» не мешает кандидату: ждём именно клиента.
	state.LastMeaningful = DirectionOutgoing
	due, err := policy.Evaluate(state, localTime(t, "2026-08-19 12:00"))
	if err != nil || due.Finding == nil || due.Finding.Severity != SeverityMedium || due.Finding.Source != SourceHybrid ||
		due.Finding.Confidence == nil || *due.Finding.Confidence != 0.92 ||
		due.Finding.AIRunID == nil || *due.Finding.AIRunID != "ai-run-follow-up" ||
		due.Finding.PolicyVersion != FollowUpCandidatePolicyVersion || !due.NextDueAt.IsZero() ||
		!strings.Contains(due.Finding.Reason, "24 рабочих часов") {
		t.Fatalf("кандидат = %#v, ошибка = %v", due, err)
	}
	if _, err := NewFollowUpCandidate("risk-a", *due.Finding, localTime(t, "2026-08-19 12:00")); err != nil {
		t.Fatalf("агрегат R5 не собрался: %v", err)
	}
	if _, err := NewNoResponse("risk-b", *due.Finding, localTime(t, "2026-08-19 12:00")); err == nil {
		t.Fatal("MEDIUM принят чужим типом риска")
	}
}

func TestFollowUpCandidateUsesWaitingCustomerStageWithoutFact(t *testing.T) {
	state := baseState(t, "2026-08-17 12:00")
	state.OpportunityStage = "WAITING_CUSTOMER"
	state.LastIncoming = &MessageRef{ID: "message-a", At: localTime(t, "2026-08-17 12:00")}
	decision, err := (FollowUpCandidatePolicy{}).Evaluate(state, localTime(t, "2026-08-19 12:00"))
	if err != nil || decision.Finding == nil || decision.Finding.Source != SourceRule ||
		decision.Finding.Confidence != nil || decision.Finding.AIRunID != nil || decision.Finding.TriggerMessageID != "message-a" {
		t.Fatalf("решение по этапу = %#v, ошибка = %v", decision, err)
	}
	other := baseState(t, "2026-08-17 12:00")
	other.OpportunityStage = "QUALIFYING"
	if decision, err := (FollowUpCandidatePolicy{}).Evaluate(other, localTime(t, "2026-08-19 12:00")); err != nil || decision.Finding != nil || decision.Resolve || !decision.DueAt.IsZero() {
		t.Fatalf("этап без ожидания клиента изменил домен: %#v, %v", decision, err)
	}
}

// Выходной критерий этапа 19: колебание создаёт кандидата, явный отказ — нет.
func TestFollowUpCandidateExcludesRejectionAndResolvesOnReturn(t *testing.T) {
	policy := FollowUpCandidatePolicy{}
	at := localTime(t, "2026-08-19 12:00")

	returned := hesitationState(t, "2026-08-17 12:00", 0.92)
	returned.FollowUp.IncomingAfter = true
	if decision, err := policy.Evaluate(returned, at); err != nil || !decision.Resolve || decision.Finding != nil {
		t.Fatalf("возвращение клиента не закрыло кандидата: %#v, %v", decision, err)
	}
	for _, outcome := range []string{"LOST", "NOT_A_LEAD"} {
		rejected := hesitationState(t, "2026-08-17 12:00", 0.92)
		rejected.LatestOutcome = outcome
		if decision, err := policy.Evaluate(rejected, at); err != nil || !decision.Resolve || decision.Finding != nil {
			t.Fatalf("отказ %s не исключил кандидата: %#v, %v", outcome, decision, err)
		}
	}
	for _, stage := range []string{"BOOKED", "WON", "LOST", "ARCHIVED"} {
		terminal := hesitationState(t, "2026-08-17 12:00", 0.92)
		terminal.OpportunityStage = stage
		if decision, err := policy.Evaluate(terminal, at); err != nil || !decision.Resolve || decision.Finding != nil {
			t.Fatalf("этап %s не исключил кандидата: %#v, %v", stage, decision, err)
		}
	}
	weak := hesitationState(t, "2026-08-17 12:00", 0.8)
	weak.OpportunityStage = "QUALIFYING"
	if decision, err := policy.Evaluate(weak, at); err != nil || decision.Resolve || decision.Finding != nil || !decision.DueAt.IsZero() {
		t.Fatalf("слабый факт изменил домен: %#v, %v", decision, err)
	}
	vanished := baseState(t, "2026-08-17 12:00")
	vanished.OpportunityStage = "QUALIFYING"
	vanished.ActiveRisks = map[Type]ActiveRiskSnapshot{
		TypeFollowUpCandidate: {TriggerMessageID: "message-old", TriggerAt: localTime(t, "2026-08-10 12:00"), IncomingAfterTrigger: true},
	}
	if decision, err := policy.Evaluate(vanished, at); err != nil || !decision.Resolve || decision.Finding != nil {
		t.Fatalf("исчезнувший факт с возвращением клиента не закрыл кандидата: %#v, %v", decision, err)
	}
}
