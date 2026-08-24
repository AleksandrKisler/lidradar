// Package domain содержит независимые от каналов подключения, исходные события
// и контракты адаптеров.
package domain

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"
)

var (
	ErrInvalid         = errors.New("invalid connector state")
	ErrNotFound        = errors.New("connector resource not found")
	ErrConflict        = errors.New("connector resource conflict")
	ErrUnauthenticated = errors.New("connector event authentication failed")
	ErrInvalidPayload  = errors.New("invalid connector payload")
	ErrDisconnected    = errors.New("channel connection is disconnected")
	ErrUnavailable     = errors.New("connector is unavailable")
)

type Provider string

const (
	ProviderTest                         Provider = "TEST"
	ProviderImport                       Provider = "IMPORT"
	ProviderGenericWebhook               Provider = "GENERIC_WEBHOOK"
	ProviderTelegramConnectedBusinessBot Provider = "CONNECTED_BUSINESS_BOT"
)

func ParseProvider(raw string) (Provider, error) {
	provider := Provider(strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(raw), "-", "_")))
	if !provider.Valid() {
		return "", ErrInvalid
	}
	return provider, nil
}

func (provider Provider) Valid() bool {
	switch provider {
	case ProviderTest, ProviderImport, ProviderGenericWebhook, ProviderTelegramConnectedBusinessBot:
		return true
	default:
		return false
	}
}

type ConnectionStatus string

const (
	ConnectionActive       ConnectionStatus = "ACTIVE"
	ConnectionDegraded     ConnectionStatus = "DEGRADED"
	ConnectionError        ConnectionStatus = "ERROR"
	ConnectionDisconnected ConnectionStatus = "DISCONNECTED"
)

func (status ConnectionStatus) Valid() bool {
	switch status {
	case ConnectionActive, ConnectionDegraded, ConnectionError, ConnectionDisconnected:
		return true
	default:
		return false
	}
}

type Capability string

const (
	CapabilityReceiveMessages    Capability = "CAN_RECEIVE_MESSAGES"
	CapabilitySendMessages       Capability = "CAN_SEND_MESSAGES"
	CapabilityImportHistory      Capability = "CAN_IMPORT_HISTORY"
	CapabilityReceiveEdits       Capability = "CAN_RECEIVE_EDITS"
	CapabilityReceiveDeletions   Capability = "CAN_RECEIVE_DELETES"
	CapabilityReceiveAttachments Capability = "CAN_RECEIVE_ATTACHMENTS"
	CapabilityIdentifyContact    Capability = "CAN_IDENTIFY_CONTACT"
)

func (capability Capability) Valid() bool {
	switch capability {
	case CapabilityReceiveMessages, CapabilitySendMessages, CapabilityImportHistory,
		CapabilityReceiveEdits, CapabilityReceiveDeletions, CapabilityReceiveAttachments,
		CapabilityIdentifyContact:
		return true
	default:
		return false
	}
}

type ConnectionHealth struct {
	Status        ConnectionStatus `json:"status"`
	LastEventAt   *time.Time       `json:"lastEventAt"`
	LastSuccessAt *time.Time       `json:"lastSuccessAt"`
	LastErrorAt   *time.Time       `json:"lastErrorAt"`
	LastErrorCode *string          `json:"lastErrorCode"`
	CheckedAt     time.Time        `json:"checkedAt"`
}

func (health ConnectionHealth) Validate() error {
	if !health.Status.Valid() || health.CheckedAt.IsZero() {
		return ErrInvalid
	}
	if health.Status == ConnectionActive && health.LastErrorCode != nil {
		return ErrInvalid
	}
	if health.LastErrorCode != nil {
		code := strings.TrimSpace(*health.LastErrorCode)
		if code == "" || len(code) > 100 || code != *health.LastErrorCode {
			return ErrInvalid
		}
	}
	return nil
}

