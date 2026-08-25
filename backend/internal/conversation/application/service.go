// Package application координирует канонизацию и чтение переписок.
package application

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	connectordomain "lidradar/backend/internal/connector/domain"
	"lidradar/backend/internal/conversation/domain"
)

var (
	ErrInvalid   = errors.New("некорректный запрос переписки")
	ErrForbidden = errors.New("нет разрешения на чтение переписки")
	ErrNotFound  = errors.New("переписка не найдена")
	ErrConflict  = errors.New("конфликт канонической переписки")
)

const (
	PermissionRead = "conversation.read"
	defaultLimit   = 50
	maximumLimit   = 100
)

// Authorizer проверяет именованное разрешение участника организации.
type Authorizer interface {
	Allowed(context.Context, string, string, string) (bool, error)
}

// IDs создаёт новые внутренние идентификаторы.
type IDs interface{ NewID() (string, error) }

// Service выполняет сценарии канонизации и безопасного чтения переписок.
type Service struct {
	repository domain.Repository
	authorizer Authorizer
	ids        IDs
}

// NewService собирает прикладной сервис из портов хранения, прав и идентификаторов.
func NewService(repository domain.Repository, authorizer Authorizer, ids IDs) Service {
	return Service{repository: repository, authorizer: authorizer, ids: ids}
}

// IngestCanonical применяет только независимое от канала сообщение. Повтор
// того же изменения является безопасной операцией без роста revision.
func (service Service) IngestCanonical(ctx context.Context, event connectordomain.CanonicalEvent) error {
	if service.repository == nil || service.ids == nil || event.Validate() != nil {
		return ErrInvalid
	}
	change, err := mapCanonical(event)
	if err != nil {
		return err
	}
	ids, err := service.candidateIDs(len(change.Attachments))
	if err != nil {
		return err
	}
	if _, err := service.repository.Ingest(ctx, change, ids); err != nil {
		return mapDomainError(err)
	}
	return nil
}

// ConversationPage — страница переписок с непрозрачным продолжением.
type ConversationPage struct {
	Items      []domain.Conversation `json:"items"`
	NextCursor *string               `json:"nextCursor"`
}

func (service Service) List(
	ctx context.Context,
	actorID, tenantID string,
	limit int,
	cursor string,
) (ConversationPage, error) {
	if err := service.requireRead(ctx, actorID, tenantID); err != nil {
		return ConversationPage{}, err
	}
	limit, pageCursor, err := pagination(limit, cursor)
	if err != nil {
		return ConversationPage{}, err
	}
	items, more, err := service.repository.List(ctx, tenantID, limit, pageCursor)
	if err != nil {
		return ConversationPage{}, mapDomainError(err)
	}
	if items == nil {
		items = []domain.Conversation{}
	}
	return ConversationPage{Items: items, NextCursor: conversationCursor(items, more)}, nil
}

func (service Service) Detail(
	ctx context.Context,
	actorID, tenantID, conversationID string,
) (domain.ConversationDetail, error) {
	if err := service.requireRead(ctx, actorID, tenantID); err != nil {
		return domain.ConversationDetail{}, err
	}
	if strings.TrimSpace(conversationID) == "" {
		return domain.ConversationDetail{}, ErrInvalid
	}
	detail, found, err := service.repository.Detail(ctx, tenantID, conversationID)
	if err != nil {
		return domain.ConversationDetail{}, mapDomainError(err)
	}
	if !found {
		return domain.ConversationDetail{}, ErrNotFound
	}
	return detail, nil
}

// MessagePage — страница сообщений с непрозрачным продолжением.
type MessagePage struct {
	Items      []domain.MessageView `json:"items"`
	NextCursor *string              `json:"nextCursor"`
}

func (service Service) Messages(
	ctx context.Context,
	actorID, tenantID, conversationID string,
	limit int,
	cursor string,
) (MessagePage, error) {
	if err := service.requireRead(ctx, actorID, tenantID); err != nil {
		return MessagePage{}, err
	}
	if strings.TrimSpace(conversationID) == "" {
		return MessagePage{}, ErrInvalid
	}
	limit, pageCursor, err := pagination(limit, cursor)
	if err != nil {
		return MessagePage{}, err
	}
	items, more, err := service.repository.Messages(ctx, tenantID, conversationID, limit, pageCursor)
	if err != nil {
		return MessagePage{}, mapDomainError(err)
	}
	if items == nil {
		items = []domain.MessageView{}
	}
	return MessagePage{Items: items, NextCursor: messageCursor(items, more)}, nil
}

// CommercialSnapshot отдаёт внутреннему прикладному процессу только актуальное
// состояние переписки. Проверка прав выполняется у владельца вызываемого сценария.
func (service Service) CommercialSnapshot(
	ctx context.Context,
	tenantID, conversationID string,
) (domain.CandidateSnapshot, bool, error) {
	if service.repository == nil || tenantID == "" || strings.TrimSpace(conversationID) == "" {
		return domain.CandidateSnapshot{}, false, ErrInvalid
	}
	snapshot, found, err := service.repository.CandidateSnapshot(ctx, tenantID, conversationID)
	if err != nil {
		return domain.CandidateSnapshot{}, false, mapDomainError(err)
	}
	return snapshot, found, nil
}

