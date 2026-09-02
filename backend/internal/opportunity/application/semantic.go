package application

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	eventsapplication "lidradar/backend/internal/events/application"
	eventsdomain "lidradar/backend/internal/events/domain"
	jobsdomain "lidradar/backend/internal/jobs/domain"
	"lidradar/backend/internal/opportunity/domain"
)

// AnalysisAppliedEventType повторяет тип события модуля AI. Модуль возможностей
// не зависит от AI напрямую: он читает только версионированный конверт.
const AnalysisAppliedEventType = "ai.analysis.applied.v1"

// BookingIntentFact — проверенный сильный факт намерения записаться.
// Уверенность передаётся десятичной строкой, чтобы попасть в историю этапа
// без двоичной арифметики.
type BookingIntentFact struct {
	RunID      string
	Confidence string
}

// PriceFact — проверенная названная компанией цена. Сумма и уверенность —
// десятичные строки для точной арифметики; валюта — трёхбуквенный код.
type PriceFact struct {
	RunID      string
	Confidence string
	Amount     string
	Currency   string
}

// SemanticFactSource читает производную проекцию AI. Возвращает факты только
// для указанного run: устаревший run не должен двигать сделку.
type SemanticFactSource interface {
	TrustedBookingIntent(ctx context.Context, tenantID, conversationID, runID string) (BookingIntentFact, bool, error)
	TrustedPriceMentioned(ctx context.Context, tenantID, conversationID, runID string) (PriceFact, bool, error)
}

type analysisAppliedData struct {
	ConversationID           string `json:"conversationId"`
	RunID                    string `json:"runId"`
	BaseConversationRevision int64  `json:"baseConversationRevision"`
	AnalysisThroughMessageID string `json:"analysisThroughMessageId"`
}

// AnalysisAppliedEventHandler переводит активную сделку переписки в
// BOOKING_INTENT по сильному проверенному факту AI (LR-BE-RM-013, ТЗ §26,
// источник истории AI). Правило R3 затем работает по стадии; AI не создаёт
// Risk и не принимает решение о нём. Переход только вперёд и идемпотентен:
// повторная доставка события находит сделку уже на нужной стадии.
func AnalysisAppliedEventHandler(
	repository domain.Repository,
	facts SemanticFactSource,
	ids IDs,
	now func() time.Time,
) eventsapplication.Handler {
	return func(ctx context.Context, event eventsdomain.Event) error {
		if repository == nil || facts == nil || ids == nil || now == nil {
			return jobsdomain.Permanent("BACKGROUND_CONFIGURATION_INVALID", errors.New("обработчик факта AI не настроен"))
		}
		var data analysisAppliedData
		if json.Unmarshal(event.Data, &data) != nil || event.AggregateType != "ai_run" ||
			event.AggregateID == "" || data.RunID != event.AggregateID || data.ConversationID == "" ||
			data.BaseConversationRevision < 1 || data.AnalysisThroughMessageID == "" {
			return jobsdomain.Permanent("INVALID_OUTBOX_PAYLOAD", errors.New("некорректное событие применённого анализа"))
		}
		price, priceFound, err := facts.TrustedPriceMentioned(ctx, event.TenantID, data.ConversationID, data.RunID)
		if err != nil {
			return jobsdomain.Retryable("SEMANTIC_FACT_UNAVAILABLE", err)
		}
		booking, bookingFound, err := facts.TrustedBookingIntent(ctx, event.TenantID, data.ConversationID, data.RunID)
		if err != nil {
			return jobsdomain.Retryable("SEMANTIC_FACT_UNAVAILABLE", err)
		}
		if !priceFound && !bookingFound {
			return nil
		}
		opportunity, found, err := repository.ActiveByConversation(ctx, event.TenantID, data.ConversationID)
		if err != nil {
			return jobsdomain.Retryable("OPPORTUNITY_UNAVAILABLE", err)
		}
		if !found {
			return nil
		}
		at := now().UTC()
		// Цена обрабатывается раньше намерения записаться: PRICE_SENT стоит в
		// машине этапов до BOOKING_INTENT, и обратный порядок потерял бы этап.
		if priceFound {
			if err := applyPrice(ctx, repository, ids, event.TenantID, opportunity, price, at); err != nil {
				return err
			}
			opportunity.Stage = maxStage(opportunity.Stage, domain.StagePriceSent)
		}
		if bookingFound {
			if err := transitionByFact(ctx, repository, ids, event.TenantID, opportunity, domain.StageBookingIntent,
				booking.Confidence, booking.RunID, at); err != nil {
				return err
			}
		}
		return nil
	}
}