type ChannelConnection struct {
	ID                     string           `json:"id"`
	TenantID               string           `json:"-"`
	LocationID             *string          `json:"locationId"`
	Provider               Provider         `json:"provider"`
	Name                   string           `json:"name"`
	Status                 ConnectionStatus `json:"status"`
	Capabilities           []Capability     `json:"capabilities"`
	VerificationSecretHash string           `json:"-"`
	LastEventAt            *time.Time       `json:"lastEventAt"`
	LastSuccessAt          *time.Time       `json:"lastSuccessAt"`
	LastErrorAt            *time.Time       `json:"lastErrorAt"`
	LastErrorCode          *string          `json:"lastErrorCode"`
	CreatedAt              time.Time        `json:"createdAt"`
	UpdatedAt              time.Time        `json:"updatedAt"`
}

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func NewChannelConnection(
	id, tenantID string,
	locationID *string,
	provider Provider,
	name string,
	capabilities []Capability,
	verificationSecretHash string,
	health ConnectionHealth,
	at time.Time,
) (ChannelConnection, error) {
	connection := ChannelConnection{
		ID: id, TenantID: tenantID, LocationID: cleanOptionalID(locationID), Provider: provider,
		Name: strings.Join(strings.Fields(name), " "), Status: health.Status,
		Capabilities: cleanCapabilities(capabilities), VerificationSecretHash: verificationSecretHash,
		LastEventAt: utcTime(health.LastEventAt), LastSuccessAt: utcTime(health.LastSuccessAt),
		LastErrorAt: utcTime(health.LastErrorAt), LastErrorCode: cleanOptionalCode(health.LastErrorCode),
		CreatedAt: at.UTC(), UpdatedAt: at.UTC(),
	}
	if connection.LastErrorCode != nil && connection.LastErrorAt == nil {
		errorAt := at.UTC()
		connection.LastErrorAt = &errorAt
	}
	if at.IsZero() || connection.Validate() != nil {
		return ChannelConnection{}, ErrInvalid
	}
	return connection, nil
}

func (connection ChannelConnection) Validate() error {
	if connection.ID == "" || connection.TenantID == "" || !connection.Provider.Valid() ||
		connection.Name == "" || len(connection.Name) > 200 || connection.Name != strings.Join(strings.Fields(connection.Name), " ") ||
		!connection.Status.Valid() || !sha256Pattern.MatchString(connection.VerificationSecretHash) ||
		connection.CreatedAt.IsZero() || connection.UpdatedAt.IsZero() || validateCapabilities(connection.Capabilities) != nil {
		return ErrInvalid
	}
	if connection.LocationID != nil {
		locationID := strings.TrimSpace(*connection.LocationID)
		if locationID == "" || locationID != *connection.LocationID {
			return ErrInvalid
		}
	}
	health := connection.Health(connection.UpdatedAt)
	return health.Validate()
}

func (connection ChannelConnection) Health(checkedAt time.Time) ConnectionHealth {
	return ConnectionHealth{
		Status: connection.Status, LastEventAt: utcTime(connection.LastEventAt),
		LastSuccessAt: utcTime(connection.LastSuccessAt), LastErrorAt: utcTime(connection.LastErrorAt),
		LastErrorCode: cleanOptionalCode(connection.LastErrorCode), CheckedAt: checkedAt.UTC(),
	}
}

func (connection *ChannelConnection) RecordSuccess(at time.Time, providerHealth ConnectionHealth) error {
	if connection == nil || at.IsZero() || providerHealth.Validate() != nil {
		return ErrInvalid
	}
	at = at.UTC()
	connection.LastEventAt = &at
	connection.LastSuccessAt = &at
	connection.Status = providerHealth.Status
	if providerHealth.Status == ConnectionActive {
		connection.LastErrorAt = nil
		connection.LastErrorCode = nil
	} else {
		connection.LastErrorAt = utcTime(providerHealth.LastErrorAt)
		connection.LastErrorCode = cleanOptionalCode(providerHealth.LastErrorCode)
		if connection.LastErrorCode != nil && connection.LastErrorAt == nil {
			connection.LastErrorAt = &at
		}
	}
	connection.UpdatedAt = at
	return connection.Validate()
}

func (connection *ChannelConnection) RecordFailure(code string, at time.Time) error {
	code = strings.TrimSpace(code)
	if connection == nil || code == "" || len(code) > 100 || at.IsZero() {
		return ErrInvalid
	}
	at = at.UTC()
	connection.Status = ConnectionError
	connection.LastEventAt = &at
	connection.LastErrorAt = &at
	connection.LastErrorCode = &code
	connection.UpdatedAt = at
	return connection.Validate()
}

