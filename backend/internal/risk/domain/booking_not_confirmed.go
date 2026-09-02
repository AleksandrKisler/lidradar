package domain

import (
	"fmt"
	"time"
)

const (
	BookingNotConfirmedPolicyVersion = "booking-not-confirmed/v1"
	BookingConfirmationThreshold     = 30 * time.Minute
	StrongBookingIntentConfidence    = 0.85
)

// bookingEligibleStages — канонический список стадий R3 (LR-BE-RM-013).
// NEW и ENGAGED исключены: сильный факт намерения сначала переводит сделку в
// BOOKING_INTENT (источник AI), после чего правило работает по стадии.
// BOOKED, WON, LOST и ARCHIVED закрывают риск.
var bookingEligibleStages = map[string]struct{}{
	"QUALIFYING": {}, "PRICE_SENT": {}, "WAITING_CUSTOMER": {},
	"WAITING_BUSINESS": {}, "BOOKING_INTENT": {},
}

// BookingRiskEligible сообщает, может ли правило R3 открыть риск на стадии.
func BookingRiskEligible(stage string) bool {
	_, eligible := bookingEligibleStages[stage]
	return eligible
}

type BookingNotConfirmedPolicy struct{}

func (BookingNotConfirmedPolicy) Type() Type { return TypeBookingNotConfirmed }
func (BookingNotConfirmedPolicy) Version() string {
	return BookingNotConfirmedPolicyVersion
}

// Evaluate применяет только правило R3. AI сообщает смысловой факт, но не
// принимает решение: порог, ожидание бизнеса, рабочее время и итоговый Risk
// вычисляются здесь детерминированно.
func (policy BookingNotConfirmedPolicy) Evaluate(state ConversationState, at time.Time) (Decision, error) {
	if err := validateState(state); err != nil || at.IsZero() {
		return Decision{}, ErrInvalidRisk
	}
	if !state.ActiveOpportunity || state.OpportunityStage == "BOOKED" ||
		state.OpportunityStage == "WON" || state.OpportunityStage == "LOST" ||
		state.OpportunityStage == "ARCHIVED" {
		return Decision{Resolve: true}, nil
	}
	if !BookingRiskEligible(state.OpportunityStage) {
		return Decision{}, nil
	}

	// waitingFor = BUSINESS выводится из последнего канонического направления.
	// Ответ бизнеса не подтверждает запись и потому сам по себе не закрывает
	// уже открытый риск, но новую проверку после него не создаёт.
	if state.LastMeaningful != DirectionIncoming {
		return Decision{}, nil
	}

	triggerID := ""
	var triggerAt time.Time
	source := SourceRule
	var confidence *float64
	var aiRunID *string
	if signal := state.BookingIntent; signal != nil && signal.Value &&
		signal.Confidence >= StrongBookingIntentConfidence {
		if signal.AIRunID == "" || signal.EvidenceMessageID == "" || signal.EvidenceAt.IsZero() {
			return Decision{}, ErrInvalidRisk
		}
		triggerID, triggerAt, source = signal.EvidenceMessageID, signal.EvidenceAt, SourceHybrid
		value, runID := signal.Confidence, signal.AIRunID
		confidence, aiRunID = &value, &runID
	} else if state.OpportunityStage == "BOOKING_INTENT" {
		triggerID, triggerAt = state.LastMeaningfulID, state.LastMeaningfulAt
	} else {
		// Отсутствующий или недостаточно уверенный факт не меняет предметное
		// состояние, в том числе не закрывает ранее открытый риск.
		return Decision{}, nil
	}

	due, err := state.BusinessHours.AddBusinessTime(triggerAt, BookingConfirmationThreshold)
	if err != nil {
		return Decision{}, err
	}
	decision := Decision{DueAt: due}
	if at.Before(due) {
		return decision, nil
	}
	reason := "Возможность находится на этапе намерения записаться, но запись не подтверждена в течение 30 рабочих минут"
	if source == SourceHybrid {
		reason = fmt.Sprintf(
			"Клиент выразил намерение записаться с уверенностью %.3f, но запись не подтверждена в течение 30 рабочих минут",
			*confidence,
		)
	}
	decision.Finding = &Finding{
		TenantID: state.TenantID, OpportunityID: state.OpportunityID,
		LocationID: state.LocationID, TriggerMessageID: triggerID,
		Severity: SeverityCritical, PolicyVersion: policy.Version(),
		ReasonCode: "BOOKING_NOT_CONFIRMED_AFTER_INTENT", Reason: reason,
		DueAt: due, Source: source, Confidence: confidence, AIRunID: aiRunID,
	}
	return decision, nil
}
