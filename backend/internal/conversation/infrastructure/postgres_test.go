package infrastructure

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	connectordomain "lidradar/backend/internal/connector/domain"
	connectorinfrastructure "lidradar/backend/internal/connector/infrastructure"
	conversationapplication "lidradar/backend/internal/conversation/application"
	"lidradar/backend/internal/conversation/domain"
	"lidradar/backend/internal/testsupport"
	"lidradar/backend/platform/ids"
)

type allowReader bool

func (allowed allowReader) Allowed(_ context.Context, _, _, permission string) (bool, error) {
	return bool(allowed) && permission == conversationapplication.PermissionRead, nil
}

func TestConversationCoreCreateReuseEditDeleteRevisionAndIsolation(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	pair := testsupport.TwoTenants(t, ctx, pool)
	generator := ids.Generator{}
	connectorRepository := connectorinfrastructure.NewPostgresRepository(pool)
	conversationRepository := NewPostgresRepository(pool)
	service := conversationapplication.NewService(conversationRepository, allowReader(true), generator)
	connection := conversationConnection(t, connectorRepository, generator, pair.A.TenantID, &pair.A.LocationID)

	baseTime := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
	first := canonicalMessage(connection, "event-1", "dialog-1", "message-1", "contact-1", connectordomain.CanonicalIncoming, baseTime)
	if err := service.IngestCanonical(ctx, first); err != nil {
		t.Fatalf("первое сообщение: %v", err)
	}
	requireConversationCounts(t, pool, pair.A.TenantID, 1, 1, 1, 1, 0)
	conversation := onlyConversation(t, conversationRepository, pair.A.TenantID)
	if conversation.Revision != 1 || conversation.LastMessageDirection == nil || *conversation.LastMessageDirection != domain.DirectionIncoming {
		t.Fatalf("переписка после первого сообщения = %#v", conversation)
	}

	second := canonicalMessage(connection, "event-2", "dialog-1", "message-2", "contact-1", connectordomain.CanonicalOutgoing, baseTime.Add(time.Minute))
	replyExternalID := "message-1"
	second.ReplyToMessageExternalID = &replyExternalID
	text := "Ответ владельца"
	second.Text = &text
	if err := service.IngestCanonical(ctx, second); err != nil {
		t.Fatalf("второе сообщение: %v", err)
	}
	requireConversationCounts(t, pool, pair.A.TenantID, 1, 1, 1, 2, 0)
	conversation = onlyConversation(t, conversationRepository, pair.A.TenantID)
	if conversation.Revision != 2 || *conversation.LastMessageDirection != domain.DirectionOutgoing {
		t.Fatalf("переписка после второго сообщения = %#v", conversation)
	}

	if err := service.IngestCanonical(ctx, first); err != nil {
		t.Fatalf("безопасный повтор первого сообщения: %v", err)
	}
	requireConversationCounts(t, pool, pair.A.TenantID, 1, 1, 1, 2, 0)
	if revision := onlyConversation(t, conversationRepository, pair.A.TenantID).Revision; revision != 2 {
		t.Fatalf("revision после повтора = %d, нужно 2", revision)
	}

	edited := second
	edited.SourceEventID = "event-3"
	edited.Type = connectordomain.CanonicalMessageEdited
	edited.OccurredAt = baseTime.Add(2 * time.Minute)
	edited.ReceivedAt = edited.OccurredAt
	editedText := "Исправленный ответ"
	edited.Text = &editedText
	mime := "application/pdf"
	providerFileID := "provider-file-1"
	edited.Attachments = []connectordomain.CanonicalAttachment{{
		ObjectKey: "fixtures/document-1.pdf", MIMEType: &mime, SizeBytes: 2048, ProviderFileID: &providerFileID,
	}}
	if err := service.IngestCanonical(ctx, edited); err != nil {
		t.Fatalf("изменение сообщения: %v", err)
	}
	requireConversationCounts(t, pool, pair.A.TenantID, 1, 1, 1, 2, 1)
	if revision := onlyConversation(t, conversationRepository, pair.A.TenantID).Revision; revision != 3 {
		t.Fatalf("revision после изменения = %d, нужно 3", revision)
	}
	if err := service.IngestCanonical(ctx, edited); err != nil {
		t.Fatalf("повтор изменения: %v", err)
	}
	if revision := onlyConversation(t, conversationRepository, pair.A.TenantID).Revision; revision != 3 {
		t.Fatalf("revision после повтора изменения = %d, нужно 3", revision)
	}

	deleted := connectordomain.CanonicalEvent{
		SourceEventID: "event-4", Type: connectordomain.CanonicalMessageDeleted,
		TenantID: pair.A.TenantID, ConnectionID: connection.ID, LocationID: connection.LocationID,
		Provider: connection.Provider, ConversationExternalID: "dialog-1", MessageExternalID: "message-1",
		OccurredAt: baseTime.Add(3 * time.Minute), ReceivedAt: baseTime.Add(3 * time.Minute),
		Attachments: []connectordomain.CanonicalAttachment{}, Metadata: json.RawMessage(`{}`),
	}
	if err := service.IngestCanonical(ctx, deleted); err != nil {
		t.Fatalf("удаление сообщения: %v", err)
	}
	if err := service.IngestCanonical(ctx, deleted); err != nil {
		t.Fatalf("повтор удаления: %v", err)
	}
	conversation = onlyConversation(t, conversationRepository, pair.A.TenantID)
	if conversation.Revision != 4 {
		t.Fatalf("revision после удаления и повтора = %d, нужно 4", conversation.Revision)
	}

	detail, found, err := conversationRepository.Detail(ctx, pair.A.TenantID, conversation.ID)
	if err != nil || !found || detail.Contact.ID != conversation.ContactID {
		t.Fatalf("Detail() = %#v, found=%v, err=%v", detail, found, err)
	}
	messages, more, err := conversationRepository.Messages(ctx, pair.A.TenantID, conversation.ID, 1, nil)
	if err != nil || len(messages) != 1 || !more || messages[0].Message.ExternalID != "message-2" || len(messages[0].Attachments) != 1 {
		t.Fatalf("Messages(limit=1) = %#v, more=%v, err=%v", messages, more, err)
	}
	cursor := &domain.PageCursor{At: messages[0].Message.SentAt, ID: messages[0].Message.ID}
	messages, more, err = conversationRepository.Messages(ctx, pair.A.TenantID, conversation.ID, 10, cursor)
	if err != nil || len(messages) != 1 || more || messages[0].Message.ProviderDeletedAt == nil {
		t.Fatalf("Messages(next) = %#v, more=%v, err=%v", messages, more, err)
	}

	if _, found, err := conversationRepository.Detail(ctx, pair.B.TenantID, conversation.ID); err != nil || found {
		t.Fatalf("чужая переписка: found=%v, err=%v", found, err)
	}
	if _, _, err := conversationRepository.Messages(ctx, pair.B.TenantID, conversation.ID, 10, nil); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("чужие сообщения: %v", err)
	}
}

