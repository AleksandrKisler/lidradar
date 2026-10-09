package domain

import (
	"strings"
	"testing"
	"time"
)

func agreementState(at time.Time) ConversationState {
	return ConversationState{
		TenantID: "tenant", OpportunityID: "opportunity", LocationID: "location",
		ActiveOpportunity: true, OpportunityStage: "WAITING_CUSTOMER",
		LastMeaningfulID: "outgoing", LastMeaningfulAt: at, LastMeaningful: DirectionOutgoing,
		ResponseThreshold: 45 * time.Minute, AgreementThreshold: 120 * time.Minute,
		BusinessHours: BusinessHours{Timezone: "UTC", Weekly: map[time.Weekday][]BusinessPeriod{
			time.Monday:  {{Open: Clock(9, 0), Close: Clock(18, 0)}},
			time.Tuesday: {{Open: Clock(9, 0), Close: Clock(18, 0)}},
		}},
		AgreementsCurrent: true, ActiveRisks: map[Type]ActiveRiskSnapshot{},
		Agreements: []AgreementSignal{{Kind: "BOOKING_CONFIRMATION", WaitingFor: "CUSTOMER", Status: "PENDING",
			TriggerMessageID: "trigger", TriggerAt: at, TriggerText: "Подойдёт время?", Confidence: .95, AIRunID: "run"}},
	}
}

func TestUnfinishedAgreementBusinessDeadline(t *testing.T) {
	start := time.Date(2026, 10, 5, 17, 0, 0, 0, time.UTC) // Monday
	state := agreementState(start)
	policy := UnfinishedAgreementPolicy{}
	due := time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		at   time.Time
		want bool
	}{
		{"before", due.Add(-time.Second), false},
		{"exact", due, true},
		{"after", due.Add(time.Minute), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decision, err := policy.Evaluate(state, tc.at)
			if err != nil || !decision.DueAt.Equal(due) || (decision.Finding != nil) != tc.want {
				t.Fatalf("decision=%+v err=%v", decision, err)
			}
			if tc.want && (!strings.Contains(decision.Finding.Reason, "клиента") || decision.Finding.TriggerMessageID != "trigger") {
				t.Fatalf("finding=%+v", decision.Finding)
			}
		})
	}
}

func TestUnfinishedAgreementExplicitDeferAndSlot(t *testing.T) {
	start := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	state := agreementState(start)
	state.Agreements[0].TriggerText = "Напишите завтра до 15:00, подходит ли запись"
	decision, err := (UnfinishedAgreementPolicy{}).Evaluate(state, start)
	if err != nil || !decision.DueAt.Equal(time.Date(2026, 10, 6, 15, 0, 0, 0, time.UTC)) {
		t.Fatalf("explicit deadline: %+v %v", decision, err)
	}
	state.Agreements[0].TriggerText = "Запись завтра в 15:00 вам подходит?"
	decision, err = (UnfinishedAgreementPolicy{}).Evaluate(state, start)
	if err != nil || !decision.DueAt.Equal(start.Add(2*time.Hour)) {
		t.Fatalf("appointment slot is not reply deadline: %+v %v", decision, err)
	}
}

func TestUnfinishedAgreementSemanticLifecycle(t *testing.T) {
	start := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	state := agreementState(start)
	state.ActiveRisks[TypeUnfinishedAgreement] = ActiveRiskSnapshot{TriggerMessageID: "trigger", TriggerAt: start}
	policy := UnfinishedAgreementPolicy{}
	state.AgreementsCurrent = false
	if decision, _ := policy.Evaluate(state, start.Add(3*time.Hour)); decision.Resolve || decision.Finding != nil {
		t.Fatalf("unavailable snapshot changed risk: %+v", decision)
	}
	state.AgreementsCurrent = true
	state.Agreements = nil // bounded context alone is not a resolution
	if decision, _ := policy.Evaluate(state, start.Add(3*time.Hour)); decision.Resolve {
		t.Fatalf("missing trigger resolved risk: %+v", decision)
	}
	state.Agreements = []AgreementSignal{{Kind: "BOOKING_CONFIRMATION", WaitingFor: "CUSTOMER", Status: "RESOLVED",
		TriggerMessageID: "trigger", TriggerAt: start, Confidence: .95, AIRunID: "run"}}
	if decision, _ := policy.Evaluate(state, start.Add(3*time.Hour)); !decision.Resolve {
		t.Fatalf("explicit resolution ignored: %+v", decision)
	}
	state.Agreements[0].Status = "PENDING"
	state.Agreements[0].TriggerMessageID = "new"
	state.Agreements[0].TriggerAt = start.Add(time.Hour)
	if decision, _ := policy.Evaluate(state, start.Add(90*time.Minute)); !decision.Resolve || decision.Finding != nil || decision.TriggerMessageID != "new" {
		t.Fatalf("new trigger did not reset schedule: %+v", decision)
	}
}