func (connection *ChannelConnection) Disconnect(at time.Time) error {
	if connection == nil || at.IsZero() {
		return ErrInvalid
	}
	connection.Status = ConnectionDisconnected
	connection.UpdatedAt = at.UTC()
	return connection.Validate()
}

type RawEventStatus string

const (
	RawEventReceived   RawEventStatus = "RECEIVED"
	RawEventProcessing RawEventStatus = "PROCESSING"
	RawEventProcessed  RawEventStatus = "PROCESSED"
	RawEventFailed     RawEventStatus = "FAILED"
)

func (status RawEventStatus) Valid() bool {
	switch status {
	case RawEventReceived, RawEventProcessing, RawEventProcessed, RawEventFailed:
		return true
	default:
		return false
	}
}

type RawEvent struct {
	ID              string          `json:"id"`
	TenantID        string          `json:"-"`
	ConnectionID    string          `json:"connectionId"`
	Provider        Provider        `json:"provider"`
	ExternalEventID string          `json:"externalEventId"`
	Payload         json.RawMessage `json:"-"`
	PayloadHash     string          `json:"-"`
	Status          RawEventStatus  `json:"status"`
	ErrorCode       *string         `json:"errorCode"`
	ReceivedAt      time.Time       `json:"receivedAt"`
	ProcessedAt     *time.Time      `json:"processedAt"`
	CreatedAt       time.Time       `json:"createdAt"`
}

func NewRawEvent(
	id, tenantID, connectionID string,
	provider Provider,
	externalEventID string,
	payload json.RawMessage,
	payloadHash string,
	status RawEventStatus,
	errorCode *string,
	at time.Time,
) (RawEvent, error) {
	event := RawEvent{
		ID: id, TenantID: tenantID, ConnectionID: connectionID, Provider: provider,
		ExternalEventID: strings.TrimSpace(externalEventID), Payload: append(json.RawMessage(nil), payload...),
		PayloadHash: payloadHash, Status: status, ErrorCode: cleanOptionalCode(errorCode),
		ReceivedAt: at.UTC(), CreatedAt: at.UTC(),
	}
	if status == RawEventFailed {
		processedAt := at.UTC()
		event.ProcessedAt = &processedAt
	}
	if at.IsZero() || event.Validate() != nil {
		return RawEvent{}, ErrInvalid
	}
	return event, nil
}

func (event RawEvent) Validate() error {
	if event.ID == "" || event.TenantID == "" || event.ConnectionID == "" || !event.Provider.Valid() ||
		event.ExternalEventID == "" || len(event.ExternalEventID) > 512 || !json.Valid(event.Payload) ||
		!sha256Pattern.MatchString(event.PayloadHash) || !event.Status.Valid() ||
		event.ReceivedAt.IsZero() || event.CreatedAt.IsZero() {
		return ErrInvalid
	}
	if (event.Status == RawEventFailed && event.ErrorCode == nil) ||
		(event.Status != RawEventFailed && event.ErrorCode != nil) {
		return ErrInvalid
	}
	if event.Status == RawEventFailed && event.ProcessedAt == nil {
		return ErrInvalid
	}
	return nil
}

type NormalizationWorkStatus string

const NormalizationWorkPending NormalizationWorkStatus = "PENDING"

type NormalizationWork struct {
	ID           string
	TenantID     string
	ConnectionID string
	RawEventID   string
	Status       NormalizationWorkStatus
	CreatedAt    time.Time
}

func NewNormalizationWork(id string, event RawEvent, at time.Time) (NormalizationWork, error) {
	work := NormalizationWork{
		ID: id, TenantID: event.TenantID, ConnectionID: event.ConnectionID,
		RawEventID: event.ID, Status: NormalizationWorkPending, CreatedAt: at.UTC(),
	}
	if event.Status != RawEventReceived || work.Validate(event) != nil {
		return NormalizationWork{}, ErrInvalid
	}
	return work, nil
}

func (work NormalizationWork) Validate(event RawEvent) error {
	if work.ID == "" || work.TenantID == "" || work.ConnectionID == "" || work.RawEventID == "" ||
		work.Status != NormalizationWorkPending || work.CreatedAt.IsZero() ||
		work.TenantID != event.TenantID || work.ConnectionID != event.ConnectionID || work.RawEventID != event.ID {
		return ErrInvalid
	}
	return nil
}

