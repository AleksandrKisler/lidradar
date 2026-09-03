package infrastructure

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/internal/notification/application"
	"lidradar/backend/internal/notification/domain"
	"lidradar/backend/internal/testsupport"
	"lidradar/backend/platform/ids"
)

func TestPostgresNotificationLinkDedupLeaseAndTenantIsolation(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	pair := testsupport.TwoTenants(t, ctx, pool)
	riskID := insertNotificationRisk(t, pool, pair.A.TenantID, pair.A.LocationID)
	repository := NewPostgresRepository(pool)
	generator := ids.Generator{}
	now := time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)

	linker := application.NewLinker(repository, generator, func() time.Time { return now })
	issued, err := linker.Issue(ctx, pair.A.TenantID, pair.A.UserID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var plaintextRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM telegram_link_tokens WHERE token_hash = $1`, issued.Plaintext).Scan(&plaintextRows); err != nil {
		t.Fatal(err)
	}
	if plaintextRows != 0 {
		t.Fatal("открытый код привязки сохранён в PostgreSQL")
	}
	if _, err := linker.Redeem(ctx, pair.B.TenantID, issued.Plaintext, "7001", "7001"); !errors.Is(err, application.ErrInvalidLinkToken) {
		t.Fatalf("чужая организация использовала код: %v", err)
	}
	if _, err := linker.Redeem(ctx, pair.A.TenantID, issued.Plaintext, "7001", "7001"); err != nil {
		t.Fatal(err)
	}
	if _, err := linker.Redeem(ctx, pair.A.TenantID, issued.Plaintext, "7001", "7001"); !errors.Is(err, application.ErrInvalidLinkToken) {
		t.Fatalf("код использован повторно: %v", err)
	}
	if link, found, err := repository.Link(ctx, pair.A.TenantID, pair.A.UserID); err != nil || !found || link.TelegramUserID != "7001" {
		t.Fatalf("привязка не читается: found=%v link=%#v err=%v", found, link, err)
	}
	if disabled, err := repository.DisableLink(ctx, pair.A.TenantID, pair.A.UserID, now.Add(time.Second)); err != nil || !disabled {
		t.Fatalf("отключение привязки: disabled=%v err=%v", disabled, err)
	}
	if _, found, err := repository.TelegramDestination(ctx, pair.A.TenantID, pair.A.UserID); err != nil || found {
		t.Fatalf("отключённая привязка активна: found=%v err=%v", found, err)
	}
	now = now.Add(2 * time.Second)
	issuedAgain, err := linker.Issue(ctx, pair.A.TenantID, pair.A.UserID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := linker.Redeem(ctx, pair.A.TenantID, issuedAgain.Plaintext, "7001", "7001"); err != nil {
		t.Fatal(err)
	}
	if _, found, err := repository.TelegramDestination(ctx, pair.B.TenantID, pair.A.UserID); err != nil || found {
		t.Fatalf("утечка привязки между организациями: found=%v err=%v", found, err)
	}

	notificationID := newID(t)
	notification, err := domain.NewNotification(
		notificationID, pair.A.TenantID, pair.A.UserID, riskID, "Критический риск", "Нужно ответить клиенту", now,
	)
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := domain.NewDelivery(newID(t), notification, "7001", domain.ChannelTelegram, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, created, err := repository.Create(ctx, notification, []domain.Delivery{delivery}); err != nil || !created {
		t.Fatalf("создание уведомления: created=%v err=%v", created, err)
	}
	replay, _ := domain.NewNotification(
		newID(t), pair.A.TenantID, pair.A.UserID, riskID, "Другой текст", "Не должен заменить факт", now.Add(time.Second),
	)
	replayDelivery, _ := domain.NewDelivery(newID(t), replay, "7001", domain.ChannelTelegram, now.Add(time.Second))
	stored, created, err := repository.Create(ctx, replay, []domain.Delivery{replayDelivery})
	if err != nil || created || stored.ID != notificationID {
		t.Fatalf("повтор создал уведомление: stored=%s created=%v err=%v", stored.ID, created, err)
	}

	claimed, err := repository.ClaimDue(ctx, "worker-a", now, now.Add(time.Minute), 1, []domain.Channel{domain.ChannelTelegram})
	if err != nil || len(claimed) != 1 || claimed[0].Status != domain.DeliveryProcessing {
		t.Fatalf("аренда доставки: %#v, %v", claimed, err)
	}
	secondClaim, err := repository.ClaimDue(ctx, "worker-b", now, now.Add(time.Minute), 1, []domain.Channel{domain.ChannelTelegram})
	if err != nil || len(secondClaim) != 0 {
		t.Fatalf("двойная аренда: %#v, %v", secondClaim, err)
	}

	userID, callbackRiskID, found, err := repository.CallbackTarget(
		ctx, pair.A.TenantID, notificationID, "7001", "7001",
	)
	if err != nil || !found || userID != pair.A.UserID || callbackRiskID != riskID {
		t.Fatalf("цель команды: user=%s risk=%s found=%v err=%v", userID, callbackRiskID, found, err)
	}
	if _, _, found, err := repository.CallbackTarget(ctx, pair.B.TenantID, notificationID, "7001", "7001"); err != nil || found {
		t.Fatalf("чужая команда нашла цель: found=%v err=%v", found, err)
	}
	command := application.CallbackCommand{
		TenantID: pair.A.TenantID, UserID: pair.A.UserID, NotificationID: notificationID,
		RiskID: riskID, IdempotencyKey: "callback-1", Action: application.CallbackSnooze,
	}
	if created, err := repository.RecordCallback(ctx, command, newID(t), now.Add(time.Second)); err != nil || !created {
		t.Fatalf("фиксация команды: created=%v err=%v", created, err)
	}
	if created, err := repository.RecordCallback(ctx, command, newID(t), now.Add(2*time.Second)); err != nil || created {
		t.Fatalf("повтор команды: created=%v err=%v", created, err)
	}
	var snoozed bool
	if err := pool.QueryRow(ctx, `SELECT snoozed_at IS NOT NULL FROM notifications WHERE id = $1`, notificationID).Scan(&snoozed); err != nil || !snoozed {
		t.Fatalf("уведомление не отложено: snoozed=%v err=%v", snoozed, err)
	}
}

type retryingTransport struct{}

func (retryingTransport) Send(context.Context, string, string, string, string, bool) (string, bool, error) {
	return "", true, errors.New("Telegram недоступен")
}

func TestTelegramFailureCreatesRetryAndNeverChangesRisk(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	pair := testsupport.TwoTenants(t, ctx, pool)
	riskID := insertNotificationRisk(t, pool, pair.A.TenantID, pair.A.LocationID)
	repository := NewPostgresRepository(pool)
	now := time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `
		INSERT INTO telegram_user_links(id, tenant_id, user_id, telegram_user_id, chat_id, linked_at, updated_at)
		VALUES ($1,$2,$3,7001,7001,$4,$4)`, newID(t), pair.A.TenantID, pair.A.UserID, now); err != nil {
		t.Fatal(err)
	}
	service := application.NewService(repository, repository, retryingTransport{}, ids.Generator{}, func() time.Time { return now })
	if _, created, err := service.NotifyRisk(ctx, pair.A.TenantID, pair.A.UserID, riskID, "Критический риск", "Нужно ответить"); err != nil || !created {
		t.Fatalf("уведомление: created=%v err=%v", created, err)
	}
	if processed, err := service.DispatchOne(ctx, "worker", time.Minute); err != nil || !processed {
		t.Fatalf("попытка доставки: processed=%v err=%v", processed, err)
	}
	var riskStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM risk_signals WHERE tenant_id = $1 AND id = $2`, pair.A.TenantID, riskID).Scan(&riskStatus); err != nil {
		t.Fatal(err)
	}
	if riskStatus != "OPEN" {
		t.Fatalf("отказ Telegram изменил риск на %s", riskStatus)
	}
	var notifications, retries, pending int
	if err := pool.QueryRow(ctx, `
		SELECT count(DISTINCT notification.id),
		       count(*) FILTER (WHERE delivery.status = 'RETRY'),
		       count(*) FILTER (WHERE delivery.status = 'PENDING')
		FROM notifications AS notification
		JOIN notification_deliveries AS delivery ON delivery.notification_id = notification.id
		WHERE notification.tenant_id = $1 AND notification.risk_id = $2`, pair.A.TenantID, riskID).Scan(&notifications, &retries, &pending); err != nil {
		t.Fatal(err)
	}
	if notifications != 1 || retries != 1 || pending != 1 {
		t.Fatalf("после отказа: notifications=%d retry=%d pending=%d", notifications, retries, pending)
	}
}

