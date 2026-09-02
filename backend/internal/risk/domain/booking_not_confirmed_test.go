package domain

import (
	"strings"
	"testing"
	"time"
)

func bookingState(t *testing.T, received string, confidence float64) ConversationState {
	state := baseState(t, received)
	state.OpportunityStage = "QUALIFYING"
	state.BookingIntent = &BookingIntentSignal{
		Value: true, Confidence: confidence, AIRunID: "ai-run-a",
		EvidenceMessageID: "message-a", EvidenceAt: state.LastMeaningfulAt,
	}
	return state
}

func TestBookingNotConfirmedUsesStrongSemanticFactAndBusinessMinutes(t *testing.T) {
	state := bookingState(t, "2026-08-17 20:50", 0.95)
	decision, err := (BookingNotConfirmedPolicy{}).Evaluate(state, localTime(t, "2026-08-18 09:20"))
	if err != nil {
		t.Fatal(err)
	}
	wantDue := localTime(t, "2026-08-18 09:20")
	if !decision.DueAt.Equal(wantDue) || decision.Finding == nil {
		t.Fatalf("решение = %#v, срок нужен %s", decision, wantDue)
	}
	finding := decision.Finding
	if finding.Severity != SeverityCritical || finding.Source != SourceHybrid ||
		finding.Confidence == nil || *finding.Confidence != 0.95 ||
		finding.AIRunID == nil || *finding.AIRunID != "ai-run-a" ||
		finding.TriggerMessageID != "message-a" ||
		!strings.Contains(finding.Reason, "0.950") || !strings.Contains(finding.Reason, "30 рабочих минут") {
		t.Fatalf("объяснение R3 = %#v", finding)
	}
}

func TestBookingNotConfirmedLowConfidenceDoesNotChangeDomain(t *testing.T) {
	state := bookingState(t, "2026-08-17 12:00", 0.849)
	decision, err := (BookingNotConfirmedPolicy{}).Evaluate(state, localTime(t, "2026-08-17 15:00"))
	if err != nil || decision.Finding != nil || decision.Resolve || !decision.DueAt.IsZero() {
		t.Fatalf("решение слабого факта = %#v, ошибка = %v", decision, err)
	}
}

func TestBookingNotConfirmedUsesStageWithoutAI(t *testing.T) {
	state := baseState(t, "2026-08-17 12:00")
	state.OpportunityStage = "BOOKING_INTENT"
	decision, err := (BookingNotConfirmedPolicy{}).Evaluate(state, localTime(t, "2026-08-17 12:30"))
	if err != nil || decision.Finding == nil || decision.Finding.Source != SourceRule ||
		decision.Finding.Confidence != nil || decision.Finding.AIRunID != nil {
		t.Fatalf("решение по этапу = %#v, ошибка = %v", decision, err)
	}
}

func TestBookingNotConfirmedClosesOnlyOnConfirmedOrInactiveOpportunity(t *testing.T) {
	state := bookingState(t, "2026-08-17 12:00", 0.95)
	state.LastMeaningful = DirectionOutgoing
	decision, err := (BookingNotConfirmedPolicy{}).Evaluate(state, time.Now())
	if err != nil || decision.Resolve {
		t.Fatalf("ответ бизнеса не является подтверждением записи: %#v, %v", decision, err)
	}
	state.OpportunityStage = "BOOKED"
	decision, err = (BookingNotConfirmedPolicy{}).Evaluate(state, time.Now())
	if err != nil || !decision.Resolve {
		t.Fatalf("BOOKED должен закрыть риск: %#v, %v", decision, err)
	}
}
