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

// SemanticFactSource читает производную проекцию AI. Возвращает факт только
// для указанного run: устаревший run не должен двигать сделку.
type SemanticFactSource interface {
	TrustedBookingIntent(ctx context.Context, tenantID, conversationID, runID string) (BookingIntentFact, bool, error)
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
		fact, found, err := facts.TrustedBookingIntent(ctx, event.TenantID, data.ConversationID, data.RunID)
		if err != nil {
			return jobsdomain.Retryable("SEMANTIC_FACT_UNAVAILABLE", err)
		}
		if !found {
			return nil
		}
		opportunity, found, err := repository.ActiveByConversation(ctx, event.TenantID, data.ConversationID)
		if err != nil {
			return jobsdomain.Retryable("OPPORTUNITY_UNAVAILABLE", err)
		}
		if !found || opportunity.Stage == domain.StageBookingIntent ||
			!opportunity.Stage.CanTransitionTo(domain.StageBookingIntent) {
			return nil
		}
		confidence, err := domain.ParseConfidence(fact.Confidence)
		if err != nil {
			return jobsdomain.Permanent("SEMANTIC_FACT_INVALID", err)
		}
		historyID, err := ids.NewID()
		if err != nil {
			return jobsdomain.Retryable("HISTORY_ID_UNAVAILABLE", err)
		}
		runID := fact.RunID
		_, _, err = repository.Transition(ctx, domain.TransitionCommand{
			TenantID: event.TenantID, OpportunityID: opportunity.ID, HistoryID: historyID,
			ToStage: domain.StageBookingIntent, Source: domain.SourceAI,
			Confidence: &confidence, AIRunID: &runID, At: now().UTC(),
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
}