func insertNotificationRisk(t *testing.T, pool *pgxpool.Pool, tenantID, locationID string) string {
	t.Helper()
	ctx := context.Background()
	at := time.Date(2026, 8, 26, 9, 0, 0, 0, time.UTC)
	connectionID, contactID, conversationID := newID(t), newID(t), newID(t)
	messageID, opportunityID, riskID := newID(t), newID(t), newID(t)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO channel_connections(
			id,tenant_id,location_id,provider,name,status,capabilities,verification_secret_hash,created_at,updated_at
		) VALUES ($1,$2,$3,'TEST','Notification test','ACTIVE','["RECEIVE_MESSAGES"]'::jsonb,repeat('0',64),$4,$4)`, []any{connectionID, tenantID, locationID, at}},
		{`INSERT INTO contacts(id,tenant_id,display_name,created_at,updated_at) VALUES ($1,$2,'Клиент',$3,$3)`, []any{contactID, tenantID, at}},
		{`INSERT INTO conversations(
			id,tenant_id,location_id,connection_id,contact_id,external_id,status,first_message_at,last_message_at,last_message_direction,revision,created_at,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,'ACTIVE',$7,$7,'INCOMING',1,$7,$7)`, []any{conversationID, tenantID, locationID, connectionID, contactID, "conversation-" + conversationID, at}},
		{`INSERT INTO messages(
			id,tenant_id,conversation_id,connection_id,external_id,direction,type,text,sent_at,received_at,metadata,created_at
		) VALUES ($1,$2,$3,$4,$5,'INCOMING','TEXT','Нужна услуга',$6,$6,'{}'::jsonb,$6)`, []any{messageID, tenantID, conversationID, connectionID, "message-" + messageID, at}},
		{`INSERT INTO opportunities(
			id,tenant_id,conversation_id,stage,currency,opened_at,created_at,updated_at
		) VALUES ($1,$2,$3,'NEW','RUB',$4,$4,$4)`, []any{opportunityID, tenantID, conversationID, at}},
		{`INSERT INTO risk_signals(
			id,tenant_id,opportunity_id,location_id,type,severity,status,reason_code,reason_text,source,risk_engine_version,trigger_message_id,detected_at,due_at,created_at,updated_at
		) VALUES ($1,$2,$3,$4,'NO_RESPONSE','CRITICAL','OPEN','NO_RESPONSE_THRESHOLD_EXCEEDED','Бизнес не ответил','RULE','no-response.v1',$5,$6,$7,$6,$6)`, []any{riskID, tenantID, opportunityID, locationID, messageID, at.Add(time.Hour), at}},
	}
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return riskID
}

func newID(t *testing.T) string {
	t.Helper()
	id, err := (ids.Generator{}).NewID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
