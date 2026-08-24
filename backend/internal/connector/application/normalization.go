package application

import (
	"context"
	"errors"
	"time"

	"lidradar/backend/internal/connector/domain"
)

const normalizationInvalidCode = "NORMALIZATION_INVALID_PAYLOAD"

// CanonicalSink — граница передачи независимого от канала события модулю
// переписок. Реализация обязана быть идемпотентной при повторной доставке.
type CanonicalSink interface {
	IngestCanonical(context.Context, domain.CanonicalEvent) error
}

// NormalizationService преобразует сохранённые события и передаёт их ядру переписок.
type NormalizationService struct {
	repository domain.Repository
	registry   domain.ConnectorRegistry
	sink       CanonicalSink
	now        func() time.Time
}

// NewNormalizationService собирает фоновый сценарий из реестра и портов хранения.
func NewNormalizationService(
	repository domain.Repository,
	registry domain.ConnectorRegistry,
	sink CanonicalSink,
	now func() time.Time,
) NormalizationService {
	return NormalizationService{repository: repository, registry: registry, sink: sink, now: now}
}

// ProcessBatch обрабатывает ограниченную порцию ранее сохранённых RawEvent.
// Общая очередь с арендой появится на этапе 6; повтор здесь безопасен за счёт
// уникальных внешних идентификаторов и идемпотентного Conversation Core.
func (service NormalizationService) ProcessBatch(ctx context.Context, limit int) (int, error) {
	if service.repository == nil || service.registry == nil || service.sink == nil || service.now == nil || limit < 1 || limit > 100 {
		return 0, ErrInvalid
	}
	items, err := service.repository.PendingNormalization(ctx, limit)
	if err != nil {
		return 0, mapDomainError(err)
	}
	processed := 0
	for _, item := range items {
		registration, found := service.registry.Lookup(item.Connection.Provider)
		if !found || registration.Connector == nil {
			return processed, ErrUnavailable
		}
		events, normalizeErr := registration.Connector.NormalizeEvent(ctx, item.Connection, item.Event)
		if errors.Is(normalizeErr, domain.ErrInvalidPayload) || invalidCanonicalEvents(events) {
			if err := service.repository.FailNormalization(
				ctx, item.Event.TenantID, item.Event.ID, normalizationInvalidCode, service.now().UTC(),
			); err != nil {
				return processed, mapDomainError(err)
			}
			processed++
			continue
		}
		if normalizeErr != nil {
			return processed, normalizeErr
		}
		for _, event := range events {
			if err := service.sink.IngestCanonical(ctx, event); err != nil {
				return processed, err
			}
		}
		if err := service.repository.CompleteNormalization(
			ctx, item.Event.TenantID, item.Event.ID, service.now().UTC(),
		); err != nil {
			return processed, mapDomainError(err)
		}
		processed++
	}
	return processed, nil
}

func invalidCanonicalEvents(events []domain.CanonicalEvent) bool {
	for _, event := range events {
		if event.Validate() != nil {
			return true
		}
	}
	return false
}
