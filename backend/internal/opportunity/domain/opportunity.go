// Package domain содержит коммерческую возможность, её этапы и неизменяемую историю.
package domain

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

var (
	ErrInvalid           = errors.New("некорректная коммерческая возможность")
	ErrNotFound          = errors.New("коммерческая возможность не найдена")
	ErrConflict          = errors.New("конфликт коммерческой возможности")
	ErrInvalidTransition = errors.New("недопустимый переход этапа")
)

const (
	CreatedEventName      = "opportunity.created"
	StageChangedEventName = "opportunity.stage_changed"
)

// Stage — самостоятельный коммерческий этап. Риск не является этапом сделки.
type Stage string

const (
	StageNew             Stage = "NEW"
	StageEngaged         Stage = "ENGAGED"
	StageQualifying      Stage = "QUALIFYING"
	StagePriceSent       Stage = "PRICE_SENT"
	StageWaitingCustomer Stage = "WAITING_CUSTOMER"
	StageWaitingBusiness Stage = "WAITING_BUSINESS"
	StageBookingIntent   Stage = "BOOKING_INTENT"
	StageBooked          Stage = "BOOKED"
	StageWon             Stage = "WON"
	StageLost            Stage = "LOST"
	StageArchived        Stage = "ARCHIVED"
)

var activeStageOrder = map[Stage]int{
	StageNew: 0, StageEngaged: 1, StageQualifying: 2, StagePriceSent: 3,
	StageWaitingCustomer: 4, StageWaitingBusiness: 5, StageBookingIntent: 6, StageBooked: 7,
}

func (stage Stage) Valid() bool {
	if _, ok := activeStageOrder[stage]; ok {
		return true
	}
	return stage == StageWon || stage == StageLost || stage == StageArchived
}

func (stage Stage) Active() bool {
	_, ok := activeStageOrder[stage]
	return ok
}

// CanTransitionTo разрешает пропуск этапов вперёд, но запрещает откат и
// повторное открытие закрытой возможности. Повтор текущего этапа идемпотентен.
func (stage Stage) CanTransitionTo(next Stage) bool {
	if !stage.Valid() || !next.Valid() {
		return false
	}
	if stage == next {
		return true
	}
	if currentOrder, active := activeStageOrder[stage]; active {
		if nextOrder, nextActive := activeStageOrder[next]; nextActive {
			return nextOrder > currentOrder
		}
		if next == StageLost {
			return true
		}
		return stage == StageBooked && next == StageWon
	}
	return (stage == StageWon || stage == StageLost) && next == StageArchived
}

var amountPattern = regexp.MustCompile(`^[0-9]{1,12}(?:\.[0-9]{1,2})?$`)

// PotentialRevenue — необязательная точная денежная оценка NUMERIC(14,2).
// Отсутствующее надёжное значение представляется nil, а не нулём.
type PotentialRevenue struct{ value decimal.Decimal }

func ParsePotentialRevenue(raw string) (PotentialRevenue, error) {
	raw = strings.TrimSpace(raw)
	if !amountPattern.MatchString(raw) {
		return PotentialRevenue{}, ErrInvalid
	}
	value, err := decimal.NewFromString(raw)
	if err != nil || value.IsNegative() {
		return PotentialRevenue{}, ErrInvalid
	}
	return PotentialRevenue{value: value}, nil
}

func (revenue PotentialRevenue) String() string { return revenue.value.StringFixed(2) }

func (revenue PotentialRevenue) MarshalJSON() ([]byte, error) { return json.Marshal(revenue.String()) }

// Confidence хранится как NUMERIC(4,3) и не использует двоичную арифметику.
type Confidence struct{ value decimal.Decimal }

func ParseConfidence(raw string) (Confidence, error) {
	value, err := decimal.NewFromString(strings.TrimSpace(raw))
	if err != nil || value.IsNegative() || value.GreaterThan(decimal.NewFromInt(1)) || value.Exponent() < -3 {
		return Confidence{}, ErrInvalid
	}
	return Confidence{value: value}, nil
}

func (confidence Confidence) String() string { return confidence.value.StringFixed(3) }

func (confidence Confidence) MarshalJSON() ([]byte, error) { return []byte(confidence.String()), nil }

// Opportunity — отдельный от переписки коммерческий агрегат.
type Opportunity struct {
	ID                        string            `json:"id"`
	TenantID                  string            `json:"-"`
	ConversationID            string            `json:"conversationId"`
	ServiceID                 *string           `json:"serviceId"`
	Stage                     Stage             `json:"stage"`
	EstimatedAmount           *PotentialRevenue `json:"estimatedAmount"`
	EstimatedAmountConfidence *Confidence       `json:"estimatedAmountConfidence"`
	Currency                  string            `json:"currency"`
	OpenedAt                  time.Time         `json:"openedAt"`
	ClosedAt                  *time.Time        `json:"closedAt"`
	CreatedAt                 time.Time         `json:"createdAt"`
	UpdatedAt                 time.Time         `json:"updatedAt"`
}

func NewOpportunity(
	id, tenantID, conversationID string,
	serviceID *string,
	estimatedAmount *PotentialRevenue,
	estimatedConfidence *Confidence,
	currency string,
	at time.Time,
) (Opportunity, error) {
	opportunity := Opportunity{
		ID: id, TenantID: tenantID, ConversationID: conversationID, ServiceID: cleanOptional(serviceID), Stage: StageNew,
		EstimatedAmount: estimatedAmount, EstimatedAmountConfidence: estimatedConfidence,
		Currency: strings.ToUpper(strings.TrimSpace(currency)), OpenedAt: at.UTC(), CreatedAt: at.UTC(), UpdatedAt: at.UTC(),
	}
	if opportunity.Currency == "" {
		opportunity.Currency = "RUB"
	}
	if opportunity.Validate() != nil {
		return Opportunity{}, ErrInvalid
	}
	return opportunity, nil
}