func (service Service) requireRead(ctx context.Context, actorID, tenantID string) error {
	if service.repository == nil || service.authorizer == nil || actorID == "" || tenantID == "" {
		return ErrForbidden
	}
	allowed, err := service.authorizer.Allowed(ctx, actorID, tenantID, PermissionRead)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrForbidden
	}
	return nil
}

func (service Service) candidateIDs(attachmentCount int) (domain.CandidateIDs, error) {
	values := make([]string, 0, 5+attachmentCount)
	for index := 0; index < 5+attachmentCount; index++ {
		id, err := service.ids.NewID()
		if err != nil {
			return domain.CandidateIDs{}, err
		}
		values = append(values, id)
	}
	return domain.CandidateIDs{
		ContactID: values[0], ExternalIdentityID: values[1], ConversationID: values[2], MessageID: values[3],
		OutboxEventID: values[4], AttachmentIDs: append([]string(nil), values[5:]...),
	}, nil
}

func mapCanonical(event connectordomain.CanonicalEvent) (domain.CanonicalChange, error) {
	attachments := make([]domain.AttachmentInput, 0, len(event.Attachments))
	for _, attachment := range event.Attachments {
		attachments = append(attachments, domain.AttachmentInput{
			ObjectKey: attachment.ObjectKey, MIMEType: cleanOptional(attachment.MIMEType), SizeBytes: attachment.SizeBytes,
			SHA256: cleanOptional(attachment.SHA256), ProviderFileID: cleanOptional(attachment.ProviderFileID),
		})
	}
	change := domain.CanonicalChange{
		SourceEventID: event.SourceEventID, Type: domain.ChangeType(event.Type), TenantID: event.TenantID,
		ConnectionID: event.ConnectionID, LocationID: cleanOptional(event.LocationID), Provider: string(event.Provider),
		ConversationExternalID: strings.TrimSpace(event.ConversationExternalID), MessageExternalID: strings.TrimSpace(event.MessageExternalID),
		ContactExternalID: strings.TrimSpace(event.ContactExternalID), ContactDisplayName: cleanDisplayName(event.ContactDisplayName),
		ContactPhoneNormalized: cleanOptional(event.ContactPhoneNormalized), ContactEmailNormalized: normalizeEmail(event.ContactEmailNormalized),
		Direction: domain.Direction(event.Direction), MessageType: domain.MessageType(event.MessageType), Text: event.Text,
		SenderExternalID: cleanOptional(event.SenderExternalID), ReplyToMessageExternalID: cleanOptional(event.ReplyToMessageExternalID),
		SentAt: event.SentAt.UTC(), OccurredAt: event.OccurredAt.UTC(), ReceivedAt: event.ReceivedAt.UTC(),
		Attachments: attachments, Metadata: append(json.RawMessage(nil), event.Metadata...),
	}
	if change.Validate() != nil {
		return domain.CanonicalChange{}, ErrInvalid
	}
	return change, nil
}

type encodedCursor struct {
	At string `json:"at"`
	ID string `json:"id"`
}

func pagination(limit int, cursor string) (int, *domain.PageCursor, error) {
	if limit == 0 {
		limit = defaultLimit
	}
	if limit < 1 || limit > maximumLimit {
		return 0, nil, ErrInvalid
	}
	if cursor == "" {
		return limit, nil, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, nil, ErrInvalid
	}
	var value encodedCursor
	if json.Unmarshal(decoded, &value) != nil || value.ID == "" {
		return 0, nil, ErrInvalid
	}
	at, err := time.Parse(time.RFC3339Nano, value.At)
	if err != nil {
		return 0, nil, ErrInvalid
	}
	return limit, &domain.PageCursor{At: at.UTC(), ID: value.ID}, nil
}

func conversationCursor(items []domain.Conversation, more bool) *string {
	if !more || len(items) == 0 {
		return nil
	}
	last := items[len(items)-1]
	return encodeCursor(last.UpdatedAt, last.ID)
}

func messageCursor(items []domain.MessageView, more bool) *string {
	if !more || len(items) == 0 {
		return nil
	}
	last := items[len(items)-1].Message
	return encodeCursor(last.SentAt, last.ID)
}

func encodeCursor(at time.Time, id string) *string {
	encoded, _ := json.Marshal(encodedCursor{At: at.UTC().Format(time.RFC3339Nano), ID: id})
	value := base64.RawURLEncoding.EncodeToString(encoded)
	return &value
}

func cleanOptional(value *string) *string {
	if value == nil {
		return nil
	}
	cleaned := strings.TrimSpace(*value)
	if cleaned == "" {
		return nil
	}
	return &cleaned
}

func cleanDisplayName(value *string) *string {
	if value == nil {
		return nil
	}
	cleaned := strings.Join(strings.Fields(*value), " ")
	if cleaned == "" {
		return nil
	}
	return &cleaned
}

func normalizeEmail(value *string) *string {
	cleaned := cleanOptional(value)
	if cleaned == nil {
		return nil
	}
	normalized := strings.ToLower(*cleaned)
	return &normalized
}

func mapDomainError(err error) error {
	switch {
	case errors.Is(err, domain.ErrInvalid):
		return ErrInvalid
	case errors.Is(err, domain.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, domain.ErrConflict):
		return ErrConflict
	default:
		return err
	}
}
