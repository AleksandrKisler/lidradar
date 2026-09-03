package application_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	eventsdomain "lidradar/backend/internal/events/domain"
	jobsdomain "lidradar/backend/internal/jobs/domain"
	"lidradar/backend/internal/notification/application"
	"lidradar/backend/internal/notification/domain"
)

// alertStore — память логических уведомлений и попыток с той же дедупликацией,
// что и PostgreSQL: один tenant_id + dedup_key.
type alertStore struct {
	notifications []domain.Notification
	deliveries    []domain.Delivery
}

func (store *alertStore) Create(_ context.Context, notification domain.Notification, deliveries []domain.Delivery) (domain.Notification, bool, error) {
	for _, existing := range store.notifications {
		if existing.TenantID == notification.TenantID && existing.DedupKey == notification.DedupKey {
			return existing, false, nil
		}
	}
	store.notifications = append(store.notifications, notification)
	store.deliveries = append(store.deliveries, deliveries...)
	return notification, true, nil
}

func (store *alertStore) ClaimDue(_ context.Context, owner string, at, leaseUntil time.Time, limit int, channels []domain.Channel) ([]domain.Delivery, error) {
	var claimed []domain.Delivery
	for index, delivery := range store.deliveries {
		allowed := false
		for _, channel := range channels {
			allowed = allowed || channel == delivery.Channel
		}
		if !allowed || delivery.Status != domain.DeliveryPending || delivery.AvailableAt.After(at) || len(claimed) >= limit {
			continue
		}
		delivery.Status = domain.DeliveryProcessing
		delivery.LeasedBy, delivery.LeaseUntil, delivery.UpdatedAt = &owner, &leaseUntil, at
		store.deliveries[index] = delivery
		claimed = append(claimed, delivery)
	}
	return claimed, nil
}

func (store *alertStore) Complete(_ context.Context, _ string, delivery domain.Delivery, retry *domain.Delivery) error {
	for index := range store.deliveries {
		if store.deliveries[index].ID == delivery.ID {
			store.deliveries[index] = delivery
		}
	}
	if retry != nil {
		store.deliveries = append(store.deliveries, *retry)
	}
	return nil
}

func (store *alertStore) byKind(kind domain.Kind) []domain.Notification {
	var matched []domain.Notification
	for _, notification := range store.notifications {
		if notification.Kind == kind {
			matched = append(matched, notification)
		}
	}
	return matched
}

type riskState struct {
	severity domain.Severity
	status   string
}

// policyStore — память настроек, очереди сводок и состояния рисков.
type policyStore struct {
	alerts   *alertStore
	policy   application.TenantPolicy
	owner    *application.Recipient
	items    []domain.DigestItem
	risks    map[string]riskState
	awaiting map[string]bool
}

func (store *policyStore) TenantPolicy(context.Context, string) (application.TenantPolicy, error) {
	return store.policy, nil
}

func (store *policyStore) OwnerRecipient(context.Context, string) (application.Recipient, bool, error) {
	if store.owner == nil {
		return application.Recipient{}, false, nil
	}
	return *store.owner, true, nil
}

func (store *policyStore) Timezone(context.Context, string) (string, bool, error) {
	return store.policy.Timezone, true, nil
}

func (store *policyStore) EnqueueDigestItem(_ context.Context, item domain.DigestItem) (bool, error) {
	for _, existing := range store.items {
		if existing.UserID == item.UserID && existing.RiskID == item.RiskID {
			return false, nil
		}
	}
	store.items = append(store.items, item)
	return true, nil
}

func (store *policyStore) PendingDigestEntries(_ context.Context, _, userID, slot string) ([]domain.DigestEntry, error) {
	var entries []domain.DigestEntry
	for _, item := range store.items {
		if item.UserID != userID || item.Slot != slot || item.ConsumedAt != nil {
			continue
		}
		state, known := store.risks[item.RiskID]
		if !known {
			state = riskState{severity: domain.SeverityHigh, status: "OPEN"}
		}
		entries = append(entries, domain.DigestEntry{
			Item: item, Severity: state.severity, Status: state.status, Contact: "Иван", DetectedAt: item.CreatedAt,
		})
	}
	return entries, nil
}

