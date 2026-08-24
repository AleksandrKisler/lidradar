// Package infrastructure содержит PostgreSQL-адаптер модуля переписок.
package infrastructure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/internal/conversation/domain"
)

// PostgresRepository сохраняет канонические переписки в PostgreSQL.
type PostgresRepository struct{ pool *pgxpool.Pool }

// NewPostgresRepository создаёт хранилище переписок поверх общего пула.
func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

func (repository *PostgresRepository) Ingest(
	ctx context.Context,
	change domain.CanonicalChange,
	ids domain.CandidateIDs,
) (domain.IngestResult, error) {
	if repository == nil || repository.pool == nil || change.Validate() != nil || ids.Validate(change) != nil {
		return domain.IngestResult{}, domain.ErrInvalid
	}
	tx, err := repository.pool.Begin(ctx)
	if err != nil {
		return domain.IngestResult{}, fmt.Errorf("начало канонизации: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockAndValidateConnection(ctx, tx, change); err != nil {
		return domain.IngestResult{}, err
	}
	var result domain.IngestResult
	switch change.Type {
	case domain.ChangeReceived:
		result, err = ingestReceived(ctx, tx, change, ids)
	case domain.ChangeEdited:
		result, err = ingestEdited(ctx, tx, change, ids)
	case domain.ChangeDeleted:
		result, err = ingestDeleted(ctx, tx, change)
	default:
		err = domain.ErrInvalid
	}
	if err != nil {
		return domain.IngestResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.IngestResult{}, fmt.Errorf("фиксация канонизации: %w", err)
	}
	return result, nil
}

func ingestReceived(
	ctx context.Context,
	tx pgx.Tx,
	change domain.CanonicalChange,
	ids domain.CandidateIDs,
) (domain.IngestResult, error) {
	contact, err := resolveContact(ctx, tx, change, ids)
	if err != nil {
		return domain.IngestResult{}, err
	}
	conversation, found, err := conversationByExternal(ctx, tx, change.TenantID, change.ConnectionID, change.ConversationExternalID, true)
	if err != nil {
		return domain.IngestResult{}, err
	}
	if !found {
		conversation = domain.Conversation{
			ID: ids.ConversationID, TenantID: change.TenantID, LocationID: change.LocationID,
			ConnectionID: change.ConnectionID, ContactID: contact.ID, ExternalID: change.ConversationExternalID,
			Status: domain.ConversationActive, Revision: 0, CreatedAt: change.ReceivedAt.UTC(), UpdatedAt: change.ReceivedAt.UTC(),
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO conversations(
				id, tenant_id, location_id, connection_id, contact_id, external_id, status, revision, created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, 'ACTIVE', 0, $7, $7)`,
			conversation.ID, conversation.TenantID, conversation.LocationID, conversation.ConnectionID,
			conversation.ContactID, conversation.ExternalID, conversation.CreatedAt,
		); err != nil {
			return domain.IngestResult{}, mapPostgresError("создание переписки", err)
		}
	} else if conversation.ContactID != contact.ID {
		return domain.IngestResult{}, domain.ErrConflict
	}

	existing, messageFound, err := messageByExternal(ctx, tx, change.TenantID, change.ConnectionID, change.MessageExternalID, true)
	if err != nil {
		return domain.IngestResult{}, err
	}
	if messageFound {
		if existing.ConversationID != conversation.ID {
			return domain.IngestResult{}, domain.ErrConflict
		}
		same, err := sameCanonicalMessage(ctx, tx, existing, change)
		if err != nil {
			return domain.IngestResult{}, err
		}
		if !same {
			return domain.IngestResult{}, domain.ErrConflict
		}
		return domain.IngestResult{
			ContactID: contact.ID, ConversationID: conversation.ID, MessageID: existing.ID,
			Changed: false, Revision: conversation.Revision,
		}, nil
	}

	replyID, err := replyMessageID(ctx, tx, change, conversation.ID)
	if err != nil {
		return domain.IngestResult{}, err
	}
	message := domain.Message{
		ID: ids.MessageID, TenantID: change.TenantID, ConversationID: conversation.ID,
		ConnectionID: change.ConnectionID, ExternalID: change.MessageExternalID,
		Direction: change.Direction, Type: change.MessageType, Text: change.Text,
		SenderExternalID: change.SenderExternalID, ReplyToMessageID: replyID,
		SentAt: change.SentAt.UTC(), ReceivedAt: change.ReceivedAt.UTC(),
		Metadata: append(json.RawMessage(nil), change.Metadata...), CreatedAt: change.ReceivedAt.UTC(),
	}
	if message.Validate() != nil {
		return domain.IngestResult{}, domain.ErrInvalid
	}
	if err := insertMessage(ctx, tx, message); err != nil {
		return domain.IngestResult{}, err
	}
	if err := replaceAttachments(ctx, tx, message, change.Attachments, ids.AttachmentIDs); err != nil {
		return domain.IngestResult{}, err
	}
	revision, err := refreshConversation(ctx, tx, change.TenantID, conversation.ID, change.ReceivedAt)
	if err != nil {
		return domain.IngestResult{}, err
	}
	return domain.IngestResult{
		ContactID: contact.ID, ConversationID: conversation.ID, MessageID: message.ID, Changed: true, Revision: revision,
	}, nil
}

func ingestEdited(
	ctx context.Context,
	tx pgx.Tx,
	change domain.CanonicalChange,
	ids domain.CandidateIDs,
) (domain.IngestResult, error) {
	conversation, found, err := conversationByExternal(ctx, tx, change.TenantID, change.ConnectionID, change.ConversationExternalID, true)
	if err != nil || !found {
		if err == nil {
			err = domain.ErrNotFound
		}
		return domain.IngestResult{}, err
	}
	message, found, err := messageByExternal(ctx, tx, change.TenantID, change.ConnectionID, change.MessageExternalID, true)
	if err != nil || !found || message.ConversationID != conversation.ID {
		if err == nil {
			err = domain.ErrNotFound
		}
		return domain.IngestResult{}, err
	}
	same, err := sameCanonicalMessage(ctx, tx, message, change)
	if err != nil {
		return domain.IngestResult{}, err
	}
	if same {
		return domain.IngestResult{
			ContactID: conversation.ContactID, ConversationID: conversation.ID, MessageID: message.ID,
			Changed: false, Revision: conversation.Revision,
		}, nil
	}
	replyID, err := replyMessageID(ctx, tx, change, conversation.ID)
	if err != nil {
		return domain.IngestResult{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE messages
		SET direction = $4, type = $5, text = $6, sender_external_id = $7,
		    reply_to_message_id = $8, sent_at = $9, received_at = $10, metadata = $11::jsonb
		WHERE tenant_id = $1 AND id = $2 AND conversation_id = $3`,
		change.TenantID, message.ID, conversation.ID, change.Direction, change.MessageType, change.Text,
		change.SenderExternalID, replyID, change.SentAt.UTC(), change.ReceivedAt.UTC(), string(change.Metadata),
	); err != nil {
		return domain.IngestResult{}, mapPostgresError("изменение сообщения", err)
	}
	if err := replaceAttachments(ctx, tx, message, change.Attachments, ids.AttachmentIDs); err != nil {
		return domain.IngestResult{}, err
	}
	revision, err := refreshConversation(ctx, tx, change.TenantID, conversation.ID, change.ReceivedAt)
	if err != nil {
		return domain.IngestResult{}, err
	}
	return domain.IngestResult{
		ContactID: conversation.ContactID, ConversationID: conversation.ID, MessageID: message.ID,
		Changed: true, Revision: revision,
	}, nil
}

func ingestDeleted(ctx context.Context, tx pgx.Tx, change domain.CanonicalChange) (domain.IngestResult, error) {
	conversation, found, err := conversationByExternal(ctx, tx, change.TenantID, change.ConnectionID, change.ConversationExternalID, true)
	if err != nil || !found {
		if err == nil {
			err = domain.ErrNotFound
		}
		return domain.IngestResult{}, err
	}
	message, found, err := messageByExternal(ctx, tx, change.TenantID, change.ConnectionID, change.MessageExternalID, true)
	if err != nil || !found || message.ConversationID != conversation.ID {
		if err == nil {
			err = domain.ErrNotFound
		}
		return domain.IngestResult{}, err
	}
	if message.ProviderDeletedAt != nil {
		return domain.IngestResult{
			ContactID: conversation.ContactID, ConversationID: conversation.ID, MessageID: message.ID,
			Changed: false, Revision: conversation.Revision,
		}, nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE messages SET provider_deleted_at = $4, received_at = $5
		WHERE tenant_id = $1 AND id = $2 AND conversation_id = $3`,
		change.TenantID, message.ID, conversation.ID, change.OccurredAt.UTC(), change.ReceivedAt.UTC(),
	); err != nil {
		return domain.IngestResult{}, mapPostgresError("удаление сообщения у провайдера", err)
	}
	revision, err := refreshConversation(ctx, tx, change.TenantID, conversation.ID, change.ReceivedAt)
	if err != nil {
		return domain.IngestResult{}, err
	}
	return domain.IngestResult{
		ContactID: conversation.ContactID, ConversationID: conversation.ID, MessageID: message.ID,
		Changed: true, Revision: revision,
	}, nil
}

func resolveContact(
	ctx context.Context,
	tx pgx.Tx,
	change domain.CanonicalChange,
	ids domain.CandidateIDs,
) (domain.Contact, error) {
	var contactID string
	err := tx.QueryRow(ctx, `
		SELECT contact_id FROM external_identities
		WHERE tenant_id = $1 AND provider = $2 AND connection_id = $3 AND external_id = $4
		FOR UPDATE`, change.TenantID, change.Provider, change.ConnectionID, change.ContactExternalID).Scan(&contactID)
	if err == nil {
		contact, found, err := contactByID(ctx, tx, change.TenantID, contactID)
		if err != nil || !found {
			if err == nil {
				err = domain.ErrNotFound
			}
			return domain.Contact{}, err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE contacts
			SET display_name = CASE WHEN $6 >= updated_at THEN COALESCE($3, display_name) ELSE display_name END,
			    phone_normalized = CASE WHEN $6 >= updated_at THEN COALESCE($4, phone_normalized) ELSE phone_normalized END,
			    email_normalized = CASE WHEN $6 >= updated_at THEN COALESCE($5, email_normalized) ELSE email_normalized END,
			    updated_at = GREATEST(updated_at, $6)
			WHERE tenant_id = $1 AND id = $2`,
			change.TenantID, contact.ID, change.ContactDisplayName, change.ContactPhoneNormalized,
			change.ContactEmailNormalized, change.ReceivedAt.UTC(),
		); err != nil {
			return domain.Contact{}, mapPostgresError("обновление контакта", err)
		}
		if !change.ReceivedAt.Before(contact.UpdatedAt) {
			contact.DisplayName = prefer(change.ContactDisplayName, contact.DisplayName)
			contact.PhoneNormalized = prefer(change.ContactPhoneNormalized, contact.PhoneNormalized)
			contact.EmailNormalized = prefer(change.ContactEmailNormalized, contact.EmailNormalized)
			contact.UpdatedAt = change.ReceivedAt.UTC()
		}
		return contact, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return domain.Contact{}, fmt.Errorf("поиск внешней личности: %w", err)
	}
	contact := domain.Contact{
		ID: ids.ContactID, TenantID: change.TenantID, DisplayName: change.ContactDisplayName,
		PhoneNormalized: change.ContactPhoneNormalized, EmailNormalized: change.ContactEmailNormalized,
		CreatedAt: change.ReceivedAt.UTC(), UpdatedAt: change.ReceivedAt.UTC(),
	}
	if contact.Validate() != nil {
		return domain.Contact{}, domain.ErrInvalid
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO contacts(id, tenant_id, display_name, phone_normalized, email_normalized, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $6)`,
		contact.ID, contact.TenantID, contact.DisplayName, contact.PhoneNormalized, contact.EmailNormalized, contact.CreatedAt,
	); err != nil {
		return domain.Contact{}, mapPostgresError("создание контакта", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO external_identities(
			id, tenant_id, contact_id, provider, connection_id, external_id, metadata, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, '{}'::jsonb, $7)`,
		ids.ExternalIdentityID, change.TenantID, contact.ID, change.Provider,
		change.ConnectionID, change.ContactExternalID, change.ReceivedAt.UTC(),
	); err != nil {
		return domain.Contact{}, mapPostgresError("создание внешней личности", err)
	}
	return contact, nil
}

func lockAndValidateConnection(ctx context.Context, tx pgx.Tx, change domain.CanonicalChange) error {
	var locationID *string
	err := tx.QueryRow(ctx, `
		SELECT location_id FROM channel_connections
		WHERE tenant_id = $1 AND id = $2 AND provider = $3
		FOR UPDATE`, change.TenantID, change.ConnectionID, change.Provider).Scan(&locationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("проверка подключения: %w", err)
	}
	if !sameOptionalString(locationID, change.LocationID) {
		return domain.ErrInvalid
	}
	return nil
}

func insertMessage(ctx context.Context, tx pgx.Tx, message domain.Message) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO messages(
			id, tenant_id, conversation_id, connection_id, external_id, direction, type, text,
			sender_external_id, reply_to_message_id, sent_at, received_at, provider_deleted_at, metadata, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14::jsonb, $15)`,
		message.ID, message.TenantID, message.ConversationID, message.ConnectionID, message.ExternalID,
		message.Direction, message.Type, message.Text, message.SenderExternalID, message.ReplyToMessageID,
		message.SentAt, message.ReceivedAt, message.ProviderDeletedAt, string(message.Metadata), message.CreatedAt,
	)
	if err != nil {
		return mapPostgresError("создание сообщения", err)
	}
	return nil
}

func replaceAttachments(
	ctx context.Context,
	tx pgx.Tx,
	message domain.Message,
	inputs []domain.AttachmentInput,
	ids []string,
) error {
	if _, err := tx.Exec(ctx, `DELETE FROM attachments WHERE tenant_id = $1 AND message_id = $2`, message.TenantID, message.ID); err != nil {
		return fmt.Errorf("удаление метаданных вложений: %w", err)
	}
	for index, input := range inputs {
		attachment := domain.Attachment{
			ID: ids[index], TenantID: message.TenantID, MessageID: message.ID, ObjectKey: input.ObjectKey,
			MIMEType: input.MIMEType, SizeBytes: input.SizeBytes, SHA256: input.SHA256,
			ProviderFileID: input.ProviderFileID, CreatedAt: message.ReceivedAt.UTC(),
		}
		if attachment.Validate() != nil {
			return domain.ErrInvalid
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO attachments(
				id, tenant_id, message_id, object_key, mime_type, size_bytes, sha256, provider_file_id, created_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			attachment.ID, attachment.TenantID, attachment.MessageID, attachment.ObjectKey, attachment.MIMEType,
			attachment.SizeBytes, attachment.SHA256, attachment.ProviderFileID, attachment.CreatedAt,
		); err != nil {
			return mapPostgresError("создание метаданных вложения", err)
		}
	}
	return nil
}

func refreshConversation(ctx context.Context, tx pgx.Tx, tenantID, conversationID string, at time.Time) (int64, error) {
	var revision int64
	err := tx.QueryRow(ctx, `
		WITH message_bounds AS (
			SELECT min(sent_at) AS first_at, max(sent_at) AS last_at
			FROM messages WHERE tenant_id = $1 AND conversation_id = $2
		), last_message AS (
			SELECT direction FROM messages
			WHERE tenant_id = $1 AND conversation_id = $2
			ORDER BY sent_at DESC, id DESC LIMIT 1
		)
		UPDATE conversations
		SET first_message_at = message_bounds.first_at,
		    last_message_at = message_bounds.last_at,
		    last_message_direction = last_message.direction,
		    revision = conversations.revision + 1,
		    updated_at = $3
		FROM message_bounds, last_message
		WHERE conversations.tenant_id = $1 AND conversations.id = $2
		RETURNING conversations.revision`, tenantID, conversationID, at.UTC()).Scan(&revision)
	if err != nil {
		return 0, fmt.Errorf("обновление ревизии переписки: %w", err)
	}
	return revision, nil
}

func replyMessageID(ctx context.Context, tx pgx.Tx, change domain.CanonicalChange, conversationID string) (*string, error) {
	if change.ReplyToMessageExternalID == nil {
		return nil, nil
	}
	message, found, err := messageByExternal(ctx, tx, change.TenantID, change.ConnectionID, *change.ReplyToMessageExternalID, false)
	if err != nil {
		return nil, err
	}
	if !found || message.ConversationID != conversationID {
		return nil, domain.ErrNotFound
	}
	return &message.ID, nil
}

func sameCanonicalMessage(ctx context.Context, tx pgx.Tx, message domain.Message, change domain.CanonicalChange) (bool, error) {
	replyID, err := replyMessageID(ctx, tx, change, message.ConversationID)
	if err != nil {
		return false, err
	}
	if message.Direction != change.Direction || message.Type != change.MessageType ||
		!sameOptionalString(message.Text, change.Text) || !sameOptionalString(message.SenderExternalID, change.SenderExternalID) ||
		!sameOptionalString(message.ReplyToMessageID, replyID) || !message.SentAt.Equal(change.SentAt) ||
		!sameJSON(message.Metadata, change.Metadata) {
		return false, nil
	}
	attachments, err := attachmentsByMessage(ctx, tx, message.TenantID, message.ID)
	if err != nil {
		return false, err
	}
	return sameAttachments(attachments, change.Attachments), nil
}

func sameAttachments(stored []domain.Attachment, inputs []domain.AttachmentInput) bool {
	if len(stored) != len(inputs) {
		return false
	}
	storedSignatures := make([]string, 0, len(stored))
	inputSignatures := make([]string, 0, len(inputs))
	for _, attachment := range stored {
		storedSignatures = append(storedSignatures, attachmentSignature(
			attachment.ObjectKey, attachment.MIMEType, attachment.SizeBytes, attachment.SHA256, attachment.ProviderFileID,
		))
	}
	for _, attachment := range inputs {
		inputSignatures = append(inputSignatures, attachmentSignature(
			attachment.ObjectKey, attachment.MIMEType, attachment.SizeBytes, attachment.SHA256, attachment.ProviderFileID,
		))
	}
	sort.Strings(storedSignatures)
	sort.Strings(inputSignatures)
	for index := range storedSignatures {
		if storedSignatures[index] != inputSignatures[index] {
			return false
		}
	}
	return true
}

func attachmentSignature(objectKey string, mimeType *string, sizeBytes int64, sha256, providerFileID *string) string {
	value, _ := json.Marshal([]any{objectKey, mimeType, sizeBytes, sha256, providerFileID})
	return string(value)
}

func sameJSON(left, right json.RawMessage) bool {
	var leftValue, rightValue any
	return json.Unmarshal(left, &leftValue) == nil && json.Unmarshal(right, &rightValue) == nil &&
		reflect.DeepEqual(leftValue, rightValue)
}

func conversationByExternal(
	ctx context.Context,
	queryer interface {
		QueryRow(context.Context, string, ...any) pgx.Row
	},
	tenantID, connectionID, externalID string,
	lock bool,
) (domain.Conversation, bool, error) {
	query := `
		SELECT id, tenant_id, location_id, connection_id, contact_id, external_id, status,
		       first_message_at, last_message_at, last_message_direction, revision, created_at, updated_at
		FROM conversations
		WHERE tenant_id = $1 AND connection_id = $2 AND external_id = $3`
	if lock {
		query += " FOR UPDATE"
	}
	return scanConversation(queryer.QueryRow(ctx, query, tenantID, connectionID, externalID))
}

func messageByExternal(
	ctx context.Context,
	queryer interface {
		QueryRow(context.Context, string, ...any) pgx.Row
	},
	tenantID, connectionID, externalID string,
	lock bool,
) (domain.Message, bool, error) {
	query := `
		SELECT id, tenant_id, conversation_id, connection_id, external_id, direction, type, text,
		       sender_external_id, reply_to_message_id, sent_at, received_at, provider_deleted_at, metadata, created_at
		FROM messages
		WHERE tenant_id = $1 AND connection_id = $2 AND external_id = $3`
	if lock {
		query += " FOR UPDATE"
	}
	return scanMessage(queryer.QueryRow(ctx, query, tenantID, connectionID, externalID))
}

func contactByID(ctx context.Context, queryer pgx.Tx, tenantID, contactID string) (domain.Contact, bool, error) {
	var contact domain.Contact
	err := queryer.QueryRow(ctx, `
		SELECT id, tenant_id, display_name, phone_normalized, email_normalized, created_at, updated_at
		FROM contacts WHERE tenant_id = $1 AND id = $2`, tenantID, contactID).Scan(
		&contact.ID, &contact.TenantID, &contact.DisplayName, &contact.PhoneNormalized,
		&contact.EmailNormalized, &contact.CreatedAt, &contact.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Contact{}, false, nil
	}
	if err != nil {
		return domain.Contact{}, false, fmt.Errorf("чтение контакта: %w", err)
	}
	if contact.Validate() != nil {
		return domain.Contact{}, false, domain.ErrInvalid
	}
	return contact, true, nil
}

func (repository *PostgresRepository) List(
	ctx context.Context,
	tenantID string,
	limit int,
	cursor *domain.PageCursor,
) ([]domain.Conversation, bool, error) {
	if repository == nil || repository.pool == nil || tenantID == "" || limit < 1 || limit > 100 {
		return nil, false, domain.ErrInvalid
	}
	rows, err := repository.pool.Query(ctx, `
		SELECT id, tenant_id, location_id, connection_id, contact_id, external_id, status,
		       first_message_at, last_message_at, last_message_direction, revision, created_at, updated_at
		FROM conversations
		WHERE tenant_id = $1 AND ($2::timestamptz IS NULL OR (updated_at, id) < ($2, $3::uuid))
		ORDER BY updated_at DESC, id DESC LIMIT $4`, tenantID, cursorTime(cursor), cursorID(cursor), limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("список переписок: %w", err)
	}
	defer rows.Close()
	items := make([]domain.Conversation, 0, limit+1)
	for rows.Next() {
		conversation, _, scanErr := scanConversation(rows)
		if scanErr != nil {
			return nil, false, scanErr
		}
		items = append(items, conversation)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("обход списка переписок: %w", err)
	}
	more := len(items) > limit
	if more {
		items = items[:limit]
	}
	return items, more, nil
}

func (repository *PostgresRepository) Detail(
	ctx context.Context,
	tenantID, conversationID string,
) (domain.ConversationDetail, bool, error) {
	if repository == nil || repository.pool == nil || tenantID == "" || conversationID == "" {
		return domain.ConversationDetail{}, false, domain.ErrInvalid
	}
	row := repository.pool.QueryRow(ctx, `
		SELECT c.id, c.tenant_id, c.location_id, c.connection_id, c.contact_id, c.external_id, c.status,
		       c.first_message_at, c.last_message_at, c.last_message_direction, c.revision, c.created_at, c.updated_at,
		       ct.id, ct.tenant_id, ct.display_name, ct.phone_normalized, ct.email_normalized, ct.created_at, ct.updated_at
		FROM conversations c
		JOIN contacts ct ON ct.tenant_id = c.tenant_id AND ct.id = c.contact_id
		WHERE c.tenant_id = $1 AND c.id = $2`, tenantID, conversationID)
	var detail domain.ConversationDetail
	err := row.Scan(
		&detail.Conversation.ID, &detail.Conversation.TenantID, &detail.Conversation.LocationID,
		&detail.Conversation.ConnectionID, &detail.Conversation.ContactID, &detail.Conversation.ExternalID,
		&detail.Conversation.Status, &detail.Conversation.FirstMessageAt, &detail.Conversation.LastMessageAt,
		&detail.Conversation.LastMessageDirection, &detail.Conversation.Revision, &detail.Conversation.CreatedAt,
		&detail.Conversation.UpdatedAt, &detail.Contact.ID, &detail.Contact.TenantID, &detail.Contact.DisplayName,
		&detail.Contact.PhoneNormalized, &detail.Contact.EmailNormalized, &detail.Contact.CreatedAt, &detail.Contact.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ConversationDetail{}, false, nil
	}
	if err != nil {
		return domain.ConversationDetail{}, false, fmt.Errorf("чтение переписки: %w", err)
	}
	if detail.Conversation.Validate() != nil || detail.Contact.Validate() != nil {
		return domain.ConversationDetail{}, false, domain.ErrInvalid
	}
	return detail, true, nil
}

func (repository *PostgresRepository) Messages(
	ctx context.Context,
	tenantID, conversationID string,
	limit int,
	cursor *domain.PageCursor,
) ([]domain.MessageView, bool, error) {
	if repository == nil || repository.pool == nil || tenantID == "" || conversationID == "" || limit < 1 || limit > 100 {
		return nil, false, domain.ErrInvalid
	}
	var exists bool
	if err := repository.pool.QueryRow(ctx, `SELECT true FROM conversations WHERE tenant_id = $1 AND id = $2`, tenantID, conversationID).Scan(&exists); errors.Is(err, pgx.ErrNoRows) {
		return nil, false, domain.ErrNotFound
	} else if err != nil {
		return nil, false, fmt.Errorf("проверка переписки: %w", err)
	}
	rows, err := repository.pool.Query(ctx, `
		SELECT id, tenant_id, conversation_id, connection_id, external_id, direction, type, text,
		       sender_external_id, reply_to_message_id, sent_at, received_at, provider_deleted_at, metadata, created_at
		FROM messages
		WHERE tenant_id = $1 AND conversation_id = $2
		  AND ($3::timestamptz IS NULL OR (sent_at, id) < ($3, $4::uuid))
		ORDER BY sent_at DESC, id DESC LIMIT $5`,
		tenantID, conversationID, cursorTime(cursor), cursorID(cursor), limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("список сообщений: %w", err)
	}
	defer rows.Close()
	items := make([]domain.MessageView, 0, limit+1)
	for rows.Next() {
		message, _, scanErr := scanMessage(rows)
		if scanErr != nil {
			return nil, false, scanErr
		}
		attachments, attachmentErr := attachmentsByMessage(ctx, repository.pool, tenantID, message.ID)
		if attachmentErr != nil {
			return nil, false, attachmentErr
		}
		items = append(items, domain.MessageView{Message: message, Attachments: attachments})
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("обход списка сообщений: %w", err)
	}
	more := len(items) > limit
	if more {
		items = items[:limit]
	}
	return items, more, nil
}

type rowScanner interface{ Scan(...any) error }

func scanConversation(row rowScanner) (domain.Conversation, bool, error) {
	var conversation domain.Conversation
	err := row.Scan(
		&conversation.ID, &conversation.TenantID, &conversation.LocationID, &conversation.ConnectionID,
		&conversation.ContactID, &conversation.ExternalID, &conversation.Status, &conversation.FirstMessageAt,
		&conversation.LastMessageAt, &conversation.LastMessageDirection, &conversation.Revision,
		&conversation.CreatedAt, &conversation.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Conversation{}, false, nil
	}
	if err != nil {
		return domain.Conversation{}, false, fmt.Errorf("чтение переписки: %w", err)
	}
	if conversation.Validate() != nil {
		return domain.Conversation{}, false, domain.ErrInvalid
	}
	return conversation, true, nil
}

func scanMessage(row rowScanner) (domain.Message, bool, error) {
	var message domain.Message
	err := row.Scan(
		&message.ID, &message.TenantID, &message.ConversationID, &message.ConnectionID, &message.ExternalID,
		&message.Direction, &message.Type, &message.Text, &message.SenderExternalID, &message.ReplyToMessageID,
		&message.SentAt, &message.ReceivedAt, &message.ProviderDeletedAt, &message.Metadata, &message.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Message{}, false, nil
	}
	if err != nil {
		return domain.Message{}, false, fmt.Errorf("чтение сообщения: %w", err)
	}
	if message.Validate() != nil {
		return domain.Message{}, false, domain.ErrInvalid
	}
	return message, true, nil
}

func attachmentsByMessage(
	ctx context.Context,
	queryer interface {
		Query(context.Context, string, ...any) (pgx.Rows, error)
	},
	tenantID, messageID string,
) ([]domain.Attachment, error) {
	rows, err := queryer.Query(ctx, `
		SELECT id, tenant_id, message_id, object_key, mime_type, size_bytes, sha256, provider_file_id, created_at
		FROM attachments WHERE tenant_id = $1 AND message_id = $2
		ORDER BY object_key, id`, tenantID, messageID)
	if err != nil {
		return nil, fmt.Errorf("чтение вложений: %w", err)
	}
	defer rows.Close()
	items := make([]domain.Attachment, 0)
	for rows.Next() {
		var attachment domain.Attachment
		if err := rows.Scan(
			&attachment.ID, &attachment.TenantID, &attachment.MessageID, &attachment.ObjectKey,
			&attachment.MIMEType, &attachment.SizeBytes, &attachment.SHA256,
			&attachment.ProviderFileID, &attachment.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("чтение вложения: %w", err)
		}
		if attachment.Validate() != nil {
			return nil, domain.ErrInvalid
		}
		items = append(items, attachment)
	}
	return items, rows.Err()
}

func cursorTime(cursor *domain.PageCursor) *time.Time {
	if cursor == nil {
		return nil
	}
	value := cursor.At.UTC()
	return &value
}

func cursorID(cursor *domain.PageCursor) *string {
	if cursor == nil {
		return nil
	}
	return &cursor.ID
}

func sameOptionalString(left, right *string) bool {
	return reflect.DeepEqual(left, right)
}

func prefer(candidate, current *string) *string {
	if candidate != nil {
		return candidate
	}
	return current
}

func mapPostgresError(operation string, err error) error {
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) {
		switch postgresError.Code {
		case "23505":
			return domain.ErrConflict
		case "23503":
			return domain.ErrNotFound
		case "23514", "22P02", "22001", "22003":
			return domain.ErrInvalid
		}
	}
	return fmt.Errorf("%s: %w", operation, err)
}
