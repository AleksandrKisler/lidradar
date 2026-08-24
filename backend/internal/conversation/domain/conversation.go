// Package domain содержит независимую от каналов модель контактов и переписок.
package domain

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
)

var (
	ErrInvalid  = errors.New("некорректное состояние переписки")
	ErrNotFound = errors.New("переписка не найдена")
	ErrConflict = errors.New("конфликт канонической переписки")
)

// Direction показывает, с какой стороны делового диалога пришло сообщение.
type Direction string

const (
	DirectionIncoming Direction = "INCOMING"
	DirectionOutgoing Direction = "OUTGOING"
	DirectionSystem   Direction = "SYSTEM"
)

func (direction Direction) Valid() bool {
	switch direction {
	case DirectionIncoming, DirectionOutgoing, DirectionSystem:
		return true
	default:
		return false
	}
}

// MessageType описывает канонический вид содержимого сообщения.
type MessageType string

const (
	MessageText     MessageType = "TEXT"
	MessageImage    MessageType = "IMAGE"
	MessageVoice    MessageType = "VOICE"
	MessageAudio    MessageType = "AUDIO"
	MessageVideo    MessageType = "VIDEO"
	MessageDocument MessageType = "DOCUMENT"
	MessageOther    MessageType = "OTHER"
)

func (messageType MessageType) Valid() bool {
	switch messageType {
	case MessageText, MessageImage, MessageVoice, MessageAudio, MessageVideo, MessageDocument, MessageOther:
		return true
	default:
		return false
	}
}

// ChangeType определяет поддерживаемую операцию над сообщением.
type ChangeType string

const (
	ChangeReceived ChangeType = "message.received.v1"
	ChangeEdited   ChangeType = "message.edited.v1"
	ChangeDeleted  ChangeType = "message.deleted.v1"
)

func (changeType ChangeType) Valid() bool {
	switch changeType {
	case ChangeReceived, ChangeEdited, ChangeDeleted:
		return true
	default:
		return false
	}
}

// ConversationStatus показывает, участвует ли переписка в текущей работе.
type ConversationStatus string

const (
	ConversationActive   ConversationStatus = "ACTIVE"
	ConversationArchived ConversationStatus = "ARCHIVED"
)

func (status ConversationStatus) Valid() bool {
	return status == ConversationActive || status == ConversationArchived
}