func (store *policyStore) CreateDigest(ctx context.Context, notification domain.Notification, deliveries []domain.Delivery, itemIDs []string) (domain.Notification, bool, error) {
	stored, created, err := store.alerts.Create(ctx, notification, deliveries)
	if err != nil {
		return domain.Notification{}, false, err
	}
	store.consume(itemIDs, notification.CreatedAt, stored.ID)
	return stored, created, nil
}

func (store *policyStore) ConsumeDigestItems(_ context.Context, _ string, itemIDs []string, at time.Time) error {
	store.consume(itemIDs, at, "")
	return nil
}

func (store *policyStore) consume(itemIDs []string, at time.Time, notificationID string) {
	for _, id := range itemIDs {
		for index := range store.items {
			if store.items[index].ID == id && store.items[index].ConsumedAt == nil {
				consumedAt := at
				store.items[index].ConsumedAt = &consumedAt
				store.items[index].NotificationID = notificationID
			}
		}
	}
}

func (store *policyStore) RiskAwaitingAction(_ context.Context, _, riskID string) (bool, error) {
	return store.awaiting[riskID], nil
}

func (store *policyStore) pending() []domain.DigestItem {
	var pending []domain.DigestItem
	for _, item := range store.items {
		if item.ConsumedAt == nil {
			pending = append(pending, item)
		}
	}
	return pending
}

type checkScheduler struct{ checks []jobsdomain.ScheduledCheck }

func (scheduler *checkScheduler) Schedule(_ context.Context, check jobsdomain.ScheduledCheck) (jobsdomain.ScheduledCheck, bool, error) {
	for _, existing := range scheduler.checks {
		if existing.Type == check.Type && existing.DedupKey == check.DedupKey {
			return existing, false, nil
		}
	}
	scheduler.checks = append(scheduler.checks, check)
	return check, true, nil
}

type recordingTransport struct {
	actions []bool
}

func (transport *recordingTransport) Send(_ context.Context, _, _, _, _ string, actions bool) (string, bool, error) {
	transport.actions = append(transport.actions, actions)
	return "provider-1", false, nil
}

type sequence struct{ n int }

func (s *sequence) NewID() (string, error) { s.n++; return "id-" + strings.Repeat("x", s.n), nil }