func TestUnfinishedAgreementSpecializedRulePriority(t *testing.T) {
	start := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	state := agreementState(start)
	state.Agreements[0].WaitingFor = "BUSINESS"
	state.LastMeaningful = DirectionIncoming
	state.LastMeaningfulID = "trigger"
	if decision, _ := (UnfinishedAgreementPolicy{}).Evaluate(state, start.Add(3*time.Hour)); decision.Finding != nil {
		t.Fatalf("R1 action duplicated: %+v", decision)
	}
	state.LastMeaningfulID = "other"
	state.ActiveRisks[TypeBookingNotConfirmed] = ActiveRiskSnapshot{TriggerMessageID: "trigger", TriggerAt: start}
	if decision, _ := (UnfinishedAgreementPolicy{}).Evaluate(state, start.Add(3*time.Hour)); decision.Finding != nil {
		t.Fatalf("R3 action duplicated: %+v", decision)
	}
}

func TestSpecializedFutureCheckOwnsAgreementBeforeRiskExists(t *testing.T) {
	start := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	for _, kind := range []string{"price", "followup"} {
		state := agreementState(start)
		if kind == "price" {
			state.Price = &PriceSignal{Value: true, Confidence: .95, EvidenceMessageID: "trigger", EvidenceAt: start, AIRunID: "run"}
		} else {
			state.LastMeaningful = DirectionIncoming
			state.LastMeaningfulID = "trigger"
			state.FollowUp = &FollowUpSignal{Value: true, Confidence: .95, EvidenceMessageID: "trigger", EvidenceAt: start, AIRunID: "run"}
		}
		decision, err := (UnfinishedAgreementPolicy{}).Evaluate(state, start.Add(3*time.Hour))
		if err != nil || decision.Finding != nil || !decision.DueAt.IsZero() {
			t.Fatalf("%s future specialized action duplicated: %+v %v", kind, decision, err)
		}
	}
}

func TestV2PromiseRequiresSemanticResolution(t *testing.T) {
	start := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	state := agreementState(start)
	state.ActiveRisks[TypePromiseNotFulfilled] = ActiveRiskSnapshot{
		TriggerMessageID: "trigger", TriggerAt: start, OutgoingAfterTrigger: true,
	}
	state.Agreements = []AgreementSignal{{Kind: "COMMITMENT", WaitingFor: "BUSINESS", Status: "PENDING",
		TriggerMessageID: "trigger", TriggerAt: start, TriggerText: "Подготовлю расчёт",
		Confidence: .95, AIRunID: "run"}}
	policy := PromiseNotFulfilledPolicy{}
	decision, err := policy.Evaluate(state, start.Add(61*time.Minute))
	if err != nil || decision.Resolve || decision.Finding == nil {
		t.Fatalf("unrelated outgoing fulfilled v2 promise: %+v %v", decision, err)
	}
	state.Agreements[0].Status = "RESOLVED"
	decision, err = policy.Evaluate(state, start.Add(61*time.Minute))
	if err != nil || !decision.Resolve || decision.Finding != nil {
		t.Fatalf("explicit fulfillment did not close v2 promise: %+v %v", decision, err)
	}
	state.AgreementsCurrent = false
	state.V2SnapshotUnavailable = true
	decision, err = policy.Evaluate(state, start.Add(61*time.Minute))
	if err != nil || decision.Resolve || decision.Finding != nil {
		t.Fatalf("stale v2 snapshot changed promise: %+v %v", decision, err)
	}
}

