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

// Process обрабатывает одно задание общей очереди. Повтор после потери аренды
// безопасен благодаря уникальным внешним идентификаторам Conversation Core и
// идемпотентному завершению RawEvent.
func (service NormalizationService) Process(ctx context.Context, tenantID, rawEventID string) error {
	if service.repository == nil || service.registry == nil || service.sink == nil || service.now == nil || tenantID == "" || rawEventID == "" {
		return ErrInvalid
	}
	item, found, err := service.repository.Normalization(ctx, tenantID, rawEventID)
	if err != nil {
		return mapDomainError(err)
	}
	if !found {
		return ErrNotFound
	}
	if item.Event.Status == domain.RawEventProcessed || item.Event.Status == domain.RawEventFailed {
		return nil
	}
	registration, found := service.registry.Lookup(item.Connection.Provider)
	if !found || registration.Connector == nil {
		return ErrUnavailable
	}
	events, normalizeErr := registration.Connector.NormalizeEvent(ctx, item.Connection, item.Event)
	if errors.Is(normalizeErr, domain.ErrInvalidPayload) || invalidCanonicalEvents(events) {
		return mapDomainError(service.repository.FailNormalization(
			ctx, item.Event.TenantID, item.Event.ID, normalizationInvalidCode, service.now().UTC(),
		))
	}
	if normalizeErr != nil {
		return normalizeErr
	}
	for _, event := range events {
		if err := service.sink.IngestCanonical(ctx, event); err != nil {
			return err
		}
	}
	return mapDomainError(service.repository.CompleteNormalization(
		ctx, item.Event.TenantID, item.Event.ID, service.now().UTC(),
	))
}

func invalidCanonicalEvents(events []domain.CanonicalEvent) bool {
	for _, event := range events {
		if event.Validate() != nil {
			return true
		}
	}
	return false
}