// Contact хранит известные сведения о человеке независимо от канала связи.
type Contact struct {
	ID              string    `json:"id"`
	TenantID        string    `json:"-"`
	DisplayName     *string   `json:"displayName"`
	PhoneNormalized *string   `json:"phoneNormalized"`
	EmailNormalized *string   `json:"emailNormalized"`
	CreatedAt       time.Time `json:"createdAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

func (contact Contact) Validate() error {
	if contact.ID == "" || contact.TenantID == "" || contact.CreatedAt.IsZero() || contact.UpdatedAt.IsZero() {
		return ErrInvalid
	}
	if !validOptionalClean(contact.DisplayName) || !validOptionalClean(contact.PhoneNormalized) ||
		!validOptionalClean(contact.EmailNormalized) {
		return ErrInvalid
	}
	return nil
}

// ExternalIdentity связывает внешний идентификатор канала с контактом.
type ExternalIdentity struct {
	ID           string          `json:"id"`
	TenantID     string          `json:"-"`
	ContactID    string          `json:"contactId"`
	Provider     string          `json:"provider"`
	ConnectionID string          `json:"connectionId"`
	ExternalID   string          `json:"externalId"`
	Metadata     json.RawMessage `json:"metadata"`
	CreatedAt    time.Time       `json:"createdAt"`
}

func (identity ExternalIdentity) Validate() error {
	if identity.ID == "" || identity.TenantID == "" || identity.ContactID == "" || identity.Provider == "" ||
		identity.ConnectionID == "" || identity.ExternalID == "" || identity.CreatedAt.IsZero() ||
		!jsonObject(identity.Metadata) {
		return ErrInvalid
	}
	return nil
}

// Conversation — долгоживущая каноническая переписка с одним контактом.
type Conversation struct {
	ID                   string             `json:"id"`
	TenantID             string             `json:"-"`
	LocationID           *string            `json:"locationId"`
	ConnectionID         string             `json:"connectionId"`
	ContactID            string             `json:"contactId"`
	ExternalID           string             `json:"externalId"`
	Status               ConversationStatus `json:"status"`
	FirstMessageAt       *time.Time         `json:"firstMessageAt"`
	LastMessageAt        *time.Time         `json:"lastMessageAt"`
	LastMessageDirection *Direction         `json:"lastMessageDirection"`
	Revision             int64              `json:"revision"`
	CreatedAt            time.Time          `json:"createdAt"`
	UpdatedAt            time.Time          `json:"updatedAt"`
}

func (conversation Conversation) Validate() error {
	if conversation.ID == "" || conversation.TenantID == "" || conversation.ConnectionID == "" ||
		conversation.ContactID == "" || conversation.ExternalID == "" || !conversation.Status.Valid() ||
		conversation.Revision < 0 || conversation.CreatedAt.IsZero() || conversation.UpdatedAt.IsZero() {
		return ErrInvalid
	}
	if conversation.LastMessageDirection != nil && !conversation.LastMessageDirection.Valid() {
		return ErrInvalid
	}
	if (conversation.FirstMessageAt == nil) != (conversation.LastMessageAt == nil) ||
		(conversation.LastMessageAt == nil) != (conversation.LastMessageDirection == nil) {
		return ErrInvalid
	}
	return nil
}

// Message хранит каноническое состояние одного сообщения поставщика.
type Message struct {
	ID                string          `json:"id"`
	TenantID          string          `json:"-"`
	ConversationID    string          `json:"conversationId"`
	ConnectionID      string          `json:"connectionId"`
	ExternalID        string          `json:"externalId"`
	Direction         Direction       `json:"direction"`
	Type              MessageType     `json:"type"`
	Text              *string         `json:"text"`
	SenderExternalID  *string         `json:"senderExternalId"`
	ReplyToMessageID  *string         `json:"replyToMessageId"`
	SentAt            time.Time       `json:"sentAt"`
	ReceivedAt        time.Time       `json:"receivedAt"`
	ProviderDeletedAt *time.Time      `json:"providerDeletedAt"`
	Metadata          json.RawMessage `json:"metadata"`
	CreatedAt         time.Time       `json:"createdAt"`
}

func (message Message) Validate() error {
	if message.ID == "" || message.TenantID == "" || message.ConversationID == "" || message.ConnectionID == "" ||
		message.ExternalID == "" || !message.Direction.Valid() || !message.Type.Valid() || message.SentAt.IsZero() ||
		message.ReceivedAt.IsZero() || message.CreatedAt.IsZero() || !jsonObject(message.Metadata) {
		return ErrInvalid
	}
	return nil
}

// Attachment содержит только метаданные файла; двоичные данные хранятся отдельно.
type Attachment struct {
	ID             string    `json:"id"`
	TenantID       string    `json:"-"`
	MessageID      string    `json:"messageId"`
	ObjectKey      string    `json:"objectKey"`
	MIMEType       *string   `json:"mimeType"`
	SizeBytes      int64     `json:"sizeBytes"`
	SHA256         *string   `json:"sha256"`
	ProviderFileID *string   `json:"providerFileId"`
	CreatedAt      time.Time `json:"createdAt"`
}

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (attachment Attachment) Validate() error {
	if attachment.ID == "" || attachment.TenantID == "" || attachment.MessageID == "" ||
		attachment.ObjectKey == "" || attachment.ObjectKey != strings.TrimSpace(attachment.ObjectKey) ||
		attachment.SizeBytes < 0 || attachment.CreatedAt.IsZero() {
		return ErrInvalid
	}
	if attachment.SHA256 != nil && !sha256Pattern.MatchString(*attachment.SHA256) {
		return ErrInvalid
	}
	return nil
}

// AttachmentInput содержит проверенные метаданные вложения для записи.
type AttachmentInput struct {
	ObjectKey      string
	MIMEType       *string
	SizeBytes      int64
	SHA256         *string
	ProviderFileID *string
}

// CanonicalChange — независимая от поставщика команда изменения переписки.
type CanonicalChange struct {
	SourceEventID            string
	Type                     ChangeType
	TenantID                 string
	ConnectionID             string
	LocationID               *string
	Provider                 string
	ConversationExternalID   string
	MessageExternalID        string
	ContactExternalID        string
	ContactDisplayName       *string
	ContactPhoneNormalized   *string
	ContactEmailNormalized   *string
	Direction                Direction
	MessageType              MessageType
	Text                     *string
	SenderExternalID         *string
	ReplyToMessageExternalID *string
	SentAt                   time.Time
	OccurredAt               time.Time
	ReceivedAt               time.Time
	Attachments              []AttachmentInput
	Metadata                 json.RawMessage
}

func (change CanonicalChange) Validate() error {
	if change.SourceEventID == "" || !change.Type.Valid() || change.TenantID == "" || change.ConnectionID == "" ||
		change.Provider == "" || change.ConversationExternalID == "" || change.MessageExternalID == "" ||
		change.OccurredAt.IsZero() || change.ReceivedAt.IsZero() || !jsonObject(change.Metadata) {
		return ErrInvalid
	}
	if change.Type != ChangeDeleted && (!change.Direction.Valid() || !change.MessageType.Valid() || change.SentAt.IsZero()) {
		return ErrInvalid
	}
	if change.Type == ChangeReceived && change.ContactExternalID == "" {
		return ErrInvalid
	}
	return nil
}

// CandidateIDs содержит заранее созданные идентификаторы для одной транзакции.
type CandidateIDs struct {
	ContactID          string
	ExternalIdentityID string
	ConversationID     string
	MessageID          string
	AttachmentIDs      []string
}

func (ids CandidateIDs) Validate(change CanonicalChange) error {
	if ids.ContactID == "" || ids.ExternalIdentityID == "" || ids.ConversationID == "" || ids.MessageID == "" ||
		len(ids.AttachmentIDs) != len(change.Attachments) {
		return ErrInvalid
	}
	for _, id := range ids.AttachmentIDs {
		if id == "" {
			return ErrInvalid
		}
	}
	return nil
}

// IngestResult сообщает итог идемпотентного применения канонического изменения.
type IngestResult struct {
	ContactID      string
	ConversationID string
	MessageID      string
	Changed        bool
	Revision       int64
}

// ConversationDetail объединяет переписку и её контакт для чтения.
type ConversationDetail struct {
	Conversation Conversation `json:"conversation"`
	Contact      Contact      `json:"contact"`
}

// MessageView объединяет сообщение и метаданные его вложений.
type MessageView struct {
	Message     Message      `json:"message"`
	Attachments []Attachment `json:"attachments"`
}

// PageCursor задаёт устойчивую границу следующей страницы.
type PageCursor struct {
	At time.Time
	ID string
}

// Repository описывает только операции, принадлежащие модулю переписок.
type Repository interface {
	Ingest(context.Context, CanonicalChange, CandidateIDs) (IngestResult, error)
	List(context.Context, string, int, *PageCursor) ([]Conversation, bool, error)
	Detail(context.Context, string, string) (ConversationDetail, bool, error)
	Messages(context.Context, string, string, int, *PageCursor) ([]MessageView, bool, error)
}

func validOptionalClean(value *string) bool {
	return value == nil || (*value != "" && *value == strings.TrimSpace(*value))
}

func jsonObject(value json.RawMessage) bool {
	var object map[string]json.RawMessage
	return json.Unmarshal(value, &object) == nil && object != nil
}
