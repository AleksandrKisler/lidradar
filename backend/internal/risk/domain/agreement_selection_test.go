package domain

import (
	"testing"
	"time"
)

func TestIndependentAgreementsDoNotDisplaceEachOther(t *testing.T) {
	start := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	for _, promise := range []bool{false, true} {
		state := agreementState(start)
		var policy Policy = UnfinishedAgreementPolicy{}
		kind := TypeUnfinishedAgreement
		if promise {
			policy = PromiseNotFulfilledPolicy{}
			kind = TypePromiseNotFulfilled
			state.Agreements[0].Kind = "COMMITMENT"
			state.Agreements[0].WaitingFor = "BUSINESS"
			state.Agreements[0].TriggerText = "Отправлю счёт"
		} else {
			state.Agreements[0].Kind = "RESCHEDULE"
		}
		second := state.Agreements[0]
		second.TriggerMessageID = "second"
		second.TriggerAt = start.Add(2 * time.Hour)
		second.TriggerText = "Проверю наличие"
		if !promise {
			second.Kind = "PURCHASE_BLOCKER"
			second.TriggerText = "Не получается оплатить"
		}
		state.Agreements = append(state.Agreements, second)
		at := start.Add(150 * time.Minute)
		for _, active := range []bool{false, true} {
			if active {
				state.ActiveRisks[kind] = ActiveRiskSnapshot{TriggerMessageID: "trigger", TriggerAt: start}
			}
			d, err := policy.Evaluate(state, at)
			if err != nil || d.Finding == nil || d.TriggerMessageID != "trigger" || d.Resolve {
				t.Fatalf("%s active=%v: %+v %v", kind, active, d, err)
			}
		}
		// Completing the newer item never completes the older one.
		state.Agreements[1].Status = "RESOLVED"
		if d, _ := policy.Evaluate(state, at); d.Finding == nil || d.Resolve || d.TriggerMessageID != "trigger" {
			t.Fatalf("%s newer completion hid older wait: %+v", kind, d)
		}
		state.Agreements[1].Status = "PENDING"
		state.Agreements[0].Status = "RESOLVED"
		d, err := policy.Evaluate(state, at)
		if err != nil || !d.Resolve || d.Finding != nil || d.TriggerMessageID != "second" {
			t.Fatalf("%s next pending not scheduled: %+v %v", kind, d, err)
		}
		delete(state.ActiveRisks, kind)
		if d, _ := policy.Evaluate(state, start.Add(5*time.Hour)); d.Finding == nil || d.TriggerMessageID != "second" {
			t.Fatalf("%s remaining wait lost: %+v", kind, d)
		}
	}
}

func TestAgreementSelectionUsesDeadlineAndSkipsOnlyOwnedCandidate(t *testing.T) {
	start := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	state := agreementState(start)
	state.Agreements[0].TriggerText = "Я подумаю и отвечу завтра до 15:00"
	second := state.Agreements[0]
	second.Kind = "PURCHASE_BLOCKER"
	second.WaitingFor = "BUSINESS"
	second.TriggerMessageID = "second"
	second.TriggerText = "Не получается оплатить"
	second.TriggerAt = start.Add(time.Hour)
	state.Agreements = append(state.Agreements, second)
	d, err := (UnfinishedAgreementPolicy{}).Evaluate(state, start.Add(4*time.Hour))
	if err != nil || d.Finding == nil || d.TriggerMessageID != "second" {
		t.Fatalf("earliest deadline not selected: %+v %v", d, err)
	}
	state.Agreements[0].TriggerText = "Подтвердите перенос"
	state.ActiveRisks[TypeNoResponse] = ActiveRiskSnapshot{TriggerMessageID: "second"}
	d, err = (UnfinishedAgreementPolicy{}).Evaluate(state, start.Add(4*time.Hour))
	if err != nil || d.Finding == nil || d.TriggerMessageID != "trigger" {
		t.Fatalf("owned candidate hid independent one: %+v %v", d, err)
	}
}

func TestClosedAgreementAnchorCannotReopen(t *testing.T) {
	start := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	for _, promise := range []bool{false, true} {
		state := agreementState(start)
		var policy Policy = UnfinishedAgreementPolicy{}
		kind := TypeUnfinishedAgreement
		if promise {
			policy = PromiseNotFulfilledPolicy{}
			kind = TypePromiseNotFulfilled
			state.Agreements[0].Kind = "COMMITMENT"
			state.Agreements[0].WaitingFor = "BUSINESS"
			state.Agreements[0].TriggerText = "Отправлю счёт"
		}
		state.ClosedAgreementTriggers = map[Type]map[string]bool{kind: {"trigger": true}}
		if d, _ := policy.Evaluate(state, start.Add(5*time.Hour)); d.Finding != nil || !d.DueAt.IsZero() {
			t.Fatalf("%s reopened closed anchor: %+v", kind, d)
		}
		second := state.Agreements[0]
		second.TriggerMessageID = "independent"
		state.Agreements = append(state.Agreements, second)
		if d, _ := policy.Evaluate(state, start.Add(5*time.Hour)); d.Finding == nil || d.TriggerMessageID != "independent" {
			t.Fatalf("%s closure hid other anchor: %+v", kind, d)
		}
	}
}
