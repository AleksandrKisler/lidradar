package domain

import (
	"strings"
	"time"
)

// Verdict — оценка пользователя: риск был настоящим или ложным (LR-BE-2101).
type Verdict string

const (
	VerdictTruePositive  Verdict = "TRUE_POSITIVE"
	VerdictFalsePositive Verdict = "FALSE_POSITIVE"
)

func ValidVerdict(verdict Verdict) bool {
	return verdict == VerdictTruePositive || verdict == VerdictFalsePositive
}

// FeedbackReason объясняет вердикт (LR-BE-2102); для ложного срабатывания
// причина обязательна.
type FeedbackReason string

const (
	ReasonCustomerAlreadyBooked   FeedbackReason = "CUSTOMER_ALREADY_BOOKED"
	ReasonCustomerAlreadyAnswered FeedbackReason = "CUSTOMER_ALREADY_ANSWERED"
	ReasonNotALead                FeedbackReason = "NOT_A_LEAD"
	ReasonCustomerRejected        FeedbackReason = "CUSTOMER_REJECTED"
	ReasonWrongInterpretation     FeedbackReason = "WRONG_INTERPRETATION"
	ReasonOther                   FeedbackReason = "OTHER"
)

func ValidFeedbackReason(reason FeedbackReason) bool {
	switch reason {
	case ReasonCustomerAlreadyBooked, ReasonCustomerAlreadyAnswered, ReasonNotALead,
		ReasonCustomerRejected, ReasonWrongInterpretation, ReasonOther:
		return true
	default:
		return false
	}
}

// Types перечисляет типы рисков в порядке ТЗ §27.
func Types() []Type {
	return []Type{
		TypeNoResponse, TypeCustomerSilentAfterPrice, TypeBookingNotConfirmed,
		TypePromiseNotFulfilled, TypeFollowUpCandidate,
	}
}

// FeedbackContext — неизменяемый снимок риска на момент вердикта
// (LR-BE-2104): последующие изменения риска и сделки его не переписывают.
type FeedbackContext struct {
	Type             Type      `json:"type"`
	Severity         Severity  `json:"severity"`
	Status           Status    `json:"status"`
	Source           Source    `json:"source"`
	PolicyVersion    string    `json:"policyVersion"`
	AIRunID          *string   `json:"aiRunId,omitempty"`
	TriggerMessageID string    `json:"triggerMessageId"`
	OpportunityStage string    `json:"opportunityStage"`
	DetectedAt       time.Time `json:"detectedAt"`
}

// Feedback — append-only факт обратной связи. DatasetEligible фиксирует,
// действовало ли явное ML-согласие организации в момент записи (ТЗ §70).
type Feedback struct {
	ID              string          `json:"id"`
	TenantID        string          `json:"-"`
	RiskID          string          `json:"riskId"`
	OpportunityID   string          `json:"opportunityId"`
	ActorID         string          `json:"actorId"`
	Verdict         Verdict         `json:"verdict"`
	Reason          FeedbackReason  `json:"reason,omitempty"`
	Note            string          `json:"note"`
	Context         FeedbackContext `json:"context"`
	DatasetEligible bool            `json:"datasetEligible"`
	CreatedAt       time.Time       `json:"createdAt"`
}

func NewFeedback(
	id string,
	risk Risk,
	opportunityStage, actorID string,
	verdict Verdict,
	reason FeedbackReason,
	note string,
	datasetEligible bool,
	at time.Time,
) (Feedback, error) {
	if risk.Validate() != nil {
		return Feedback{}, ErrInvalidRisk
	}
	feedback := Feedback{
		ID: id, TenantID: risk.TenantID, RiskID: risk.ID, OpportunityID: risk.OpportunityID, ActorID: actorID,
		Verdict: verdict, Reason: reason, Note: strings.TrimSpace(note),
		Context: FeedbackContext{
			Type: risk.Type, Severity: risk.Severity, Status: risk.Status, Source: risk.Source,
			PolicyVersion: risk.PolicyVersion, AIRunID: risk.AIRunID, TriggerMessageID: risk.TriggerMessageID,
			OpportunityStage: strings.TrimSpace(opportunityStage), DetectedAt: risk.DetectedAt,
		},
		DatasetEligible: datasetEligible, CreatedAt: at.UTC(),
	}
	if feedback.Validate() != nil {
		return Feedback{}, ErrInvalidRisk
	}
	return feedback, nil
}