func TestExternalIdentityNamespaceIncludesConnection(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	pair := testsupport.TwoTenants(t, ctx, pool)
	generator := ids.Generator{}
	connectorRepository := connectorinfrastructure.NewPostgresRepository(pool)
	conversationRepository := NewPostgresRepository(pool)
	service := conversationapplication.NewService(conversationRepository, allowReader(true), generator)
	firstConnection := conversationConnection(t, connectorRepository, generator, pair.A.TenantID, nil)
	secondConnection := conversationConnection(t, connectorRepository, generator, pair.A.TenantID, nil)
	now := time.Now().UTC()
	if err := service.IngestCanonical(ctx, canonicalMessage(firstConnection, "event-1", "dialog", "message", "same-contact", connectordomain.CanonicalIncoming, now)); err != nil {
		t.Fatal(err)
	}
	if err := service.IngestCanonical(ctx, canonicalMessage(secondConnection, "event-2", "dialog", "message", "same-contact", connectordomain.CanonicalIncoming, now)); err != nil {
		t.Fatal(err)
	}
	requireConversationCounts(t, pool, pair.A.TenantID, 2, 2, 2, 2, 0)
}

func TestConversationChangeAndOutboxEventRollbackTogether(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	pair := testsupport.TwoTenants(t, ctx, pool)
	generator := ids.Generator{}
	connectorRepository := connectorinfrastructure.NewPostgresRepository(pool)
	repository := NewPostgresRepository(pool)
	connection := conversationConnection(t, connectorRepository, generator, pair.A.TenantID, nil)
	now := time.Now().UTC()
	text := "Нужна полировка"
	change := domain.CanonicalChange{
		SourceEventID: "source", Type: domain.ChangeReceived, TenantID: pair.A.TenantID,
		ConnectionID: connection.ID, Provider: string(connection.Provider), ConversationExternalID: "dialog-rollback",
		MessageExternalID: "message-rollback", ContactExternalID: "contact-rollback",
		Direction: domain.DirectionIncoming, MessageType: domain.MessageText, Text: &text,
		SentAt: now, OccurredAt: now, ReceivedAt: now, Metadata: json.RawMessage(`{}`),
	}
	values := make([]string, 4)
	for index := range values {
		value, err := generator.NewID()
		if err != nil {
			t.Fatal(err)
		}
		values[index] = value
	}
	_, err := repository.Ingest(ctx, change, domain.CandidateIDs{
		ContactID: values[0], ExternalIdentityID: values[1], ConversationID: values[2], MessageID: values[3],
		OutboxEventID: "not-a-uuid", AttachmentIDs: []string{},
	})
	if !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("Ingest() error = %v", err)
	}
	requireConversationCounts(t, pool, pair.A.TenantID, 0, 0, 0, 0, 0)
	var outboxCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1`, pair.A.TenantID).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if outboxCount != 0 {
		t.Fatalf("после отката осталось событий: %d", outboxCount)
	}
}

func conversationConnection(
	t *testing.T,
	repository *connectorinfrastructure.PostgresRepository,
	generator ids.Generator,
	tenantID string,
	locationID *string,
) connectordomain.ChannelConnection {
	t.Helper()
	id, err := generator.NewID()
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("conversation-fixture-secret"))
	now := time.Now().UTC()
	connection, err := connectordomain.NewChannelConnection(
		id, tenantID, locationID, connectordomain.ProviderTest, "Тестовая переписка",
		[]connectordomain.Capability{connectordomain.CapabilityReceiveMessages}, hex.EncodeToString(digest[:]),
		connectordomain.ConnectionHealth{Status: connectordomain.ConnectionActive, CheckedAt: now}, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.CreateConnection(context.Background(), tenantID, connection); err != nil {
		t.Fatal(err)
	}
	return connection
}

func canonicalMessage(
	connection connectordomain.ChannelConnection,
	eventID, conversationID, messageID, contactID string,
	direction connectordomain.CanonicalDirection,
	at time.Time,
) connectordomain.CanonicalEvent {
	text := "Первое сообщение"
	displayName := "Ирина"
	return connectordomain.CanonicalEvent{
		SourceEventID: eventID, Type: connectordomain.CanonicalMessageReceived,
		TenantID: connection.TenantID, ConnectionID: connection.ID, LocationID: connection.LocationID,
		Provider: connection.Provider, ConversationExternalID: conversationID, MessageExternalID: messageID,
		ContactExternalID: contactID, ContactDisplayName: &displayName, Direction: direction,
		MessageType: connectordomain.CanonicalText, Text: &text, SentAt: at, OccurredAt: at, ReceivedAt: at,
		Attachments: []connectordomain.CanonicalAttachment{}, Metadata: json.RawMessage(`{"fixture":true}`),
	}
}

func onlyConversation(t *testing.T, repository *PostgresRepository, tenantID string) domain.Conversation {
	t.Helper()
	items, more, err := repository.List(context.Background(), tenantID, domain.ListFilter{}, 10, nil)
	if err != nil || more || len(items) != 1 {
		t.Fatalf("List() = %#v, more=%v, err=%v", items, more, err)
	}
	return items[0].Conversation
}

func requireConversationCounts(
	t *testing.T,
	pool *pgxpool.Pool,
	tenantID string,
	wantContacts, wantIdentities, wantConversations, wantMessages, wantAttachments int,
) {
	t.Helper()
	queries := []struct {
		table string
		want  int
	}{
		{"contacts", wantContacts}, {"external_identities", wantIdentities},
		{"conversations", wantConversations}, {"messages", wantMessages}, {"attachments", wantAttachments},
	}
	for _, query := range queries {
		var count int
		if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM "+query.table+" WHERE tenant_id = $1", tenantID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != query.want {
			t.Fatalf("%s: count=%d, нужно %d", query.table, count, query.want)
		}
	}
}

// Регрессия этапа 25: страница сообщений читалась с открытым курсором и вложенным запросом
// вложений, то есть требовала два соединения на запрос и при конкуренции >= размера пула
// блокировала пул навсегда. Пул из двух соединений и шесть параллельных читателей
// должны завершиться, а не упереться в дедлайн контекста.
func TestMessagesPageUsesOneConnectionUnderContention(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	pair := testsupport.TwoTenants(t, ctx, pool)
	generator := ids.Generator{}
	connectorRepository := connectorinfrastructure.NewPostgresRepository(pool)
	repository := NewPostgresRepository(pool)
	service := conversationapplication.NewService(repository, allowReader(true), generator)
	connection := conversationConnection(t, connectorRepository, generator, pair.A.TenantID, &pair.A.LocationID)
	baseTime := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	mime := "image/jpeg"
	for index := 0; index < 5; index++ {
		event := canonicalMessage(connection, fmt.Sprintf("event-%d", index), "dialog-1", fmt.Sprintf("message-%d", index),
			"contact-1", connectordomain.CanonicalIncoming, baseTime.Add(time.Duration(index)*time.Minute))
		event.Attachments = []connectordomain.CanonicalAttachment{{
			ObjectKey: fmt.Sprintf("fixtures/photo-%d.jpg", index), MIMEType: &mime, SizeBytes: 1024,
		}}
		if err := service.IngestCanonical(ctx, event); err != nil {
			t.Fatalf("сообщение %d: %v", index, err)
		}
	}
	conversation := onlyConversation(t, repository, pair.A.TenantID)

	configuration := pool.Config()
	configuration.MaxConns = 2
	configuration.MinConns = 0
	small, err := pgxpool.NewWithConfig(ctx, configuration)
	if err != nil {
		t.Fatalf("пул из двух соединений: %v", err)
	}
	t.Cleanup(small.Close)
	contended := NewPostgresRepository(small)

	deadline, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	const readers = 6
	failures := make(chan error, readers)
	var wait sync.WaitGroup
	for reader := 0; reader < readers; reader++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			messages, more, err := contended.Messages(deadline, pair.A.TenantID, conversation.ID, 3, nil)
			if err != nil {
				failures <- err
				return
			}
			if len(messages) != 3 || !more {
				failures <- fmt.Errorf("страница = %d сообщений, more=%v", len(messages), more)
				return
			}
			for _, view := range messages {
				if len(view.Attachments) != 1 || view.Attachments[0].MessageID != view.Message.ID {
					failures <- fmt.Errorf("вложения сообщения %s = %#v", view.Message.ID, view.Attachments)
					return
				}
			}
		}()
	}
	wait.Wait()
	close(failures)
	for err := range failures {
		t.Fatalf("конкурентное чтение страницы при пуле из двух соединений: %v", err)
	}
}

// Список переписок для экрана «Диалоги» (ADR 0044): контакт, канал, превью,
// активные риски и внешняя ссылка одним запросом; серверные фильтры и курсор,
// привязанный к фильтрам.
func TestConversationListFiltersSearchRisksAndCursor(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	pair := testsupport.TwoTenants(t, ctx, pool)
	generator := ids.Generator{}
	connectorRepository := connectorinfrastructure.NewPostgresRepository(pool)
	repository := NewPostgresRepository(pool)
	service := conversationapplication.NewService(repository, allowReader(true), generator)
	connection := conversationConnection(t, connectorRepository, generator, pair.A.TenantID, &pair.A.LocationID)
	baseTime := time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)

	irina := canonicalMessage(connection, "event-irina", "dialog-irina", "message-irina", "contact-irina", connectordomain.CanonicalIncoming, baseTime)
	if err := service.IngestCanonical(ctx, irina); err != nil {
		t.Fatal(err)
	}
	petr := canonicalMessage(connection, "event-petr", "dialog-petr", "message-petr", "contact-petr", connectordomain.CanonicalIncoming, baseTime.Add(time.Minute))
	petrName := "Пётр Иванов"
	petrPhone := "+79991234567"
	petrText := "Хочу   полировку\nкузова"
	petr.ContactDisplayName, petr.ContactPhoneNormalized, petr.Text = &petrName, &petrPhone, &petrText
	if err := service.IngestCanonical(ctx, petr); err != nil {
		t.Fatal(err)
	}
	nameless := canonicalMessage(connection, "event-nameless", "dialog-nameless", "message-nameless", "contact-nameless", connectordomain.CanonicalIncoming, baseTime.Add(2*time.Minute))
	nameless.ContactDisplayName = nil
	nameless.MessageType, nameless.Text = connectordomain.CanonicalImage, nil
	if err := service.IngestCanonical(ctx, nameless); err != nil {
		t.Fatal(err)
	}

	// Активный риск у переписки Петра: сделка + два сигнала разной серьёзности.
	var petrConversationID, petrMessageID string
	if err := pool.QueryRow(ctx, `
		SELECT c.id, m.id FROM conversations AS c JOIN messages AS m ON m.tenant_id = c.tenant_id AND m.conversation_id = c.id
		WHERE c.tenant_id = $1 AND c.external_id = 'dialog-petr'`, pair.A.TenantID).Scan(&petrConversationID, &petrMessageID); err != nil {
		t.Fatal(err)
	}
	opportunityID := newConversationTestID(t)
	if _, err := pool.Exec(ctx, `
		INSERT INTO opportunities(id, tenant_id, conversation_id, stage, currency, opened_at, created_at, updated_at)
		VALUES ($1, $2, $3, 'NEW', 'RUB', $4, $4, $4)`, opportunityID, pair.A.TenantID, petrConversationID, baseTime); err != nil {
		t.Fatal(err)
	}
	for _, risk := range []struct{ riskType, severity, status string }{
		{"NO_RESPONSE", "HIGH", "OPEN"}, {"FOLLOW_UP_CANDIDATE", "CRITICAL", "ACKNOWLEDGED"}, {"BOOKING_NOT_CONFIRMED", "CRITICAL", "RESOLVED"},
	} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO risk_signals(id, tenant_id, opportunity_id, location_id, type, severity, status, reason_code, reason_text, source,
				risk_engine_version, trigger_message_id, detected_at, due_at, acknowledged_at, resolved_at, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7::text, 'FIXTURE', 'fixture', 'RULE', 'test/v1', $8, $9::timestamptz, $9::timestamptz,
				CASE WHEN $7::text = 'ACKNOWLEDGED' THEN $9::timestamptz ELSE NULL END,
				CASE WHEN $7::text = 'RESOLVED' THEN $9::timestamptz ELSE NULL END, $9::timestamptz, $9::timestamptz)`,
			newConversationTestID(t), pair.A.TenantID, opportunityID, pair.A.LocationID, risk.riskType, risk.severity, risk.status,
			petrMessageID, baseTime.Add(3*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}

	everything, err := service.List(ctx, "reader", pair.A.TenantID, conversationapplication.ListQuery{})
	if err != nil || len(everything.Items) != 3 || everything.NextCursor != nil {
		t.Fatalf("List() = %#v, %v", everything, err)
	}
	last := everything.Items[0] // updated_at DESC: последней ингестирована безымянная переписка
	if last.Contact.DisplayName != nil || last.LastMessage == nil || last.LastMessage.Type != domain.MessageImage || last.LastMessage.Preview != nil ||
		last.Channel.Provider != "TEST" || last.Channel.ConnectionID != connection.ID || last.ActiveRisks.Count != 0 || last.ActiveRisks.MaxSeverity != nil ||
		last.ExternalLink.UnavailableReason == nil || *last.ExternalLink.UnavailableReason != "PROVIDER_UNSUPPORTED" {
		t.Fatalf("строка без имени = %#v", last)
	}
	petrRow := everything.Items[1]
	if petrRow.Contact.DisplayName == nil || *petrRow.Contact.DisplayName != petrName || petrRow.LastMessage == nil ||
		petrRow.LastMessage.Preview == nil || *petrRow.LastMessage.Preview != "Хочу полировку кузова" ||
		petrRow.ActiveRisks.Count != 2 || petrRow.ActiveRisks.MaxSeverity == nil || *petrRow.ActiveRisks.MaxSeverity != "CRITICAL" {
		t.Fatalf("строка Петра = %#v", petrRow)
	}

	withRisk, err := service.List(ctx, "reader", pair.A.TenantID, conversationapplication.ListQuery{Filter: domain.ListFilter{WithRisk: true}})
	if err != nil || len(withRisk.Items) != 1 || withRisk.Items[0].Conversation.ID != petrConversationID {
		t.Fatalf("фильтр «С риском» = %#v, %v", withRisk, err)
	}
	for _, search := range []string{"пётр", "ИВАНОВ", "999 123", "+7999", "Ирина"} {
		found, err := service.List(ctx, "reader", pair.A.TenantID, conversationapplication.ListQuery{Filter: domain.ListFilter{Search: search}})
		if err != nil || len(found.Items) != 1 {
			t.Fatalf("поиск %q = %#v, %v", search, found, err)
		}
	}
	if found, err := service.List(ctx, "reader", pair.A.TenantID, conversationapplication.ListQuery{Filter: domain.ListFilter{Search: "%"}}); err != nil || len(found.Items) != 0 {
		t.Fatalf("метасимвол поиска не экранирован: %#v, %v", found, err)
	}
	if _, err := service.List(ctx, "reader", pair.A.TenantID, conversationapplication.ListQuery{Filter: domain.ListFilter{Search: strings.Repeat("а", 101)}}); !errors.Is(err, conversationapplication.ErrInvalid) {
		t.Fatalf("длинный поиск принят: %v", err)
	}
	byConnection, err := service.List(ctx, "reader", pair.A.TenantID, conversationapplication.ListQuery{Filter: domain.ListFilter{ConnectionID: connection.ID, LocationID: pair.A.LocationID, Status: domain.ConversationActive}})
	if err != nil || len(byConnection.Items) != 3 {
		t.Fatalf("фильтр по каналу и точке = %#v, %v", byConnection, err)
	}
	if other, err := service.List(ctx, "reader", pair.A.TenantID, conversationapplication.ListQuery{Filter: domain.ListFilter{LocationID: pair.B.LocationID}}); err != nil || len(other.Items) != 0 {
		t.Fatalf("чужая точка вернула переписки: %#v, %v", other, err)
	}

	firstPage, err := service.List(ctx, "reader", pair.A.TenantID, conversationapplication.ListQuery{Filter: domain.ListFilter{Search: "и"}, Limit: 1})
	if err != nil || len(firstPage.Items) != 1 || firstPage.NextCursor == nil {
		t.Fatalf("первая страница поиска = %#v, %v", firstPage, err)
	}
	secondPage, err := service.List(ctx, "reader", pair.A.TenantID, conversationapplication.ListQuery{Filter: domain.ListFilter{Search: "и"}, Limit: 1, Cursor: *firstPage.NextCursor})
	if err != nil || len(secondPage.Items) != 1 || secondPage.Items[0].Conversation.ID == firstPage.Items[0].Conversation.ID || secondPage.NextCursor != nil {
		t.Fatalf("вторая страница поиска = %#v, %v", secondPage, err)
	}
	if _, err := service.List(ctx, "reader", pair.A.TenantID, conversationapplication.ListQuery{Limit: 1, Cursor: *firstPage.NextCursor}); !errors.Is(err, conversationapplication.ErrInvalid) {
		t.Fatalf("курсор поиска принят без фильтра: %v", err)
	}

	detail, found, err := repository.Detail(ctx, pair.A.TenantID, petrConversationID)
	if err != nil || !found || detail.Channel.Provider != "TEST" || detail.Channel.Name != "Тестовая переписка" ||
		detail.ExternalLink.URL != nil || detail.ExternalLink.UnavailableReason == nil {
		t.Fatalf("Detail() = %#v, %v, %v", detail, found, err)
	}
	if foreign, err := service.List(ctx, "reader", pair.B.TenantID, conversationapplication.ListQuery{}); err != nil || len(foreign.Items) != 0 {
		t.Fatalf("список чужой организации = %#v, %v", foreign, err)
	}
}

func newConversationTestID(t *testing.T) string {
	t.Helper()
	value, err := (ids.Generator{}).NewID()
	if err != nil {
		t.Fatal(err)
	}
	return value
}
