package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	notificationdomain "lidradar/backend/internal/notification/domain"
	"lidradar/backend/platform/ids"
)

type preferenceItem struct {
	RiskType          string  `json:"riskType"`
	MinimumSeverity   string  `json:"minimumSeverity"`
	DeliveryMode      string  `json:"deliveryMode"`
	InAppEnabled      bool    `json:"inAppEnabled"`
	TelegramEnabled   bool    `json:"telegramEnabled"`
	QuietHoursEnabled bool    `json:"quietHoursEnabled"`
	QuietHoursStart   *string `json:"quietHoursStart"`
	QuietHoursEnd     *string `json:"quietHoursEnd"`
	DigestTime        string  `json:"digestTime"`
	Timezone          string  `json:"timezone"`
	IsDefault         bool    `json:"isDefault"`
}

func listPreferences(t *testing.T, fixture apiFixture, cookie *http.Cookie, tenantID string) []preferenceItem {
	t.Helper()
	response := request(t, fixture.handler, http.MethodGet, "/api/v1/notifications/preferences", "", cookie, tenantID)
	requireStatus(t, response, http.StatusOK)
	var page struct {
		Items []preferenceItem `json:"items"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	return page.Items
}

// TestNotificationPolicySettingsAndQuietHoursFlow доказывает выходной критерий
// этапа 20 на PostgreSQL: владелец управляет оповещениями по типу риска через
// API, а риск, открытый внутри тихих часов, не уходит немедленно, ждёт конца
// тихих часов и приходит одним сообщением в оба канала (LR-BE-RM-020).
func TestNotificationPolicySettingsAndQuietHoursFlow(t *testing.T) {
	fixture := newAPIFixture(t)
	owner := register(t, fixture.handler, "policy-owner@example.com", "Владелец политики")
	tenantID := createOrganization(t, fixture, owner, "Организация политики")
	stranger := register(t, fixture.handler, "policy-stranger@example.com", "Посторонний")

	defaults := listPreferences(t, fixture, owner.Cookie, tenantID)
	if len(defaults) != 5 || defaults[0].RiskType != "NO_RESPONSE" || defaults[0].DeliveryMode != "IMMEDIATE" || !defaults[0].IsDefault ||
		defaults[0].MinimumSeverity != "LOW" || !defaults[0].InAppEnabled || !defaults[0].TelegramEnabled || defaults[0].QuietHoursEnabled ||
		defaults[0].QuietHoursStart == nil || *defaults[0].QuietHoursStart != "22:00" || *defaults[0].QuietHoursEnd != "08:00" ||
		defaults[0].DigestTime != "09:00" || defaults[0].Timezone != "Europe/Moscow" ||
		defaults[3].RiskType != "CUSTOMER_SILENT_AFTER_PRICE" || defaults[3].DeliveryMode != "DIGEST" || defaults[4].DeliveryMode != "DIGEST" {
		t.Fatalf("настройки по умолчанию: %#v", defaults)
	}
	requireStatus(t, request(t, fixture.handler, http.MethodGet, "/api/v1/notifications/preferences", "", stranger.Cookie, tenantID), http.StatusForbidden)
	degenerate := `{"minimumSeverity":"LOW","deliveryMode":"IMMEDIATE","inAppEnabled":true,"telegramEnabled":true,
		"quietHoursEnabled":true,"quietHoursStart":"22:00","quietHoursEnd":"22:00","digestTime":"09:00"}`
	requireStatus(t, request(t, fixture.handler, http.MethodPut, "/api/v1/notifications/preferences/NO_RESPONSE", degenerate, owner.Cookie, tenantID), http.StatusBadRequest)
	valid := `{"minimumSeverity":"HIGH","deliveryMode":"DIGEST","inAppEnabled":true,"telegramEnabled":false,
		"quietHoursEnabled":false,"quietHoursStart":null,"quietHoursEnd":null,"digestTime":"18:30"}`
	requireStatus(t, request(t, fixture.handler, http.MethodPut, "/api/v1/notifications/preferences/UNKNOWN", valid, owner.Cookie, tenantID), http.StatusBadRequest)
	requireStatus(t, request(t, fixture.handler, http.MethodPut, "/api/v1/notifications/preferences/NO_RESPONSE", `{"deliveryMode":"DIGEST"}`, owner.Cookie, tenantID), http.StatusBadRequest)
	requireStatus(t, request(t, fixture.handler, http.MethodPut, "/api/v1/notifications/preferences/NO_RESPONSE", valid, stranger.Cookie, tenantID), http.StatusForbidden)
	saved := request(t, fixture.handler, http.MethodPut, "/api/v1/notifications/preferences/CUSTOMER_SILENT_AFTER_PRICE", valid, owner.Cookie, tenantID)
	requireStatus(t, saved, http.StatusOK)
	var custom preferenceItem
	if err := json.Unmarshal(saved.Body.Bytes(), &custom); err != nil {
		t.Fatal(err)
	}
	if custom.IsDefault || custom.MinimumSeverity != "HIGH" || custom.DigestTime != "18:30" || custom.QuietHoursStart != nil || custom.TelegramEnabled {
		t.Fatalf("сохранённая настройка: %#v", custom)
	}

	// Тихие часы вокруг текущего момента в часовом поясе организации.
	location, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Fatal(err)
	}
	localNow := time.Now().In(location)
	start, end := localNow.Add(-time.Hour).Format("15:04"), localNow.Add(time.Hour).Format("15:04")
	quiet := `{"minimumSeverity":"LOW","deliveryMode":"IMMEDIATE","inAppEnabled":true,"telegramEnabled":true,
		"quietHoursEnabled":true,"quietHoursStart":"` + start + `","quietHoursEnd":"` + end + `","digestTime":"09:00"}`
	requireStatus(t, request(t, fixture.handler, http.MethodPut, "/api/v1/notifications/preferences/NO_RESPONSE", quiet, owner.Cookie, tenantID), http.StatusOK)
	linkID, err := (ids.Generator{}).NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(context.Background(), `
		INSERT INTO telegram_user_links(id, tenant_id, user_id, telegram_user_id, chat_id, linked_at, updated_at)
		VALUES ($1,$2,$3,7001,7001,now(),now())`, linkID, tenantID, owner.ID); err != nil {
		t.Fatal(err)
	}

	locationResponse := request(t, fixture.handler, http.MethodPost, "/api/v1/locations", `{
		"name":"Точка политики","timezone":"UTC","responseThresholdMinutes":45
	}`, owner.Cookie, tenantID)
	requireStatus(t, locationResponse, http.StatusCreated)
	locationID := jsonID(t, locationResponse)
	requireStatus(t, request(t, fixture.handler, http.MethodPut, "/api/v1/locations/"+locationID+"/business-hours", `{
		"timezone":"UTC","days":[
			{"weekday":1,"closed":false,"opensAt":"00:00","closesAt":"23:59"},
			{"weekday":2,"closed":false,"opensAt":"00:00","closesAt":"23:59"},
			{"weekday":3,"closed":false,"opensAt":"00:00","closesAt":"23:59"},
			{"weekday":4,"closed":false,"opensAt":"00:00","closesAt":"23:59"},
			{"weekday":5,"closed":false,"opensAt":"00:00","closesAt":"23:59"},
			{"weekday":6,"closed":false,"opensAt":"00:00","closesAt":"23:59"},
			{"weekday":7,"closed":false,"opensAt":"00:00","closesAt":"23:59"}
		]
	}`, owner.Cookie, tenantID), http.StatusOK)
	requireStatus(t, request(t, fixture.handler, http.MethodPost, "/api/v1/services", `{
		"name":"Полировка","locationId":"`+locationID+`","priceFrom":"5000","priceTo":"5000"
	}`, owner.Cookie, tenantID), http.StatusCreated)
	secret := "policy-flow-secret-123"
	connected := request(t, fixture.handler, http.MethodPost, "/api/v1/integrations/GENERIC_WEBHOOK/connect", `{
		"name":"Канал политики","locationId":"`+locationID+`","webhookSecret":"`+secret+`"
	}`, owner.Cookie, tenantID)
	requireStatus(t, connected, http.StatusCreated)
	path := "/api/v1/webhooks/GENERIC_WEBHOOK/" + tenantID + "/" + jsonID(t, connected)
	incomingAt := time.Now().UTC().Add(-60 * time.Minute).Format(time.RFC3339Nano)
	incoming := canonicalWebhook(
		"policy-event-incoming", "message.received.v1", "policy-dialog", "policy-message-incoming", "policy-contact",
		"INCOMING", "TEXT", "Нужна полировка", incomingAt, "",
	)
	requireStatus(t, webhookRequest(t, fixture.handler, path, incoming, "X-LidRadar-Webhook-Secret", secret), http.StatusAccepted)
	processExactly(t, fixture, 3)
	if promoted, err := fixture.scheduler.RunOnce(context.Background(), 100); err != nil || promoted < 1 {
		t.Fatalf("перенос проверки R1 = %d, %v", promoted, err)
	}
	processExactly(t, fixture, 1)

	var riskID string
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT id::text FROM risk_signals WHERE tenant_id = $1 AND type = 'NO_RESPONSE'`, tenantID).Scan(&riskID); err != nil {
		t.Fatal(err)
	}
	var immediate int
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM notifications WHERE tenant_id = $1 AND risk_id = $2`, tenantID, riskID).Scan(&immediate); err != nil {
		t.Fatal(err)
	}
	if immediate != 0 {
		t.Fatalf("риск внутри тихих часов доставлен немедленно: %d", immediate)
	}
	preference := notificationdomain.DefaultPreference(tenantID, owner.ID, notificationdomain.RiskNoResponse)
	quietStart, _ := notificationdomain.ParseClockTime(start)
	quietEnd, _ := notificationdomain.ParseClockTime(end)
	preference.QuietHoursEnabled, preference.QuietHoursStart, preference.QuietHoursEnd = true, &quietStart, &quietEnd
	expected := preference.Decide(notificationdomain.SeverityHigh, time.Now(), location, true)
	var reason, slot string
	var inApp, telegram bool
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT reason, slot, in_app_enabled, telegram_enabled FROM notification_digest_items
		WHERE tenant_id = $1 AND user_id = $2 AND risk_id = $3`, tenantID, owner.ID, riskID).Scan(&reason, &slot, &inApp, &telegram); err != nil {
		t.Fatal(err)
	}
	if reason != "QUIET_HOURS" || slot != expected.Slot || !inApp || !telegram {
		t.Fatalf("элемент тихих часов: reason=%s slot=%s (ожидался %s) inApp=%v telegram=%v", reason, slot, expected.Slot, inApp, telegram)
	}
	var checks int
	var dueAt time.Time
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT count(*), min(due_at) FROM scheduled_checks
		WHERE tenant_id = $1 AND check_type = 'NOTIFICATION_DIGEST_DUE' AND dedup_key = $2 AND status = 'SCHEDULED'`,
		tenantID, "digest:user:"+owner.ID+":"+slot).Scan(&checks, &dueAt); err != nil {
		t.Fatal(err)
	}
	if checks != 1 || !dueAt.Equal(expected.DeliverAt) {
		t.Fatalf("проверка слота: count=%d due=%s ожидалось %s", checks, dueAt, expected.DeliverAt)
	}

	// Срок слота наступит через час; доставку сводки выполняем напрямую, как
	// это сделает задание планировщика.
	for round := 0; round < 2; round++ {
		if err := fixture.notifications.DeliverDigest(context.Background(), tenantID, owner.ID, slot); err != nil {
			t.Fatalf("сводка, раунд %d: %v", round, err)
		}
	}
	var digests int
	var title, body string
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT count(*), min(title), min(body) FROM notifications
		WHERE tenant_id = $1 AND user_id = $2 AND kind = 'RISK_DIGEST'`, tenantID, owner.ID).Scan(&digests, &title, &body); err != nil {
		t.Fatal(err)
	}
	if digests != 1 || title != "Риски за тихие часы: 1" || !strings.Contains(body, "Высокий · клиент ждёт ответа") {
		t.Fatalf("сводка: count=%d title=%q body=%q", digests, title, body)
	}
	for round := 0; round < 2; round++ {
		if delivered, err := fixture.notifications.DispatchOne(context.Background(), "policy-notifications", time.Minute); err != nil || !delivered {
			t.Fatalf("доставка %d = %v, %v", round, delivered, err)
		}
	}
	var inAppDone, telegramDone, pending int
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT count(*) FILTER (WHERE delivery.channel = 'IN_APP' AND delivery.status = 'SUCCEEDED' AND delivery.provider_message_id LIKE 'in-app:%'),
		       count(*) FILTER (WHERE delivery.channel = 'TELEGRAM' AND delivery.status = 'SUCCEEDED'),
		       (SELECT count(*) FROM notification_digest_items WHERE tenant_id = $1 AND consumed_at IS NULL)
		FROM notification_deliveries AS delivery
		JOIN notifications AS notification ON notification.tenant_id = delivery.tenant_id AND notification.id = delivery.notification_id
		WHERE notification.tenant_id = $1 AND notification.kind = 'RISK_DIGEST'`, tenantID).Scan(&inAppDone, &telegramDone, &pending); err != nil {
		t.Fatal(err)
	}
	if inAppDone != 1 || telegramDone != 1 || pending != 0 {
		t.Fatalf("доставки сводки: inApp=%d telegram=%d ожидающих=%d", inAppDone, telegramDone, pending)
	}

	requireStatus(t, request(t, fixture.handler, http.MethodDelete, "/api/v1/notifications/preferences/NO_RESPONSE", "", owner.Cookie, tenantID), http.StatusNoContent)
	if reset := listPreferences(t, fixture, owner.Cookie, tenantID); !reset[0].IsDefault || reset[0].QuietHoursEnabled || reset[3].IsDefault {
		t.Fatalf("сброс настройки: %#v", reset)
	}
}