func (feedback Feedback) Validate() error {
	if feedback.ID == "" || feedback.TenantID == "" || feedback.RiskID == "" || feedback.OpportunityID == "" ||
		feedback.ActorID == "" || !ValidVerdict(feedback.Verdict) || len(feedback.Note) > 1000 ||
		feedback.Note != strings.TrimSpace(feedback.Note) || feedback.CreatedAt.IsZero() ||
		!SupportedType(feedback.Context.Type) || feedback.Context.PolicyVersion == "" ||
		feedback.Context.TriggerMessageID == "" || feedback.Context.OpportunityStage == "" || feedback.Context.DetectedAt.IsZero() {
		return ErrInvalidRisk
	}
	if feedback.Reason != "" && !ValidFeedbackReason(feedback.Reason) {
		return ErrInvalidRisk
	}
	if feedback.Verdict == VerdictFalsePositive && feedback.Reason == "" {
		return ErrInvalidRisk
	}
	return nil
}

// CorrectsLead сообщает, что пользователь признал переписку не лидом
// (LR-BE-2103): сделка закрывается, а не только риск.
func (feedback Feedback) CorrectsLead() bool {
	return feedback.Verdict == VerdictFalsePositive && feedback.Reason == ReasonNotALead
}

// MarkFalsePositive закрывает активный риск как ложное срабатывание. Уже
// закрытый риск не меняется: история остаётся такой, какой была.
func (r *Risk) MarkFalsePositive(now time.Time) error {
	if now.IsZero() {
		return ErrInvalidRisk
	}
	if !r.Active() {
		return nil
	}
	at := now.UTC()
	r.Status = StatusFalsePositive
	r.ResolvedAt = &at
	r.UpdatedAt = at
	return nil
}

// MilestoneCoverage — минимальная доля рисков с обратной связью, при которой
// precision считается надёжной для критерия Milestone E (LR-BE-RM-019).
const MilestoneCoverage = 0.5

// PrecisionRow — метрика по типу риска за окно обнаружения: каждый риск
// учитывается один раз по последнему вердикту.
type PrecisionRow struct {
	Type           Type
	TotalRisks     int
	WithFeedback   int
	TruePositives  int
	FalsePositives int
}

func ratio(numerator, denominator int) *float64 {
	if denominator == 0 {
		return nil
	}
	value := float64(numerator) / float64(denominator)
	return &value
}

// Precision = TP / (TP + FP); nil без оценённых рисков.
func (row PrecisionRow) Precision() *float64 {
	return ratio(row.TruePositives, row.TruePositives+row.FalsePositives)
}

// FalsePositiveRate = FP / (TP + FP); nil без оценённых рисков.
func (row PrecisionRow) FalsePositiveRate() *float64 {
	return ratio(row.FalsePositives, row.TruePositives+row.FalsePositives)
}

// CoverageRate = риски с обратной связью / все риски окна; ноль без рисков.
func (row PrecisionRow) CoverageRate() float64 {
	if row.TotalRisks == 0 {
		return 0
	}
	return float64(row.WithFeedback) / float64(row.TotalRisks)
}

// Reliable сообщает, достаточно ли покрытие, чтобы precision не была смещена выборкой.
func (row PrecisionRow) Reliable() bool {
	return row.WithFeedback > 0 && row.CoverageRate() >= MilestoneCoverage
}