func TestV2TerminalBookingDoesNotBecomeAnotherRisk(t *testing.T) {
	start := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	state := agreementState(start)
	state.OpportunityStage = "BOOKING_INTENT"
	state.LastMeaningful = DirectionIncoming
	state.LastMeaningfulID = "reply"
	state.Agreements[0].EvidenceMessageIDs = []string{"trigger", "reply"}
	for _, status := range []string{"RESOLVED", "CANCELLED"} {
		state.Agreements[0].Status = status
		for _, policy := range []Policy{BookingNotConfirmedPolicy{}, NoResponsePolicy{}} {
			decision, err := policy.Evaluate(state, start.Add(3*time.Hour))
			if err != nil || decision.Finding != nil || !decision.Resolve {
				t.Fatalf("%s %s: %+v %v", status, policy.Type(), decision, err)
			}
		}
	}
	state.Agreements[0].Status = "PENDING"
	state.Agreements[0].WaitingFor = "BUSINESS"
	state.Agreements[0].TriggerMessageID = "reply"
	if decision, err := (NoResponsePolicy{}).Evaluate(state, start.Add(3*time.Hour)); err != nil || decision.Finding != nil {
		t.Fatalf("booking duplicate: %+v %v", decision, err)
	}
	if decision, err := (BookingNotConfirmedPolicy{}).Evaluate(state, start.Add(3*time.Hour)); err != nil || decision.Finding == nil {
		t.Fatalf("specialised booking lost: %+v %v", decision, err)
	}
	state.AgreementsCurrent = false
	state.V2SnapshotUnavailable = true
	if decision, err := (BookingNotConfirmedPolicy{}).Evaluate(state, start.Add(3*time.Hour)); err != nil || decision.Finding != nil || decision.Resolve {
		t.Fatalf("stale booking mutated: %+v %v", decision, err)
	}
}

func TestCustomerDeferralUsesExplicitDeadlineAndDoesNotBlameBusiness(t *testing.T) {
	start := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	state := agreementState(start)
	state.LastMeaningful = DirectionIncoming
	state.LastMeaningfulID = "trigger"
	state.Agreements[0].TriggerText = "Я подумаю и отвечу завтра до 15:00"
	decision, err := (UnfinishedAgreementPolicy{}).Evaluate(state, start.Add(3*time.Hour))
	if err != nil || decision.Finding != nil || !decision.DueAt.Equal(time.Date(2026, 10, 6, 15, 0, 0, 0, time.UTC)) {
		t.Fatalf("deferral: %+v %v", decision, err)
	}
	decision, err = (NoResponsePolicy{}).Evaluate(state, start.Add(3*time.Hour))
	if err != nil || decision.Finding != nil || !decision.Resolve {
		t.Fatalf("customer wait blamed business: %+v %v", decision, err)
	}
}

func TestResolutionDoesNotHideNewBusinessRequestInSameReply(t *testing.T) {
	start := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	state := agreementState(start)
	state.LastMeaningful = DirectionIncoming
	state.LastMeaningfulID = "reply"
	state.Agreements[0].Status = "RESOLVED"
	state.Agreements[0].EvidenceMessageIDs = []string{"trigger", "reply"}
	state.Agreements = append(state.Agreements, AgreementSignal{
		Kind: "PURCHASE_BLOCKER", WaitingFor: "BUSINESS", Status: "PENDING",
		TriggerMessageID: "reply", TriggerAt: start.Add(time.Minute), Confidence: .95, AIRunID: "run",
	})
	decision, err := (NoResponsePolicy{}).Evaluate(state, start.Add(3*time.Hour))
	if err != nil || decision.Finding == nil || decision.Resolve {
		t.Fatalf("new request hidden by previous resolution: %+v %v", decision, err)
	}
}