func moscowTime(t *testing.T, value string) time.Time {
	t.Helper()
	location, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := time.ParseInLocation("2006-01-02 15:04", value, location)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func quietPreference(t *testing.T, tenantID, userID string, riskType domain.RiskType) domain.Preference {
	t.Helper()
	preference := domain.DefaultPreference(tenantID, userID, riskType)
	preference.QuietHoursEnabled = true
	return preference
}

func newPolicyFixture(now time.Time, policy application.TenantPolicy) (application.Service, *alertStore, *policyStore, *checkScheduler, *recordingTransport) {
	alerts := new(alertStore)
	store := &policyStore{alerts: alerts, policy: policy, risks: map[string]riskState{}, awaiting: map[string]bool{}}
	scheduler := new(checkScheduler)
	transport := new(recordingTransport)
	service := application.NewService(alerts, links{"chat-owner"}, transport, new(sequence), func() time.Time { return now }).
		WithPolicy(store, scheduler)
	return service, alerts, store, scheduler, transport
}

// Владелец без настроек получает немедленное уведомление в оба канала, менеджер
// со сводкой — элемент слота, участник с DISABLED — ничего. Повтор события
// не создаёт ни второго уведомления, ни второго элемента (ТЗ §47).
func TestApplyRiskOpenedFollowsEachRecipientPreference(t *testing.T) {
	now := moscowTime(t, "2026-09-02 12:00")
	manager := domain.DefaultPreference("tenant", "manager", domain.RiskNoResponse)
	manager.DeliveryMode = domain.ModeDigest
	manager.DigestTime = domain.ClockTime{Hour: 18}
	silent := domain.DefaultPreference("tenant", "silent", domain.RiskNoResponse)
	silent.DeliveryMode = domain.ModeDisabled
	service, alerts, store, scheduler, _ := newPolicyFixture(now, application.TenantPolicy{
		Timezone: "Europe/Moscow",
		Recipients: []application.Recipient{
			{UserID: "owner", TelegramDestination: "chat-owner"},
			{UserID: "manager", Preferences: map[domain.RiskType]domain.Preference{domain.RiskNoResponse: manager}},
			{UserID: "silent", Preferences: map[domain.RiskType]domain.Preference{domain.RiskNoResponse: silent}},
		},
	})
	risk := application.RiskOpened{RiskID: "risk-1", Type: domain.RiskNoResponse, Severity: domain.SeverityHigh}
	for round := 0; round < 2; round++ {
		if err := service.ApplyRiskOpened(context.Background(), "tenant", risk); err != nil {
			t.Fatalf("раунд %d: %v", round, err)
		}
	}
	if len(alerts.notifications) != 1 || alerts.notifications[0].UserID != "owner" ||
		alerts.notifications[0].DedupKey != "risk:risk-1:opened:user:owner" || alerts.notifications[0].Kind != domain.KindRiskOpened ||
		len(alerts.deliveries) != 2 || alerts.deliveries[0].Channel != domain.ChannelInApp || alerts.deliveries[0].Destination != "owner" ||
		alerts.deliveries[1].Channel != domain.ChannelTelegram || alerts.deliveries[1].Destination != "chat-owner" {
		t.Fatalf("немедленные уведомления: %#v, доставки %#v", alerts.notifications, alerts.deliveries)
	}
	if len(store.items) != 1 || store.items[0].UserID != "manager" || store.items[0].Slot != "2026-09-02T18:00" ||
		store.items[0].Reason != domain.DeferDigest || !store.items[0].InApp || store.items[0].Telegram {
		t.Fatalf("элементы сводки: %#v", store.items)
	}
	if len(scheduler.checks) != 1 || scheduler.checks[0].Type != application.DigestCheckType ||
		scheduler.checks[0].JobType != application.DigestJobType || scheduler.checks[0].DedupKey != "digest:user:manager:2026-09-02T18:00" ||
		!scheduler.checks[0].DueAt.Equal(moscowTime(t, "2026-09-02 18:00")) || scheduler.checks[0].SubjectID != "manager" {
		t.Fatalf("проверки по расписанию: %#v", scheduler.checks)
	}
}

// LR-BE-RM-020: риск в 23:00 при тихих часах 22:00–08:00 ждёт 08:00 и приходит
// одним сообщением с актуальным состоянием; закрытый к тому времени риск в
// сводку не попадает, повтор слота не создаёт второго сообщения.
func TestQuietHoursDeferToOneDigestWithCurrentRiskState(t *testing.T) {
	now := moscowTime(t, "2026-09-02 23:00")
	owner := quietPreference(t, "tenant", "owner", domain.RiskNoResponse)
	booking := quietPreference(t, "tenant", "owner", domain.RiskBookingNotConfirmed)
	service, alerts, store, scheduler, transport := newPolicyFixture(now, application.TenantPolicy{
		Timezone: "Europe/Moscow",
		Recipients: []application.Recipient{{
			UserID: "owner", TelegramDestination: "chat-owner",
			Preferences: map[domain.RiskType]domain.Preference{domain.RiskNoResponse: owner, domain.RiskBookingNotConfirmed: booking},
		}},
	})
	if err := service.ApplyRiskOpened(context.Background(), "tenant", application.RiskOpened{
		RiskID: "risk-1", Type: domain.RiskNoResponse, Severity: domain.SeverityHigh,
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.ApplyRiskOpened(context.Background(), "tenant", application.RiskOpened{
		RiskID: "risk-2", Type: domain.RiskBookingNotConfirmed, Severity: domain.SeverityCritical,
	}); err != nil {
		t.Fatal(err)
	}
	if len(alerts.notifications) != 0 || len(store.items) != 2 || store.items[0].Slot != "2026-09-03T08:00" ||
		store.items[0].Reason != domain.DeferQuietHours || len(scheduler.checks) != 1 ||
		!scheduler.checks[0].DueAt.Equal(moscowTime(t, "2026-09-03 08:00")) {
		t.Fatalf("тихие часы: уведомления %d, элементы %#v, проверки %#v", len(alerts.notifications), store.items, scheduler.checks)
	}
	store.risks["risk-1"] = riskState{severity: domain.SeverityHigh, status: "RESOLVED"}
	store.risks["risk-2"] = riskState{severity: domain.SeverityCritical, status: "OPEN"}
	for round := 0; round < 2; round++ {
		if err := service.DeliverDigest(context.Background(), "tenant", "owner", "2026-09-03T08:00"); err != nil {
			t.Fatalf("сводка, раунд %d: %v", round, err)
		}
	}
	digests := alerts.byKind(domain.KindRiskDigest)
	if len(digests) != 1 || digests[0].Title != "Риски за тихие часы: 1" || digests[0].DedupKey != "digest:user:owner:2026-09-03T08:00" ||
		!strings.Contains(digests[0].Body, "1. Критический · запись не подтверждена: Иван") || strings.Contains(digests[0].Body, "клиент ждёт ответа") {
		t.Fatalf("сводка: %#v", digests)
	}
	if len(alerts.deliveries) != 2 || len(store.pending()) != 0 || store.items[0].NotificationID != digests[0].ID {
		t.Fatalf("доставки %#v, элементы %#v", alerts.deliveries, store.items)
	}
	for round := 0; round < 2; round++ {
		if delivered, err := service.DispatchOne(context.Background(), "worker", time.Minute); err != nil || !delivered {
			t.Fatalf("доставка %d = %v, %v", round, delivered, err)
		}
	}
	if alerts.deliveries[0].Status != domain.DeliverySucceeded || alerts.deliveries[0].ProviderMessageID != "in-app:"+digests[0].ID ||
		alerts.deliveries[1].Status != domain.DeliverySucceeded || len(transport.actions) != 1 || transport.actions[0] {
		t.Fatalf("сводка доставлена неверно: %#v, кнопки %v", alerts.deliveries, transport.actions)
	}
}

func TestDeliverDigestWithoutActiveRisksStaysSilent(t *testing.T) {
	now := moscowTime(t, "2026-09-02 12:00")
	service, alerts, store, _, _ := newPolicyFixture(now, application.TenantPolicy{
		Timezone: "Europe/Moscow", Recipients: []application.Recipient{{UserID: "owner"}},
	})
	if err := service.ApplyRiskOpened(context.Background(), "tenant", application.RiskOpened{
		RiskID: "risk-1", Type: domain.RiskFollowUpCandidate, Severity: domain.SeverityMedium,
	}); err != nil {
		t.Fatal(err)
	}
	store.risks["risk-1"] = riskState{severity: domain.SeverityMedium, status: "RESOLVED"}
	if err := service.DeliverDigest(context.Background(), "tenant", "owner", "2026-09-03T09:00"); err != nil {
		t.Fatal(err)
	}
	if len(alerts.notifications) != 0 || len(store.pending()) != 0 {
		t.Fatalf("пустая сводка: уведомления %d, ожидающие %d", len(alerts.notifications), len(store.pending()))
	}
}

// LR-BE-2010: без флага эскалации нет; с флагом риск HIGH получает проверку, а
// владелец — отдельное уведомление, пока риск никем не принят.
func TestEscalationFoundationIsFlagGated(t *testing.T) {
	now := moscowTime(t, "2026-09-02 12:00")
	policy := application.TenantPolicy{Timezone: "Europe/Moscow", Recipients: []application.Recipient{{UserID: "owner", TelegramDestination: "chat-owner"}}}
	risk := application.RiskOpened{RiskID: "risk-1", Type: domain.RiskNoResponse, Severity: domain.SeverityHigh}
	disabled, _, _, scheduler, _ := newPolicyFixture(now, policy)
	if err := disabled.ApplyRiskOpened(context.Background(), "tenant", risk); err != nil || len(scheduler.checks) != 0 {
		t.Fatalf("эскалация без флага: %#v, %v", scheduler.checks, err)
	}
	service, alerts, store, scheduler, _ := newPolicyFixture(now, policy)
	service = service.WithEscalation(domain.EscalationPolicy{Enabled: true, After: 30 * time.Minute})
	store.owner = &application.Recipient{UserID: "owner", TelegramDestination: "chat-owner"}
	if err := service.ApplyRiskOpened(context.Background(), "tenant", risk); err != nil {
		t.Fatal(err)
	}
	if len(scheduler.checks) != 1 || scheduler.checks[0].Type != application.EscalationCheckType ||
		scheduler.checks[0].DedupKey != "risk:risk-1:escalation" || !scheduler.checks[0].DueAt.Equal(now.Add(30*time.Minute)) {
		t.Fatalf("проверка эскалации: %#v", scheduler.checks)
	}
	if err := service.Escalate(context.Background(), "tenant", "risk-1"); err != nil || len(alerts.byKind(domain.KindRiskEscalated)) != 0 {
		t.Fatalf("принятый риск эскалирован: %#v, %v", alerts.notifications, err)
	}
	store.awaiting["risk-1"] = true
	if err := service.Escalate(context.Background(), "tenant", "risk-1"); err != nil {
		t.Fatal(err)
	}
	escalated := alerts.byKind(domain.KindRiskEscalated)
	if len(escalated) != 1 || escalated[0].DedupKey != "risk:risk-1:escalated:user:owner" || !strings.Contains(escalated[0].Body, "30 минут") ||
		len(alerts.deliveries) != 4 {
		t.Fatalf("эскалация: %#v, доставки %d", escalated, len(alerts.deliveries))
	}
}

func TestDispatchRequiresTransportForTelegramChannel(t *testing.T) {
	now := moscowTime(t, "2026-09-02 12:00")
	service := application.NewService(new(alertStore), links{"chat"}, nil, new(sequence), func() time.Time { return now })
	if _, err := service.DispatchOne(context.Background(), "worker", time.Minute, domain.ChannelTelegram); err == nil {
		t.Fatal("Telegram-канал без транспорта принят")
	}
	if delivered, err := service.DispatchOne(context.Background(), "worker", time.Minute, domain.ChannelInApp); err != nil || delivered {
		t.Fatalf("пустая in-app очередь: %v, %v", delivered, err)
	}
}

func TestRiskOpenedEventHandlerRejectsMalformedEvents(t *testing.T) {
	now := moscowTime(t, "2026-09-02 12:00")
	service, alerts, _, _, _ := newPolicyFixture(now, application.TenantPolicy{
		Timezone: "Europe/Moscow", Recipients: []application.Recipient{{UserID: "owner"}},
	})
	handler := application.RiskOpenedEventHandler(service)
	malformed := eventsdomain.Event{TenantID: "tenant", AggregateType: "risk", AggregateID: "risk-1", Data: json.RawMessage(`{"riskId":"other"}`)}
	if retryable, code := jobsdomain.Classify(handler(context.Background(), malformed)); retryable || code != "INVALID_RISK_OPENED_EVENT" {
		t.Fatalf("некорректное событие: retryable=%v code=%s", retryable, code)
	}
	valid := eventsdomain.Event{TenantID: "tenant", AggregateType: "risk", AggregateID: "risk-1", Data: json.RawMessage(
		`{"riskId":"risk-1","opportunityId":"opp","locationId":"loc","type":"NO_RESPONSE","severity":"HIGH"}`,
	)}
	if err := handler(context.Background(), valid); err != nil || len(alerts.notifications) != 1 {
		t.Fatalf("корректное событие: %v, уведомлений %d", err, len(alerts.notifications))
	}
	digestJob := application.DigestJobHandler(service)
	if retryable, code := jobsdomain.Classify(digestJob(context.Background(), jobsdomain.Job{TenantID: "tenant", Payload: json.RawMessage(`{"userId":"owner"}`)})); retryable || code != "INVALID_JOB_PAYLOAD" {
		t.Fatalf("задание сводки без слота: retryable=%v code=%s", retryable, code)
	}
}