// CanonicalEventType определяет версионированное изменение сообщения.
type CanonicalEventType string

const (
	CanonicalMessageReceived CanonicalEventType = "message.received.v1"
	CanonicalMessageEdited   CanonicalEventType = "message.edited.v1"
	CanonicalMessageDeleted  CanonicalEventType = "message.deleted.v1"
)

func (eventType CanonicalEventType) Valid() bool {
	switch eventType {
	case CanonicalMessageReceived, CanonicalMessageEdited, CanonicalMessageDeleted:
		return true
	default:
		return false
	}
}

// CanonicalDirection показывает сторону делового диалога.
type CanonicalDirection string

const (
	CanonicalIncoming CanonicalDirection = "INCOMING"
	CanonicalOutgoing CanonicalDirection = "OUTGOING"
	CanonicalSystem   CanonicalDirection = "SYSTEM"
)

func (direction CanonicalDirection) Valid() bool {
	switch direction {
	case CanonicalIncoming, CanonicalOutgoing, CanonicalSystem:
		return true
	default:
		return false
	}
}

// CanonicalMessageType описывает вид содержимого независимо от поставщика.
type CanonicalMessageType string

const (
	CanonicalText     CanonicalMessageType = "TEXT"
	CanonicalImage    CanonicalMessageType = "IMAGE"
	CanonicalVoice    CanonicalMessageType = "VOICE"
	CanonicalAudio    CanonicalMessageType = "AUDIO"
	CanonicalVideo    CanonicalMessageType = "VIDEO"
	CanonicalDocument CanonicalMessageType = "DOCUMENT"
	CanonicalOther    CanonicalMessageType = "OTHER"
)

func (messageType CanonicalMessageType) Valid() bool {
	switch messageType {
	case CanonicalText, CanonicalImage, CanonicalVoice, CanonicalAudio,
		CanonicalVideo, CanonicalDocument, CanonicalOther:
		return true
	default:
		return false
	}
}

// CanonicalAttachment переносит только метаданные файла между модулями.
type CanonicalAttachment struct {
	ObjectKey      string  `json:"objectKey"`
	MIMEType       *string `json:"mimeType"`
	SizeBytes      int64   `json:"sizeBytes"`
	SHA256         *string `json:"sha256"`
	ProviderFileID *string `json:"providerFileId"`
}

func (attachment CanonicalAttachment) Validate() error {
	if strings.TrimSpace(attachment.ObjectKey) == "" || attachment.ObjectKey != strings.TrimSpace(attachment.ObjectKey) ||
		attachment.SizeBytes < 0 {
		return ErrInvalidPayload
	}
	if attachment.SHA256 != nil && !sha256Pattern.MatchString(*attachment.SHA256) {
		return ErrInvalidPayload
	}
	return nil
}

// CanonicalEvent — независимый от канала контракт между адаптером источника и
// модулем переписок. Поля провайдера не должны проходить дальше этого рубежа.
type CanonicalEvent struct {
	SourceEventID            string                `json:"sourceEventId"`
	Type                     CanonicalEventType    `json:"type"`
	TenantID                 string                `json:"-"`
	ConnectionID             string                `json:"connectionId"`
	LocationID               *string               `json:"locationId"`
	Provider                 Provider              `json:"provider"`
	ConversationExternalID   string                `json:"conversationExternalId"`
	MessageExternalID        string                `json:"messageExternalId"`
	ContactExternalID        string                `json:"contactExternalId"`
	ContactDisplayName       *string               `json:"contactDisplayName"`
	ContactPhoneNormalized   *string               `json:"contactPhoneNormalized"`
	ContactEmailNormalized   *string               `json:"contactEmailNormalized"`
	Direction                CanonicalDirection    `json:"direction"`
	MessageType              CanonicalMessageType  `json:"messageType"`
	Text                     *string               `json:"text"`
	SenderExternalID         *string               `json:"senderExternalId"`
	ReplyToMessageExternalID *string               `json:"replyToMessageExternalId"`
	SentAt                   time.Time             `json:"sentAt"`
	OccurredAt               time.Time             `json:"occurredAt"`
	ReceivedAt               time.Time             `json:"receivedAt"`
	Attachments              []CanonicalAttachment `json:"attachments"`
	Metadata                 json.RawMessage       `json:"metadata"`
}

