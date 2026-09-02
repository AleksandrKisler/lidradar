package domain

import (
	"fmt"
	"time"
)

const (
	CustomerSilentAfterPricePolicyVersion = "customer-silent-after-price/v1"
	// PriceSilenceThreshold и PriceSilenceEscalation — канон ТЗ §30 и
	// LR-BE-RM-012: 24 рабочих часа → MEDIUM, 48 рабочих часов → HIGH. Единица
	// (рабочее время) зафиксирована ADR 0035; переход на календарное время —
	// открытый вопрос владельца продукта и меняет только данные конфигурации.
	PriceSilenceThreshold  = 24 * time.Hour
	PriceSilenceEscalation = 48 * time.Hour
	StrongPriceConfidence  = 0.85
	// EscalationCheckSuffix отличает проверку эскалации важности от первой
	// проверки того же сообщения-основания.
	EscalationCheckSuffix = "escalation"
)

// PriceSignal — проверенный факт PRICE_MENTIONED: компания назвала цену в
// исходящем сообщении. IncomingAfter сообщает, писал ли клиент после него.
type PriceSignal struct {
	Value             bool
	Confidence        float64
	AIRunID           string
	Amount            string
	Currency          string
	EvidenceMessageID string
	EvidenceAt        time.Time
	IncomingAfter     bool
}

// MessageRef — ссылка на сообщение переписки с моментом отправки.
type MessageRef struct {
	ID string
	At time.Time
}

// customerRejectedOutcomes — деривация «CUSTOMER_REJECTED» этапа 18: явный
// отказ клиента фиксируется исходом LOST или NOT_A_LEAD. Смысловое
// обнаружение отказа добавляет этап 19 (LR-BE-1902).
var customerRejectedOutcomes = map[string]struct{}{"LOST": {}, "NOT_A_LEAD": {}}

// CustomerRejected сообщает, зафиксирован ли явный отказ клиента.
func CustomerRejected(latestOutcome string) bool {
	_, rejected := customerRejectedOutcomes[latestOutcome]
	return rejected
}

type CustomerSilentAfterPricePolicy struct{}

func (CustomerSilentAfterPricePolicy) Type() Type { return TypeCustomerSilentAfterPrice }
func (CustomerSilentAfterPricePolicy) Version() string {
	return CustomerSilentAfterPricePolicyVersion
}

// Evaluate применяет правило R2 (ТЗ §30): после исходящего сообщения с ценой
// нет входящего клиента. Основание — доказательство факта PRICE_MENTIONED
// (источник HYBRID) либо последнее исходящее сообщение на этапе PRICE_SENT
// (источник RULE). Первая проверка через 24 рабочих часа открывает MEDIUM и
// назначает проверку эскалации; 48 рабочих часов поднимают важность до HIGH.
func (policy CustomerSilentAfterPricePolicy) Evaluate(state ConversationState, at time.Time) (Decision, error) {
	if err := validateState(state); err != nil || at.IsZero() {
		return Decision{}, ErrInvalidRisk
	}
	if !state.ActiveOpportunity || state.OpportunityStage == "BOOKED" ||
		state.OpportunityStage == "WON" || state.OpportunityStage == "LOST" ||
		state.OpportunityStage == "ARCHIVED" || CustomerRejected(state.LatestOutcome) {
		return Decision{Resolve: true}, nil
	}
	active, hasActive := state.ActiveRisks[TypeCustomerSilentAfterPrice]

	var trigger MessageRef
	source := SourceRule
	var confidence *float64
	var runID *string
	amount := ""
	if signal := state.Price; signal != nil && signal.Value && signal.Confidence >= StrongPriceConfidence {
		if signal.AIRunID == "" || signal.EvidenceMessageID == "" || signal.EvidenceAt.IsZero() {
			return Decision{}, ErrInvalidRisk
		}
		if signal.IncomingAfter {
			return Decision{Resolve: true, TriggerMessageID: signal.EvidenceMessageID}, nil
		}
		trigger, source = MessageRef{ID: signal.EvidenceMessageID, At: signal.EvidenceAt}, SourceHybrid
		value, run := signal.Confidence, signal.AIRunID
		confidence, runID = &value, &run
		if signal.Amount != "" && signal.Currency != "" {
			amount = signal.Amount + " " + signal.Currency
		}
	} else if state.OpportunityStage == "PRICE_SENT" {
		if state.LastMeaningful != DirectionOutgoing {
			return Decision{Resolve: true}, nil
		}
		if state.LastOutgoing == nil {
			return Decision{}, nil
		}
		trigger = *state.LastOutgoing
	} else {
		// Без контекста цены правило домен не меняет; уже открытый риск
		// закрывается, если клиент ответил после его основания.
		return Decision{Resolve: hasActive && active.IncomingAfterTrigger}, nil
	}
	// Клиент ответил на прежнюю цену, а новая цена отправлена позже: старый
	// риск закрывается, новое основание получает собственный срок.
	due, err := state.BusinessHours.AddBusinessTime(trigger.At, PriceSilenceThreshold)
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
	severity := SeverityMedium
	if elapsed >= PriceSilenceEscalation {
		severity = SeverityHigh
	} else {
		escalation, err := state.BusinessHours.AddBusinessTime(trigger.At, PriceSilenceEscalation)
		if err != nil {
			return Decision{}, err
		}
		decision.NextDueAt, decision.NextCheckSuffix = escalation, EscalationCheckSuffix
	}
	reason := fmt.Sprintf("Клиент не ответил в течение %d рабочих часов после отправленной цены", int(elapsed/time.Hour))
	if amount != "" {
		reason = fmt.Sprintf("Клиент не ответил в течение %d рабочих часов после отправленной цены %s", int(elapsed/time.Hour), amount)
	}
	decision.Finding = &Finding{
		TenantID: state.TenantID, OpportunityID: state.OpportunityID,
		LocationID: state.LocationID, TriggerMessageID: trigger.ID,
		Severity: severity, PolicyVersion: policy.Version(),
		ReasonCode: "CUSTOMER_SILENT_AFTER_PRICE_THRESHOLD_EXCEEDED", Reason: reason,
		DueAt: due, Source: source, Confidence: confidence, AIRunID: runID,
	}
	return decision, nil
}