// applyPrice переводит сделку в PRICE_SENT и записывает оценку выручки с
// защитой LR-BE-1807: сумма только из доверенного факта, только в валюте
// сделки, только с разбираемой десятичной суммой и не поверх более надёжной
// оценки. Неопределённая сумма никогда не выдумывается.
func applyPrice(
	ctx context.Context,
	repository domain.Repository,
	ids IDs,
	tenantID string,
	opportunity domain.Opportunity,
	price PriceFact,
	at time.Time,
) error {
	if err := transitionByFact(ctx, repository, ids, tenantID, opportunity, domain.StagePriceSent,
		price.Confidence, price.RunID, at); err != nil {
		return err
	}
	amount, amountErr := domain.ParsePotentialRevenue(price.Amount)
	confidence, confidenceErr := domain.ParseConfidence(price.Confidence)
	if amountErr != nil || confidenceErr != nil || price.Currency != opportunity.Currency {
		return nil
	}
	_, err := repository.UpdateEstimate(ctx, domain.EstimateUpdate{
		TenantID: tenantID, OpportunityID: opportunity.ID, Amount: amount,
		Confidence: confidence, Currency: price.Currency, At: at,
	})
	switch {
	case err == nil, errors.Is(err, domain.ErrNotFound):
		return nil
	case errors.Is(err, domain.ErrInvalid):
		return jobsdomain.Permanent("OPPORTUNITY_ESTIMATE_INVALID", err)
	default:
		return jobsdomain.Retryable("OPPORTUNITY_ESTIMATE_FAILED", err)
	}
}

// transitionByFact выполняет переход по проверенному факту AI только вперёд и
// идемпотентно: сделка на целевом или более позднем этапе не трогается.
func transitionByFact(
	ctx context.Context,
	repository domain.Repository,
	ids IDs,
	tenantID string,
	opportunity domain.Opportunity,
	target domain.Stage,
	rawConfidence, runID string,
	at time.Time,
) error {
	if opportunity.Stage == target || !opportunity.Stage.CanTransitionTo(target) {
		return nil
	}
	confidence, err := domain.ParseConfidence(rawConfidence)
	if err != nil {
		return jobsdomain.Permanent("SEMANTIC_FACT_INVALID", err)
	}
	historyID, err := ids.NewID()
	if err != nil {
		return jobsdomain.Retryable("HISTORY_ID_UNAVAILABLE", err)
	}
	_, _, err = repository.Transition(ctx, domain.TransitionCommand{
		TenantID: tenantID, OpportunityID: opportunity.ID, HistoryID: historyID,
		ToStage: target, Source: domain.SourceAI,
		Confidence: &confidence, AIRunID: &runID, At: at,
	})
	switch {
	case err == nil, errors.Is(err, domain.ErrInvalidTransition), errors.Is(err, domain.ErrNotFound):
		// Сделку успели закрыть или продвинуть дальше вручную — факт устарел.
		return nil
	case errors.Is(err, domain.ErrInvalid):
		return jobsdomain.Permanent("OPPORTUNITY_TRANSITION_INVALID", err)
	default:
		return jobsdomain.Retryable("OPPORTUNITY_TRANSITION_FAILED", err)
	}
}

// maxStage возвращает более поздний из двух активных этапов.
func maxStage(current, candidate domain.Stage) domain.Stage {
	if current.CanTransitionTo(candidate) && current != candidate {
		return candidate
	}
	return current
}
