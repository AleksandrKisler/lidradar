package domain

import (
	"strings"
	"testing"
)

func commitmentState(t *testing.T, promisedAt, text string, confidence float64) ConversationState {
	t.Helper()
	state := baseState(t, promisedAt)
	state.OpportunityStage = "QUALIFYING"
	state.LastMeaningful = DirectionOutgoing
	state.Commitment = &CommitmentSignal{
		Value: true, Confidence: confidence, AIRunID: "ai-run-promise",
		EvidenceMessageID: "message-a", EvidenceAt: localTime(t, promisedAt), EvidenceText: text,
	}
	return state
}

func TestPromiseNotFulfilledUsesExplicitDeadlineFromPromise(t *testing.T) {
	state := commitmentState(t, "2026-08-17 12:00", "Проверю расписание и отвечу через десять минут.", 0.95)
	policy := PromiseNotFulfilledPolicy{}
	pending, err := policy.Evaluate(state, localTime(t, "2026-08-17 12:05"))
	if err != nil || pending.Finding != nil || pending.Resolve ||
		!pending.DueAt.Equal(localTime(t, "2026-08-17 12:10")) || pending.TriggerMessageID != "message-a" {
		t.Fatalf("до срока = %#v, ошибка = %v", pending, err)
	}
	overdue, err := policy.Evaluate(state, localTime(t, "2026-08-17 12:10"))
	if err != nil || overdue.Finding == nil {
		t.Fatalf("в срок = %#v, ошибка = %v", overdue, err)
	}
	finding := overdue.Finding
	if finding.Severity != SeverityHigh || finding.Source != SourceHybrid ||
		finding.Confidence == nil || *finding.Confidence != 0.95 ||
		finding.AIRunID == nil || *finding.AIRunID != "ai-run-promise" ||
		finding.TriggerMessageID != "message-a" || finding.PolicyVersion != PromiseNotFulfilledPolicyVersion ||
		finding.ReasonCode != "PROMISE_NOT_FULFILLED_AFTER_DUE" ||
		!strings.Contains(finding.Reason, "через десять минут") || !strings.Contains(finding.Reason, "17.08 12:10") {
		t.Fatalf("объяснение R4 = %#v", finding)
	}
	if _, err := NewPromiseNotFulfilled("risk-a", *finding, localTime(t, "2026-08-17 12:10")); err != nil {
		t.Fatalf("агрегат R4 не собрался: %v", err)
	}
}

func TestPromiseNotFulfilledFallsBackToSixtyBusinessMinutes(t *testing.T) {
	// 20:50 в понедельник: 10 минут сегодня, 50 минут переносятся на 09:00 вторника.
	state := commitmentState(t, "2026-08-17 20:50", "Передам вопрос руководителю и сообщу решение.", 0.9)
	decision, err := (PromiseNotFulfilledPolicy{}).Evaluate(state, localTime(t, "2026-08-17 21:00"))
	if err != nil || decision.Finding != nil || !decision.DueAt.Equal(localTime(t, "2026-08-18 09:50")) {
		t.Fatalf("запасной срок = %#v, ошибка = %v", decision, err)
	}
	overdue, err := (PromiseNotFulfilledPolicy{}).Evaluate(state, localTime(t, "2026-08-18 09:50"))
	if err != nil || overdue.Finding == nil || !strings.Contains(overdue.Finding.Reason, "60 рабочих минут") {
		t.Fatalf("просроченный запасной срок = %#v, ошибка = %v", overdue, err)
	}
}

func TestPromiseNotFulfilledResolvesOnFollowUpAndIgnoresWeakFacts(t *testing.T) {
	policy := PromiseNotFulfilledPolicy{}
	followed := commitmentState(t, "2026-08-17 12:00", "Отвечу через час.", 0.95)
	followed.Commitment.FollowedUp = true
	if decision, err := policy.Evaluate(followed, localTime(t, "2026-08-17 14:00")); err != nil || !decision.Resolve || decision.Finding != nil {
		t.Fatalf("follow-up не закрыл риск: %#v, %v", decision, err)
	}
	weak := commitmentState(t, "2026-08-17 12:00", "Отвечу через час.", 0.8)
	if decision, err := policy.Evaluate(weak, localTime(t, "2026-08-17 14:00")); err != nil || decision.Resolve || decision.Finding != nil || !decision.DueAt.IsZero() {
		t.Fatalf("слабый факт изменил домен: %#v, %v", decision, err)
	}
	none := baseState(t, "2026-08-17 12:00")
	none.LastMeaningful = DirectionOutgoing
	if decision, err := policy.Evaluate(none, localTime(t, "2026-08-17 14:00")); err != nil || decision.Resolve || decision.Finding != nil {
		t.Fatalf("отсутствие факта изменило домен: %#v, %v", decision, err)
	}
	closed := commitmentState(t, "2026-08-17 12:00", "Отвечу через час.", 0.95)
	closed.ActiveOpportunity = false
	if decision, err := policy.Evaluate(closed, localTime(t, "2026-08-17 14:00")); err != nil || !decision.Resolve {
		t.Fatalf("закрытая сделка не закрыла риск: %#v, %v", decision, err)
	}
}

func TestPromiseNotFulfilledResolvesStaleRiskWhoseFactVanished(t *testing.T) {
	// Проекция AI переанализировала переписку и больше не содержит обещания, но
	// компания уже написала клиенту после сообщения-основания открытого риска.
	state := baseState(t, "2026-08-17 12:00")
	state.LastMeaningful = DirectionOutgoing
	state.ActiveRisks = map[Type]ActiveRiskSnapshot{
		TypePromiseNotFulfilled: {TriggerMessageID: "message-old", TriggerAt: localTime(t, "2026-08-17 10:00"), OutgoingAfterTrigger: true},
	}
	decision, err := (PromiseNotFulfilledPolicy{}).Evaluate(state, localTime(t, "2026-08-17 14:00"))
	if err != nil || !decision.Resolve || decision.Finding != nil {
		t.Fatalf("старый риск не закрыт: %#v, %v", decision, err)
	}
	// Новое обещание после выполненного старого закрывает прежний риск и
	// получает собственный срок.
	renewed := commitmentState(t, "2026-08-17 12:00", "Отвечу через час.", 0.95)
	renewed.ActiveRisks = state.ActiveRisks
	decision, err = (PromiseNotFulfilledPolicy{}).Evaluate(renewed, localTime(t, "2026-08-17 12:30"))
	if err != nil || !decision.Resolve || decision.Finding != nil || !decision.DueAt.Equal(localTime(t, "2026-08-17 13:00")) ||
		decision.TriggerMessageID != "message-a" {
		t.Fatalf("новое обещание = %#v, %v", decision, err)
	}
}
