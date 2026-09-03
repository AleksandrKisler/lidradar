package infrastructure

import (
	"context"
	"testing"
	"time"

	"lidradar/backend/internal/notification/domain"
	"lidradar/backend/internal/testsupport"
)

// Настройки, очередь сводок и получатели живут в PostgreSQL с ограничениями
// LR-BE-RM-015/020: вырожденные тихие часы отвергаются схемой, риск попадает в
// очередь получателя один раз, сводка не принимает команд Telegram.
func TestPostgresNotificationPolicyPreferencesAndDigests(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	pair := testsupport.TwoTenants(t, ctx, pool)
	riskID := insertNotificationRisk(t, pool, pair.A.TenantID, pair.A.LocationID)
	repository := NewPostgresRepository(pool)
	now := time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)

	preference := domain.DefaultPreference(pair.A.TenantID, pair.A.UserID, domain.RiskNoResponse)
	preference.ID, preference.QuietHoursEnabled, preference.CreatedAt, preference.UpdatedAt = newID(t), true, now, now
	saved, err := repository.SavePreference(ctx, preference)
	if err != nil || saved.ID != preference.ID || !saved.QuietHoursEnabled || saved.QuietHoursStart.String() != "22:00" ||
		saved.QuietHoursEnd.String() != "08:00" || saved.DigestTime.String() != "09:00" {
		t.Fatalf("сохранение настройки: %#v, %v", saved, err)
	}
	replaced := preference
	replaced.ID, replaced.MinimumSeverity, replaced.UpdatedAt = newID(t), domain.SeverityHigh, now.Add(time.Second)
	resaved, err := repository.SavePreference(ctx, replaced)
	if err != nil || resaved.ID != preference.ID || resaved.MinimumSeverity != domain.SeverityHigh || !resaved.CreatedAt.Equal(now) {
		t.Fatalf("замена настройки потеряла идентификатор: %#v, %v", resaved, err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO notification_preferences(
			id, tenant_id, user_id, risk_type, minimum_severity, delivery_mode, in_app_enabled, telegram_enabled,
			quiet_hours_enabled, quiet_hours_start, quiet_hours_end, digest_time, created_at, updated_at
		) VALUES ($1, $2, $3, 'FOLLOW_UP_CANDIDATE', 'LOW', 'DIGEST', TRUE, TRUE, TRUE, '22:00', '22:00', '09:00', $4, $4)`,
		newID(t), pair.A.TenantID, pair.A.UserID, now); err == nil {
		t.Fatal("схема приняла вырожденные тихие часы")
	}
	if stored, err := repository.Preferences(ctx, pair.A.TenantID, pair.A.UserID); err != nil || len(stored) != 1 {
		t.Fatalf("список настроек: %#v, %v", stored, err)
	}
	if stored, err := repository.Preferences(ctx, pair.B.TenantID, pair.A.UserID); err != nil || len(stored) != 0 {
		t.Fatalf("настройки утекли в другую организацию: %#v, %v", stored, err)
	}

	policy, err := repository.TenantPolicy(ctx, pair.A.TenantID)
	if err != nil || policy.Timezone != "Europe/Moscow" || len(policy.Recipients) != 1 || policy.Recipients[0].UserID != pair.A.UserID ||
		policy.Recipients[0].TelegramDestination != "" || policy.Recipients[0].Preferences[domain.RiskNoResponse].MinimumSeverity != domain.SeverityHigh {
		t.Fatalf("политика организации: %#v, %v", policy, err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO telegram_user_links(id, tenant_id, user_id, telegram_user_id, chat_id, linked_at, updated_at)
		VALUES ($1,$2,$3,7001,7001,$4,$4)`, newID(t), pair.A.TenantID, pair.A.UserID, now); err != nil {
		t.Fatal(err)
	}
	owner, found, err := repository.OwnerRecipient(ctx, pair.A.TenantID)
	if err != nil || !found || owner.UserID != pair.A.UserID || owner.TelegramDestination != "7001" {
		t.Fatalf("владелец: %#v, found=%v, %v", owner, found, err)
	}
	if other, err := repository.TenantPolicy(ctx, pair.B.TenantID); err != nil || len(other.Recipients) != 1 ||
		other.Recipients[0].UserID != pair.B.UserID || len(other.Recipients[0].Preferences) != 0 {
		t.Fatalf("политика другой организации: %#v, %v", other, err)
	}

	location, _ := time.LoadLocation("Europe/Moscow")
	decision := domain.DefaultPreference(pair.A.TenantID, pair.A.UserID, domain.RiskFollowUpCandidate).
		Decide(domain.SeverityMedium, now, location, true)
	item, err := domain.NewDigestItem(newID(t), pair.A.TenantID, pair.A.UserID, riskID, domain.RiskNoResponse, decision, now)
	if err != nil {
		t.Fatal(err)
	}
	if created, err := repository.EnqueueDigestItem(ctx, item); err != nil || !created {
		t.Fatalf("постановка в сводку: created=%v err=%v", created, err)
	}
	duplicate := item
	duplicate.ID = newID(t)
	if created, err := repository.EnqueueDigestItem(ctx, duplicate); err != nil || created {
		t.Fatalf("риск попал в очередь дважды: created=%v err=%v", created, err)
	}
	entries, err := repository.PendingDigestEntries(ctx, pair.A.TenantID, pair.A.UserID, item.Slot)
	if err != nil || len(entries) != 1 || entries[0].Severity != domain.SeverityCritical || entries[0].Status != "OPEN" ||
		entries[0].Contact != "Клиент" || entries[0].Item.ID != item.ID {
		t.Fatalf("элементы слота: %#v, %v", entries, err)
	}
	title, body := domain.ComposeDigest(entries, location)
	digest, err := domain.NewDigest(newID(t), pair.A.TenantID, pair.A.UserID, item.Slot, title, body, now)
	if err != nil {
		t.Fatal(err)
	}
	inApp, _ := domain.NewDelivery(newID(t), digest, pair.A.UserID, domain.ChannelInApp, now)
	telegram, _ := domain.NewDelivery(newID(t), digest, "7001", domain.ChannelTelegram, now)
	stored, created, err := repository.CreateDigest(ctx, digest, []domain.Delivery{inApp, telegram}, []string{item.ID})
	if err != nil || !created || stored.ID != digest.ID || stored.Kind != domain.KindRiskDigest || stored.RiskID != "" {
		t.Fatalf("создание сводки: %#v, created=%v, %v", stored, created, err)
	}
	replay, _ := domain.NewDigest(newID(t), pair.A.TenantID, pair.A.UserID, item.Slot, title, body, now.Add(time.Second))
	replayDelivery, _ := domain.NewDelivery(newID(t), replay, pair.A.UserID, domain.ChannelInApp, now.Add(time.Second))
	if again, created, err := repository.CreateDigest(ctx, replay, []domain.Delivery{replayDelivery}, []string{item.ID}); err != nil || created || again.ID != digest.ID {
		t.Fatalf("повтор слота создал сводку: %#v, created=%v, %v", again, created, err)
	}
	if pending, err := repository.PendingDigestEntries(ctx, pair.A.TenantID, pair.A.UserID, item.Slot); err != nil || len(pending) != 0 {
		t.Fatalf("элементы не закрыты: %#v, %v", pending, err)
	}
	var consumedBy string
	if err := pool.QueryRow(ctx, `SELECT notification_id::text FROM notification_digest_items WHERE id = $1`, item.ID).Scan(&consumedBy); err != nil || consumedBy != digest.ID {
		t.Fatalf("элемент не связан со сводкой: %q, %v", consumedBy, err)
	}
	if _, _, found, err := repository.CallbackTarget(ctx, pair.A.TenantID, digest.ID, "7001", "7001"); err != nil || found {
		t.Fatalf("сводка приняла команду Telegram: found=%v err=%v", found, err)
	}
	onlyInApp, err := repository.ClaimDue(ctx, "worker-a", now, now.Add(time.Minute), 5, []domain.Channel{domain.ChannelInApp})
	if err != nil || len(onlyInApp) != 1 || onlyInApp[0].Channel != domain.ChannelInApp || onlyInApp[0].Kind != domain.KindRiskDigest {
		t.Fatalf("аренда in-app: %#v, %v", onlyInApp, err)
	}
	onlyTelegram, err := repository.ClaimDue(ctx, "worker-a", now, now.Add(time.Minute), 5, []domain.Channel{domain.ChannelTelegram})
	if err != nil || len(onlyTelegram) != 1 || onlyTelegram[0].Channel != domain.ChannelTelegram || onlyTelegram[0].Destination != "7001" {
		t.Fatalf("аренда Telegram: %#v, %v", onlyTelegram, err)
	}
	if awaiting, err := repository.RiskAwaitingAction(ctx, pair.A.TenantID, riskID); err != nil || !awaiting {
		t.Fatalf("открытый риск не ждёт реакции: %v, %v", awaiting, err)
	}
	if awaiting, err := repository.RiskAwaitingAction(ctx, pair.B.TenantID, riskID); err != nil || awaiting {
		t.Fatalf("чужая организация видит риск: %v, %v", awaiting, err)
	}
	if deleted, err := repository.DeletePreference(ctx, pair.A.TenantID, pair.A.UserID, domain.RiskNoResponse); err != nil || !deleted {
		t.Fatalf("сброс настройки: %v, %v", deleted, err)
	}
	if deleted, err := repository.DeletePreference(ctx, pair.A.TenantID, pair.A.UserID, domain.RiskNoResponse); err != nil || deleted {
		t.Fatalf("повторный сброс: %v, %v", deleted, err)
	}
}