func (event CanonicalEvent) Validate() error {
	if event.SourceEventID == "" || !event.Type.Valid() || event.TenantID == "" || event.ConnectionID == "" ||
		!event.Provider.Valid() || event.ConversationExternalID == "" || len(event.ConversationExternalID) > 512 ||
		event.MessageExternalID == "" || len(event.MessageExternalID) > 512 || event.OccurredAt.IsZero() ||
		event.ReceivedAt.IsZero() || !json.Valid(event.Metadata) || !jsonObject(event.Metadata) {
		return ErrInvalidPayload
	}
	if event.Type != CanonicalMessageDeleted {
		if !event.Direction.Valid() || !event.MessageType.Valid() || event.SentAt.IsZero() {
			return ErrInvalidPayload
		}
	}
	if event.Type == CanonicalMessageReceived && event.ContactExternalID == "" {
		return ErrInvalidPayload
	}
	for _, attachment := range event.Attachments {
		if attachment.Validate() != nil {
			return ErrInvalidPayload
		}
	}
	return nil
}

// Headers сохраняет доменный контракт независимым от net/http.
type Headers interface{ Get(string) string }

type Connector interface {
	Provider() Provider
	VerifyEvent(context.Context, ChannelConnection, []byte, Headers) error
	NormalizeEvent(context.Context, ChannelConnection, RawEvent) ([]CanonicalEvent, error)
	Health(context.Context, ChannelConnection) ConnectionHealth
}

type EventIdentifier interface {
	ExternalEventID([]byte, Headers) (string, error)
}

// ConnectorRegistration объединяет адаптер и подтверждённые им возможности.
type ConnectorRegistration struct {
	Connector    Connector
	Capabilities []Capability
}

// ConnectorRegistry находит адаптер по поставщику канала.
type ConnectorRegistry interface {
	Lookup(Provider) (ConnectorRegistration, bool)
}

type PersistResult struct {
	Event    RawEvent
	Inserted bool
}

// NormalizationItem объединяет исходное событие и его подключение для обработки.
type NormalizationItem struct {
	Work       NormalizationWork
	Connection ChannelConnection
	Event      RawEvent
}

// Repository требует tenant scope для каждого доступа к подключению и событию.
type Repository interface {
	ListConnections(context.Context, string) ([]ChannelConnection, error)
	Connection(context.Context, string, string) (ChannelConnection, bool, error)
	CreateConnection(context.Context, string, ChannelConnection) error
	DisconnectConnection(context.Context, string, string, time.Time) (ChannelConnection, bool, error)
	PersistEvent(context.Context, string, string, RawEvent, *NormalizationWork, ConnectionHealth) (PersistResult, error)
	PendingNormalization(context.Context, int) ([]NormalizationItem, error)
	CompleteNormalization(context.Context, string, string, time.Time) error
	FailNormalization(context.Context, string, string, string, time.Time) error
}

func cleanCapabilities(capabilities []Capability) []Capability {
	seen := make(map[Capability]struct{}, len(capabilities))
	result := make([]Capability, 0, len(capabilities))
	for _, capability := range capabilities {
		if _, duplicate := seen[capability]; !duplicate {
			seen[capability] = struct{}{}
			result = append(result, capability)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

func validateCapabilities(capabilities []Capability) error {
	if len(capabilities) == 0 {
		return ErrInvalid
	}
	seen := make(map[Capability]struct{}, len(capabilities))
	for _, capability := range capabilities {
		if !capability.Valid() {
			return ErrInvalid
		}
		if _, duplicate := seen[capability]; duplicate {
			return ErrInvalid
		}
		seen[capability] = struct{}{}
	}
	return nil
}

func cleanOptionalID(value *string) *string {
	if value == nil {
		return nil
	}
	cleaned := strings.TrimSpace(*value)
	return &cleaned
}

func cleanOptionalCode(value *string) *string {
	if value == nil {
		return nil
	}
	cleaned := strings.TrimSpace(*value)
	return &cleaned
}

func utcTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	result := value.UTC()
	return &result
}

func jsonObject(value json.RawMessage) bool {
	var object map[string]json.RawMessage
	return json.Unmarshal(value, &object) == nil && object != nil
}
