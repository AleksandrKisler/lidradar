package application

import (
	"context"
	"encoding/json"
	"errors"

	eventsapplication "lidradar/backend/internal/events/application"
	eventsdomain "lidradar/backend/internal/events/domain"
	jobsdomain "lidradar/backend/internal/jobs/domain"
)

const ConversationChangedEventType = "conversation.changed.v1"

type conversationChangedData struct {
	ConversationID string `json:"conversationId"`
	MessageID      string `json:"messageId"`
	Revision       int64  `json:"revision"`
}

// ConversationChangedEventHandler ставит свежий анализ переписки в очередь
// AI. Повтор события безопасен: уникальность задания задаётся снимком
// переписки и версиями модели, схемы и инструкции.
func ConversationChangedEventHandler(service Service, builder StaleJobBuilder) eventsapplication.Handler {
	return func(ctx context.Context, event eventsdomain.Event) error {
		var data conversationChangedData
		if builder == nil || service.store == nil {
			return jobsdomain.Permanent("AI_CONFIGURATION_INVALID", errors.New("анализ переписки не настроен"))
		}
		if json.Unmarshal(event.Data, &data) != nil || data.ConversationID == "" ||
			data.MessageID == "" || data.Revision < 1 || event.AggregateType != "conversation" ||
			event.AggregateID != data.ConversationID {
			return jobsdomain.Permanent("INVALID_OUTBOX_PAYLOAD", errors.New("некорректное событие переписки для AI"))
		}
		command, err := builder.BuildAnalysisJob(ctx, event.TenantID, data.ConversationID)
		if errors.Is(err, ErrInvalid) || errors.Is(err, ErrNotFound) {
			// После удаления последнего текстового сообщения анализировать нечего.
			return nil
		}
		if err != nil {
			return jobsdomain.Retryable("AI_CONTEXT_UNAVAILABLE", err)
		}
		if _, err := service.Enqueue(ctx, command); err != nil {
			if errors.Is(err, ErrInvalid) || errors.Is(err, ErrConflict) {
				return jobsdomain.Permanent("AI_JOB_INVALID", err)
			}
			return jobsdomain.Retryable("AI_JOB_UNAVAILABLE", err)
		}
		return nil
	}
}
