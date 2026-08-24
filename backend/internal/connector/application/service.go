// Package application управляет подключениями и приёмом событий с сохранением до обработки.
package application

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"lidradar/backend/internal/connector/domain"
)

var (
	ErrInvalid         = errors.New("invalid connector request")
	ErrForbidden       = errors.New("connector permission denied")
	ErrNotFound        = errors.New("connector resource not found")
	ErrConflict        = errors.New("connector resource conflict")
	ErrUnauthenticated = errors.New("webhook authentication failed")
	ErrUnavailable     = errors.New("connector unavailable")
)

const (
	PermissionManage        = "integration.manage"
	invalidPayloadErrorCode = "INVALID_PAYLOAD"
	minWebhookSecretBytes   = 16
	maxWebhookSecretBytes   = 256
)

var telegramSecretPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

type Authorizer interface {
	Allowed(context.Context, string, string, string) (bool, error)
}

type IDs interface{ NewID() (string, error) }

type Service struct {
	repository domain.Repository
	authorizer Authorizer
	registry   domain.ConnectorRegistry
	ids        IDs
	now        func() time.Time
}

func NewService(
	repository domain.Repository,
	authorizer Authorizer,
	registry domain.ConnectorRegistry,
	ids IDs,
	now func() time.Time,
) Service {
	return Service{repository: repository, authorizer: authorizer, registry: registry, ids: ids, now: now}
}

type ConnectCommand struct {
	Provider      string
	Name          string
	LocationID    *string
	WebhookSecret string
}

func (service Service) Connect(ctx context.Context, actorID, tenantID string, command ConnectCommand) (domain.ChannelConnection, error) {
	if err := service.requireManage(ctx, actorID, tenantID); err != nil {
		return domain.ChannelConnection{}, err
	}
	if !service.ready() || len(command.WebhookSecret) < minWebhookSecretBytes || len(command.WebhookSecret) > maxWebhookSecretBytes {
		return domain.ChannelConnection{}, ErrInvalid
	}
	provider, err := domain.ParseProvider(command.Provider)
	if err != nil {
		return domain.ChannelConnection{}, ErrInvalid
	}
	if provider == domain.ProviderTelegramConnectedBusinessBot && !telegramSecretPattern.MatchString(command.WebhookSecret) {
		return domain.ChannelConnection{}, ErrInvalid
	}
	registration, found := service.registry.Lookup(provider)
	if !found || registration.Connector == nil {
		return domain.ChannelConnection{}, ErrInvalid
	}
	id, err := service.ids.NewID()
	if err != nil {
		return domain.ChannelConnection{}, err
	}
	now := service.now().UTC()
	health := registration.Connector.Health(ctx, domain.ChannelConnection{Provider: provider})
	health.CheckedAt = now
	connection, err := domain.NewChannelConnection(
		id, tenantID, command.LocationID, provider, command.Name, registration.Capabilities,
		hashValue(command.WebhookSecret), health, now,
	)
	if err != nil {
		return domain.ChannelConnection{}, ErrInvalid
	}
	if err := service.repository.CreateConnection(ctx, tenantID, connection); err != nil {
		return domain.ChannelConnection{}, mapDomainError(err)
	}
	return connection, nil
}

func (service Service) List(ctx context.Context, actorID, tenantID string) ([]domain.ChannelConnection, error) {
	if err := service.requireManage(ctx, actorID, tenantID); err != nil {
		return nil, err
	}
	connections, err := service.repository.ListConnections(ctx, tenantID)
	if err != nil {
		return nil, mapDomainError(err)
	}
	return connections, nil
}

func (service Service) Health(ctx context.Context, actorID, tenantID, connectionID string) (domain.ConnectionHealth, error) {
	if err := service.requireManage(ctx, actorID, tenantID); err != nil {
		return domain.ConnectionHealth{}, err
	}
	if strings.TrimSpace(connectionID) == "" || service.now == nil {
		return domain.ConnectionHealth{}, ErrInvalid
	}
	connection, found, err := service.repository.Connection(ctx, tenantID, connectionID)
	if err != nil {
		return domain.ConnectionHealth{}, mapDomainError(err)
	}
	if !found {
		return domain.ConnectionHealth{}, ErrNotFound
	}
	return connection.Health(service.now().UTC()), nil
}

func (service Service) Disconnect(ctx context.Context, actorID, tenantID, connectionID string) error {
	if err := service.requireManage(ctx, actorID, tenantID); err != nil {
		return err
	}
	if strings.TrimSpace(connectionID) == "" || service.now == nil {
		return ErrInvalid
	}
	_, found, err := service.repository.DisconnectConnection(ctx, tenantID, connectionID, service.now().UTC())
	if err != nil {
		return mapDomainError(err)
	}
	if !found {
		return ErrNotFound
	}
	return nil
}

type Receipt struct {
	RawEventID string                `json:"rawEventId"`
	Status     domain.RawEventStatus `json:"status"`
	Duplicate  bool                  `json:"duplicate"`
}