func (opportunity Opportunity) Validate() error {
	if opportunity.ID == "" || opportunity.TenantID == "" || opportunity.ConversationID == "" ||
		!opportunity.Stage.Valid() || len(opportunity.Currency) != 3 || opportunity.OpenedAt.IsZero() ||
		opportunity.CreatedAt.IsZero() || opportunity.UpdatedAt.IsZero() {
		return ErrInvalid
	}
	for _, character := range opportunity.Currency {
		if character < 'A' || character > 'Z' {
			return ErrInvalid
		}
	}
	if opportunity.ServiceID != nil && (*opportunity.ServiceID == "" || *opportunity.ServiceID != strings.TrimSpace(*opportunity.ServiceID)) {
		return ErrInvalid
	}
	if opportunity.EstimatedAmount == nil && opportunity.EstimatedAmountConfidence != nil {
		return ErrInvalid
	}
	if opportunity.Stage.Active() != (opportunity.ClosedAt == nil) {
		return ErrInvalid
	}
	return nil
}

// HistorySource фиксирует происхождение решения об этапе.
type HistorySource string

const (
	SourceRule   HistorySource = "RULE"
	SourceAI     HistorySource = "AI"
	SourceUser   HistorySource = "USER"
	SourceImport HistorySource = "IMPORT"
)

func (source HistorySource) Valid() bool {
	return source == SourceRule || source == SourceAI || source == SourceUser || source == SourceImport
}

// StageHistory — добавляемая только в конец запись изменения этапа.
type StageHistory struct {
	ID            string        `json:"id"`
	TenantID      string        `json:"-"`
	OpportunityID string        `json:"opportunityId"`
	FromStage     *Stage        `json:"fromStage"`
	ToStage       Stage         `json:"toStage"`
	Source        HistorySource `json:"source"`
	Confidence    *Confidence   `json:"confidence"`
	AIRunID       *string       `json:"aiRunId"`
	ActorUserID   *string       `json:"actorUserId"`
	CreatedAt     time.Time     `json:"createdAt"`
}

func NewHistory(
	id, tenantID, opportunityID string,
	from *Stage,
	to Stage,
	source HistorySource,
	confidence *Confidence,
	aiRunID, actorUserID *string,
	at time.Time,
) (StageHistory, error) {
	history := StageHistory{
		ID: id, TenantID: tenantID, OpportunityID: opportunityID, FromStage: from, ToStage: to,
		Source: source, Confidence: confidence, AIRunID: cleanOptional(aiRunID),
		ActorUserID: cleanOptional(actorUserID), CreatedAt: at.UTC(),
	}
	if history.Validate() != nil {
		return StageHistory{}, ErrInvalid
	}
	return history, nil
}

func (history StageHistory) Validate() error {
	if history.ID == "" || history.TenantID == "" || history.OpportunityID == "" || !history.ToStage.Valid() ||
		!history.Source.Valid() || history.CreatedAt.IsZero() {
		return ErrInvalid
	}
	if history.FromStage != nil && (!history.FromStage.Valid() || *history.FromStage == history.ToStage) {
		return ErrInvalid
	}
	if history.Source == SourceUser && history.ActorUserID == nil {
		return ErrInvalid
	}
	if history.Source == SourceAI && history.Confidence == nil {
		return ErrInvalid
	}
	return nil
}

type Detail struct {
	Opportunity Opportunity    `json:"opportunity"`
	History     []StageHistory `json:"stageHistory"`
}

type TransitionCommand struct {
	TenantID      string
	OpportunityID string
	HistoryID     string
	ToStage       Stage
	Source        HistorySource
	Confidence    *Confidence
	AIRunID       *string
	ActorUserID   *string
	At            time.Time
}

func (command TransitionCommand) Validate() error {
	if command.TenantID == "" || command.OpportunityID == "" || command.HistoryID == "" || command.At.IsZero() {
		return ErrInvalid
	}
	_, err := NewHistory(command.HistoryID, command.TenantID, command.OpportunityID, nil, command.ToStage,
		command.Source, command.Confidence, command.AIRunID, command.ActorUserID, command.At)
	return err
}

// Repository требует tenant во всех обращениях и атомарно связывает агрегат с историей.
type Repository interface {
	Create(context.Context, Opportunity, StageHistory) (Opportunity, bool, error)
	Detail(context.Context, string, string) (Detail, bool, error)
	Transition(context.Context, TransitionCommand) (Opportunity, bool, error)
	// ActiveByConversation находит единственную активную сделку переписки:
	// стадия не входит в WON, LOST, ARCHIVED (ТЗ §26).
	ActiveByConversation(context.Context, string, string) (Opportunity, bool, error)
	// UpdateEstimate записывает извлечённую AI оценку выручки только если
	// валюта совпадает, сделка активна и текущая оценка не надёжнее новой
	// (LR-BE-1807). Возвращает false, если запись отклонена защитой.
	UpdateEstimate(context.Context, EstimateUpdate) (bool, error)
}

// EstimateUpdate — извлечённая из переписки оценка потенциальной выручки.
// Сумма и уверенность уже разобраны точной десятичной арифметикой; источник
// AI_EXTRACTION остаётся оценкой и никогда не считается фактической выручкой.
type EstimateUpdate struct {
	TenantID, OpportunityID string
	Amount                  PotentialRevenue
	Confidence              Confidence
	Currency                string
	At                      time.Time
}

func cleanOptional(value *string) *string {
	if value == nil {
		return nil
	}
	cleaned := strings.TrimSpace(*value)
	return &cleaned
}
