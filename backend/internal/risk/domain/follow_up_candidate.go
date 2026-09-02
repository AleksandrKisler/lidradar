package domain

import (
	"fmt"
	"time"
)

const (
	FollowUpCandidatePolicyVersion = "follow-up-candidate/v1"
	// FollowUpDelay — канон ТЗ §33 и LR-BE-1904: 24 рабочих часа после
	// колебания клиента без его возвращения. Единица — рабочее время
	// (ADR 0035; календарное время для R5 — открытый вопрос владельца).
	FollowUpDelay            = 24 * time.Hour
	StrongFollowUpConfidence = 0.85
)

// FollowUpSignal — проверенный факт FOLLOW_UP_CANDIDATE: клиент откладывает
// решение, но допускает продолжение разговора. Доказательство — входящее
// сообщение; IncomingAfter сообщает, вернулся ли клиент после него.
type FollowUpSignal struct {
	Value             bool
	Confidence        float64
	AIRunID           string
	EvidenceMessageID string
	EvidenceAt        time.Time
	IncomingAfter     bool
}

type FollowUpCandidatePolicy struct{}

func (FollowUpCandidatePolicy) Type() Type { return TypeFollowUpCandidate }
func (FollowUpCandidatePolicy) Version() string {
	return FollowUpCandidatePolicyVersion
}

// Evaluate применяет правило R5 (ТЗ §33): клиент колеблется, сделка ждёт
// клиента, явного отказа нет, и клиент не вернулся за 24 рабочих часа.
// Основание — доказательство факта (источник HYBRID) либо последнее входящее
// сообщение на этапе WAITING_CUSTOMER без факта (источник RULE). Риск —
// мягкий кандидат на продолжение: важность MEDIUM, доставка дайджестом.
func (policy FollowUpCandidatePolicy) Evaluate(state ConversationState, at time.Time) (Decision, error) {
	if err := validateState(state); err != nil || at.IsZero() {
		return Decision{}, ErrInvalidRisk
	}
	if !state.ActiveOpportunity || state.OpportunityStage == "BOOKED" ||
		state.OpportunityStage == "WON" || state.OpportunityStage == "LOST" ||
		state.OpportunityStage == "ARCHIVED" || CustomerRejected(state.LatestOutcome) {
		return Decision{Resolve: true}, nil
	}
	active, hasActive := state.ActiveRisks[TypeFollowUpCandidate]

	var trigger MessageRef
	source := SourceRule
	var confidence *float64
	var runID *string
	if signal := state.FollowUp; signal != nil && signal.Value && signal.Confidence >= StrongFollowUpConfidence {
		if signal.AIRunID == "" || signal.EvidenceMessageID == "" || signal.EvidenceAt.IsZero() {
			return Decision{}, ErrInvalidRisk
		}
		if signal.IncomingAfter {
			return Decision{Resolve: true, TriggerMessageID: signal.EvidenceMessageID}, nil
		}
		trigger, source = MessageRef{ID: signal.EvidenceMessageID, At: signal.EvidenceAt}, SourceHybrid
		value, run := signal.Confidence, signal.AIRunID
		confidence, runID = &value, &run
	} else if state.OpportunityStage == "WAITING_CUSTOMER" {
		if state.LastIncoming == nil {
			return Decision{}, nil
		}
		trigger = *state.LastIncoming
		if state.LastMeaningful == DirectionIncoming && state.LastMeaningfulID != trigger.ID {
			return Decision{Resolve: true}, nil
		}
	} else {
		return Decision{Resolve: hasActive && active.IncomingAfterTrigger}, nil
	}

	due, err := state.BusinessHours.AddBusinessTime(trigger.At, FollowUpDelay)
	if err != nil {
		return Decision{}, err
	}
	decision := Decision{
		DueAt: due, TriggerMessageID: trigger.ID,
		Resolve: hasActive && active.IncomingAfterTrigger && active.TriggerMessageID != trigger.ID,
	}
	if at.Before(due) {
		return decision, nil
	}
	elapsed, err := state.BusinessHours.ElapsedBusinessTime(trigger.At, at)
	if err != nil {
		return Decision{}, err
	}
	decision.Finding = &Finding{
		TenantID: state.TenantID, OpportunityID: state.OpportunityID,
		LocationID: state.LocationID, TriggerMessageID: trigger.ID,
		Severity: SeverityMedium, PolicyVersion: policy.Version(),
		ReasonCode: "FOLLOW_UP_CANDIDATE_AFTER_HESITATION",
		Reason: fmt.Sprintf(
			"Клиент отложил решение и не вернулся в течение %d рабочих часов; стоит уточнить, остаётся ли услуга актуальной",
			int(elapsed/time.Hour),
		),
		DueAt: due, Source: source, Confidence: confidence, AIRunID: runID,
	}
	return decision, nil
}