// Receive проверяет событие и сохраняет RawEvent вместе с заданием преобразования
// в одной короткой транзакции. Само преобразование здесь не запускается.
func (service Service) Receive(
	ctx context.Context,
	providerValue, tenantID, connectionID string,
	payload []byte,
	headers domain.Headers,
) (Receipt, error) {
	if !service.ready() || tenantID == "" || connectionID == "" || len(payload) == 0 || headers == nil {
		return Receipt{}, ErrInvalid
	}
	provider, err := domain.ParseProvider(providerValue)
	if err != nil {
		return Receipt{}, ErrNotFound
	}
	connection, found, err := service.repository.Connection(ctx, tenantID, connectionID)
	if err != nil {
		return Receipt{}, mapDomainError(err)
	}
	if !found || connection.Provider != provider {
		return Receipt{}, ErrNotFound
	}
	if connection.Status == domain.ConnectionDisconnected {
		return Receipt{}, ErrUnavailable
	}
	registration, found := service.registry.Lookup(provider)
	if !found || registration.Connector == nil {
		return Receipt{}, ErrUnavailable
	}
	identifier, ok := registration.Connector.(domain.EventIdentifier)
	if !ok {
		return Receipt{}, ErrUnavailable
	}

	verificationErr := registration.Connector.VerifyEvent(ctx, connection, payload, headers)
	if errors.Is(verificationErr, domain.ErrUnauthenticated) {
		return Receipt{}, ErrUnauthenticated
	}
	if verificationErr != nil && !errors.Is(verificationErr, domain.ErrInvalidPayload) {
		return Receipt{}, ErrUnavailable
	}

	externalEventID, identifierErr := identifier.ExternalEventID(payload, headers)
	invalidPayload := errors.Is(verificationErr, domain.ErrInvalidPayload) || identifierErr != nil
	payloadHash := hashBytes(payload)
	if invalidPayload && strings.TrimSpace(externalEventID) == "" {
		externalEventID = "invalid:" + payloadHash
	}
	persistedPayload, err := persistencePayload(payload)
	if err != nil {
		return Receipt{}, ErrInvalid
	}

	now := service.now().UTC()
	rawEventID, err := service.ids.NewID()
	if err != nil {
		return Receipt{}, err
	}
	status := domain.RawEventReceived
	var errorCode *string
	if invalidPayload {
		status = domain.RawEventFailed
		code := invalidPayloadErrorCode
		errorCode = &code
	}
	rawEvent, err := domain.NewRawEvent(
		rawEventID, tenantID, connectionID, provider, externalEventID,
		persistedPayload, payloadHash, status, errorCode, now,
	)
	if err != nil {
		return Receipt{}, ErrInvalid
	}

	var work *domain.NormalizationWork
	if !invalidPayload {
		workID, idErr := service.ids.NewID()
		if idErr != nil {
			return Receipt{}, idErr
		}
		item, workErr := domain.NewNormalizationWork(workID, rawEvent, now)
		if workErr != nil {
			return Receipt{}, ErrInvalid
		}
		work = &item
	}
	providerHealth := registration.Connector.Health(ctx, connection)
	providerHealth.CheckedAt = now
	result, err := service.repository.PersistEvent(ctx, tenantID, connectionID, rawEvent, work, providerHealth)
	if err != nil {
		return Receipt{}, mapDomainError(err)
	}
	return Receipt{RawEventID: result.Event.ID, Status: result.Event.Status, Duplicate: !result.Inserted}, nil
}

func (service Service) ready() bool {
	return service.repository != nil && service.registry != nil && service.ids != nil && service.now != nil
}

func (service Service) requireManage(ctx context.Context, actorID, tenantID string) error {
	if service.repository == nil || service.authorizer == nil || actorID == "" || tenantID == "" {
		return ErrForbidden
	}
	allowed, err := service.authorizer.Allowed(ctx, actorID, tenantID, PermissionManage)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrForbidden
	}
	return nil
}

func hashValue(value string) string { return hashBytes([]byte(value)) }

func hashBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func persistencePayload(payload []byte) (json.RawMessage, error) {
	if json.Valid(payload) {
		return append(json.RawMessage(nil), payload...), nil
	}
	wrapped, err := json.Marshal(map[string]string{
		"encoding": "base64",
		"raw":      base64.StdEncoding.EncodeToString(payload),
	})
	return wrapped, err
}

func mapDomainError(err error) error {
	switch {
	case errors.Is(err, domain.ErrInvalid):
		return ErrInvalid
	case errors.Is(err, domain.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, domain.ErrConflict):
		return ErrConflict
	case errors.Is(err, domain.ErrUnauthenticated):
		return ErrUnauthenticated
	case errors.Is(err, domain.ErrDisconnected), errors.Is(err, domain.ErrUnavailable):
		return ErrUnavailable
	default:
		return err
	}
}
