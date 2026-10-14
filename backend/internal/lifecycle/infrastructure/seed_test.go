package infrastructure_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/platform/ids"
	"lidradar/backend/platform/tenantctx"
)

// Сид-данные тестов lifecycle: организация с владельцем и менеджером и по строке во всех таблицах, которые
// выгружаются или трогаются обезличиванием. Секреты и служебные значения помечены приманками (sentinel): тест
// выгрузки проверяет, что приманок в архиве нет, а тест обезличивания — что они исчезли или остались по реестру.

var seedAt = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

// org — координаты засеянной организации.
type org struct {
	Label                                  string
	Name                                   string
	Tenant                                 string
	Owner, Manager                         string
	OwnerEmail, ManagerEmail               string
	OwnerMembership, ManagerMembership     string
	Location, Service, Channel, Contact    string
	Conversation, Incoming, Outgoing       string
	Opportunity, Risk, Action, Outcome     string
	RevenueEvent, Notification, Invitation string
	// Secrets — значения, которых не должно быть ни в выгрузке организации, ни в личной выгрузке.
	Secrets []string
	// Marker — текст, который обязан попасть в выгрузку организации.
	Marker string
}

func newID(t testing.TB) string {
	t.Helper()
	id, err := ids.Generator{}.NewID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// must выполняет оператор и прекращает тест при любой ошибке, называя оператор.
func must(t testing.TB, pool *pgxpool.Pool, statement string, arguments ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), statement, arguments...); err != nil {
		first, _, _ := strings.Cut(strings.TrimSpace(statement), "(")
		t.Fatalf("seed %q: %v", strings.Join(strings.Fields(first), " "), err)
	}
}

// seedUser создаёт активного пользователя с приманкой в хэше пароля.
func seedUser(t testing.TB, pool *pgxpool.Pool, email, name string) string {
	t.Helper()
	id := newID(t)
	must(t, pool, `
		INSERT INTO users(id, email, password_hash, display_name, status, created_at, updated_at, email_verified_at)
		VALUES ($1, $2, $3, $4, 'ACTIVE', $5, $5, $5)`,
		id, email, "$argon2id$SENTINEL-PASSWORD-HASH-"+digest(email)[:12], name, seedAt)
	return id
}

// seedOrganization засевает организацию полностью. Менеджер — обычный участник без блокировок закрытия.
func seedOrganization(t testing.TB, pool *pgxpool.Pool, label string) org {
	t.Helper()
	o := org{Label: label, Name: "Салон " + label, Marker: "МАРКЕР-" + strings.ToUpper(label)}
	o.Tenant, o.OwnerMembership, o.ManagerMembership = newID(t), newID(t), newID(t)
	o.OwnerEmail, o.ManagerEmail = "owner-"+label+"@example.test", "manager-"+label+"@example.test"
	o.Owner = seedUser(t, pool, o.OwnerEmail, "Владелец "+label)
	o.Manager = seedUser(t, pool, o.ManagerEmail, "Менеджер "+label)
	o.Location, o.Service, o.Channel, o.Contact = newID(t), newID(t), newID(t), newID(t)
	o.Conversation, o.Incoming, o.Outgoing = newID(t), newID(t), newID(t)
	o.Opportunity, o.Risk, o.Action, o.Outcome = newID(t), newID(t), newID(t), newID(t)
	o.RevenueEvent, o.Notification, o.Invitation = newID(t), newID(t), newID(t)
	credentials := "SENTINEL-CREDENTIALS-" + label
	objectKey := "SENTINEL-OBJECT-KEY-" + label
	providerFile := "SENTINEL-PROVIDER-FILE-" + label
	inviteHash := digest("SENTINEL-INVITE-CODE-" + label)
	verification := digest("SENTINEL-VERIFICATION-" + label)
	normalized := "sentinel-normalized-" + label
	o.Secrets = []string{
		"SENTINEL-PASSWORD-HASH", credentials, objectKey, providerFile, inviteHash, verification, normalized,
		"SENTINEL-TOKEN-HASH", "SENTINEL-MODEL",
	}

	must(t, pool, `INSERT INTO organizations(id, name, default_timezone, default_currency, status, created_at, updated_at)
		VALUES ($1, $2, 'Europe/Moscow', 'RUB', 'ACTIVE', $3, $3)`, o.Tenant, o.Name, seedAt)
	must(t, pool, `INSERT INTO memberships(id, tenant_id, user_id, role, status, created_at, updated_at) VALUES
		($1, $3, $4, 'OWNER', 'ACTIVE', $6, $6), ($2, $3, $5, 'MANAGER', 'ACTIVE', $6, $6)`,
		o.OwnerMembership, o.ManagerMembership, o.Tenant, o.Owner, o.Manager, seedAt)
	must(t, pool, `INSERT INTO locations(id, tenant_id, name, timezone, response_threshold_minutes, active, created_at, updated_at, agreement_threshold_minutes)
		VALUES ($1, $2, $3, 'Europe/Moscow', 45, TRUE, $4, $4, 120)`, o.Location, o.Tenant, "Точка "+label, seedAt)
	must(t, pool, `INSERT INTO location_business_hours(id, tenant_id, location_id, weekday, is_closed, opens_at, closes_at, created_at, updated_at)
		VALUES ($1, $2, $3, 1, FALSE, '09:00', '21:00', $4, $4)`, newID(t), o.Tenant, o.Location, seedAt)
	must(t, pool, `INSERT INTO service_catalog_items(id, tenant_id, location_id, name, normalized_name, price_from, price_to, currency, active, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, 5000, 7000, 'RUB', TRUE, $6, $6)`, o.Service, o.Tenant, o.Location, "Полировка "+label, normalized, seedAt)
	must(t, pool, `INSERT INTO channel_connections(id, tenant_id, location_id, provider, name, status, capabilities, encrypted_credentials, verification_secret_hash, last_event_at, last_error_code, created_at, updated_at)
		VALUES ($1, $2, $3, 'TEST', $4, 'ACTIVE', '["CAN_RECEIVE_MESSAGES"]'::jsonb, $5, $6, $7, 'PROVIDER_TIMEOUT', $7, $7)`,
		o.Channel, o.Tenant, o.Location, "Канал "+label, []byte(credentials), verification, seedAt)
	must(t, pool, `INSERT INTO contacts(id, tenant_id, display_name, phone_normalized, email_normalized, created_at, updated_at)
		VALUES ($1, $2, $3, '+79990001122', $4, $5, $5)`, o.Contact, o.Tenant, "Клиент "+label, "client-"+label+"@example.test", seedAt)
	must(t, pool, `INSERT INTO external_identities(id, tenant_id, contact_id, provider, connection_id, external_id, metadata, created_at)
		VALUES ($1, $2, $3, 'TEST', $4, $5, '{"username":"client"}'::jsonb, $6)`, newID(t), o.Tenant, o.Contact, o.Channel, "ext-"+label, seedAt)
	must(t, pool, `INSERT INTO conversations(id, tenant_id, location_id, connection_id, contact_id, external_id, status, first_message_at, last_message_at, last_message_direction, revision, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, 'ACTIVE', $7, $7, 'OUTGOING', 2, $7, $7)`,
		o.Conversation, o.Tenant, o.Location, o.Channel, o.Contact, "conv-"+label, seedAt)
	must(t, pool, `INSERT INTO messages(id, tenant_id, conversation_id, connection_id, external_id, direction, type, text, sender_external_id, sent_at, received_at, metadata, created_at) VALUES
		($1, $3, $4, $5, $6, 'INCOMING', 'TEXT', $8, 'client', $9, $9, '{}'::jsonb, $9),
		($2, $3, $4, $5, $7, 'OUTGOING', 'TEXT', 'Здравствуйте, подскажем цену', NULL, $9, $9, '{}'::jsonb, $9)`,
		o.Incoming, o.Outgoing, o.Tenant, o.Conversation, o.Channel, "m1-"+label, "m2-"+label, "Нужна полировка. "+o.Marker, seedAt)
	must(t, pool, `INSERT INTO attachments(id, tenant_id, message_id, object_key, mime_type, size_bytes, sha256, provider_file_id, created_at)
		VALUES ($1, $2, $3, $4, 'image/jpeg', 2048, $5, $6, $7)`, newID(t), o.Tenant, o.Incoming, objectKey, digest("file-"+label), providerFile, seedAt)

	node, job, run := newID(t), newID(t), newID(t)
	must(t, pool, `INSERT INTO ai_nodes(id, name, secret_digest, status, created_at, updated_at) VALUES ($1, $2, $3, 'READY', $4, $4)`,
		node, "node-"+label, make([]byte, 32), seedAt)
	must(t, pool, `INSERT INTO ai_node_tenants(node_id, tenant_id, created_at) VALUES ($1, $2, $3)`, node, o.Tenant, seedAt)
	must(t, pool, `INSERT INTO ai_jobs(id, tenant_id, job_type, entity_type, entity_id, payload, model_requirement, schema_version, prompt_version, base_conversation_revision, analysis_through_message_id, status, available_at, created_at, updated_at)
		VALUES ($1, $2, 'ANALYZE_CONVERSATION', 'CONVERSATION', $3, '{}'::jsonb, 'any', 'v1', 'p1', 2, $4, 'PENDING', $5, $5, $5)`,
		job, o.Tenant, o.Conversation, o.Outgoing, seedAt)
	must(t, pool, `INSERT INTO ai_runs(id, tenant_id, job_id, node_id, entity_type, entity_id, status, application_status, base_conversation_revision, analysis_through_message_id, model_version, prompt_version, schema_version, started_at)
		VALUES ($1, $2, $3, $4, 'CONVERSATION', $5, 'RUNNING', 'PENDING', 2, $6, 'SENTINEL-MODEL', 'p1', 'v1', $7)`,
		run, o.Tenant, job, node, o.Conversation, o.Outgoing, seedAt)
	must(t, pool, `INSERT INTO conversation_summaries(tenant_id, conversation_id, summary_text, base_conversation_revision, analysis_through_message_id, model_version, prompt_version, schema_version, ai_run_id, updated_at, semantic_facts, agreements)
		VALUES ($1, $2, $3, 2, $4, 'SENTINEL-MODEL', 'p1', 'v1', $5, $6, '[]'::jsonb, '[]'::jsonb)`,
		o.Tenant, o.Conversation, "Клиент спрашивает про полировку. "+o.Marker, o.Outgoing, run, seedAt)

	must(t, pool, `INSERT INTO opportunities(id, tenant_id, conversation_id, service_id, stage, estimated_amount, currency, opened_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'PRICE_SENT', 5000, 'RUB', $5, $5, $5)`, o.Opportunity, o.Tenant, o.Conversation, o.Service, seedAt)
	must(t, pool, `INSERT INTO opportunity_stage_history(id, tenant_id, opportunity_id, from_stage, to_stage, source, actor_user_id, created_at) VALUES
		($1, $3, $4, NULL, 'NEW', 'RULE', NULL, $6), ($2, $3, $4, 'NEW', 'PRICE_SENT', 'USER', $5, $6)`,
		newID(t), newID(t), o.Tenant, o.Opportunity, o.Manager, seedAt)
	must(t, pool, `INSERT INTO risk_signals(id, tenant_id, opportunity_id, location_id, type, severity, status, reason_code, reason_text, source, risk_engine_version, trigger_message_id, detected_at, due_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'NO_RESPONSE', 'HIGH', 'OPEN', 'NO_RESPONSE_THRESHOLD_EXCEEDED', $5, 'RULE', 'no-response/v1', $6, $7, $7, $7, $7)`,
		o.Risk, o.Tenant, o.Opportunity, o.Location, "Не ответили клиенту. "+o.Marker, o.Incoming, seedAt)
	must(t, pool, `INSERT INTO recommendations(id, tenant_id, risk_id, text, source, created_at) VALUES ($1, $2, $3, 'Свяжитесь с клиентом', 'TEMPLATE', $4)`,
		newID(t), o.Tenant, o.Risk, seedAt)
	must(t, pool, `INSERT INTO risk_feedback(id, tenant_id, risk_id, opportunity_id, actor_user_id, verdict, reason, note, risk_type, severity, risk_status, source, risk_engine_version, trigger_message_id, opportunity_stage, detected_at, dataset_eligible, created_at)
		VALUES ($1, $2, $3, $4, $5, 'FALSE_POSITIVE', 'OTHER', $6, 'NO_RESPONSE', 'HIGH', 'OPEN', 'RULE', 'no-response/v1', $7, 'PRICE_SENT', $8, FALSE, $8)`,
		newID(t), o.Tenant, o.Risk, o.Opportunity, o.Manager, "Пояснение. "+o.Marker, o.Incoming, seedAt)
	must(t, pool, `INSERT INTO actions(id, tenant_id, risk_id, actor_user_id, type, note, created_at, opportunity_id) VALUES ($1, $2, $3, $4, 'CALL', $5, $6, $7)`,
		o.Action, o.Tenant, o.Risk, o.Owner, "Позвонили. "+o.Marker, seedAt, o.Opportunity)
	must(t, pool, `INSERT INTO outcomes(id, tenant_id, opportunity_id, actor_user_id, status, note, created_at) VALUES ($1, $2, $3, $4, 'BOOKED', $5, $6)`,
		o.Outcome, o.Tenant, o.Opportunity, o.Owner, "Записали. "+o.Marker, seedAt)
	must(t, pool, `INSERT INTO revenue_events(id, tenant_id, opportunity_id, amount, currency, status, source, confirmed_by_user_id, confirmed_at, created_at)
		VALUES ($1, $2, $3, 5000, 'RUB', 'CONFIRMED', 'USER_CONFIRMED', $4, $5, $5)`, o.RevenueEvent, o.Tenant, o.Opportunity, o.Owner, seedAt)
	must(t, pool, `INSERT INTO revenue_attributions(id, tenant_id, revenue_event_id, opportunity_id, type, created_at) VALUES ($1, $2, $3, $4, 'ORGANIC', $5)`,
		newID(t), o.Tenant, o.RevenueEvent, o.Opportunity, seedAt)
	must(t, pool, `INSERT INTO ml_consents(id, tenant_id, scope, granted_by, granted_at, revoked_by, revoked_at) VALUES ($1, $2, 'DATASETS', $3, $5, $4, $6)`,
		newID(t), o.Tenant, o.Owner, o.Manager, seedAt, seedAt.Add(time.Hour))
	must(t, pool, `INSERT INTO audit_log(id, tenant_id, actor_user_id, operation, entity_type, entity_id, created_at) VALUES ($1, $2, $3, 'ORGANIZATION_UPDATED', 'ORGANIZATION', $2, $4)`,
		newID(t), o.Tenant, o.Owner, seedAt)

	// Приглашения: ожидающее (создал владелец) и принятое менеджером.
	must(t, pool, `INSERT INTO membership_invitations(id, tenant_id, role, code_hash, note, created_by_user_id, created_at, expires_at) VALUES ($1, $2, 'MANAGER', $3, 'для администратора', $4, $5, $6)`,
		o.Invitation, o.Tenant, inviteHash, o.Owner, seedAt, seedAt.Add(7*24*time.Hour))
	must(t, pool, `INSERT INTO membership_invitations(id, tenant_id, role, code_hash, created_by_user_id, created_at, expires_at, accepted_at, accepted_by_user_id) VALUES ($1, $2, 'MANAGER', $3, $4, $5, $6, $5, $7)`,
		newID(t), o.Tenant, digest("accepted-"+label), o.Owner, seedAt, seedAt.Add(7*24*time.Hour), o.Manager)

	// Личное каждого участника: сессии, токены, журнал входов, уведомления, Telegram.
	// Адреса и user agent у каждого пользователя свои: так видно, что чужие данные в выгрузку не попали.
	for index, user := range []string{o.Owner, o.Manager} {
		host := 10*(1+int(label[0]-'a')) + 2*index
		first, second := fmt.Sprintf("203.0.113.%d", host), fmt.Sprintf("203.0.113.%d", host+1)
		must(t, pool, `INSERT INTO sessions(id, user_id, token_hash, expires_at, ip, user_agent, created_at, revoked_at) VALUES
			($1, $3, $4, $5, $8::inet, $9, $6, NULL),
			($2, $3, $7, $5, $10::inet, $11, $6, $6)`,
			newID(t), newID(t), user, digest("SENTINEL-TOKEN-HASH-"+user+"-1"), seedAt.Add(24*time.Hour), seedAt, digest("SENTINEL-TOKEN-HASH-"+user+"-2"),
			first, "Agent/"+user[:8]+"/current", second, "Agent/"+user[:8]+"/old")
		must(t, pool, `INSERT INTO account_tokens(id, user_id, purpose, token_hash, created_at, expires_at) VALUES ($1, $2, 'PASSWORD_RESET', $3, $4, $5)`,
			newID(t), user, digest("SENTINEL-TOKEN-HASH-reset-"+user), seedAt, seedAt.Add(time.Hour))
		must(t, pool, `INSERT INTO auth_audit_log(id, user_id, operation, ip_address, created_at) VALUES ($1, $2, 'USER_LOGGED_IN', $6, $3), ($4, $2, 'PASSWORD_CHANGED', $7, $5)`,
			newID(t), user, seedAt, newID(t), seedAt.Add(time.Minute), first, second)
	}
	for _, person := range []struct{ user string }{{o.Owner}, {o.Manager}} {
		must(t, pool, `INSERT INTO notification_preferences(id, tenant_id, user_id, risk_type, minimum_severity, delivery_mode, in_app_enabled, telegram_enabled, quiet_hours_enabled, quiet_hours_start, quiet_hours_end, digest_time, created_at, updated_at)
			VALUES ($1, $2, $3, 'NO_RESPONSE', 'MEDIUM', 'IMMEDIATE', TRUE, TRUE, TRUE, '22:00', '08:00', '09:30', $4, $4)`, newID(t), o.Tenant, person.user, seedAt)
	}
	must(t, pool, `INSERT INTO notifications(id, tenant_id, user_id, risk_id, kind, dedup_key, title, body, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'RISK_OPENED', $5, 'Риск', $6, $7, $7)`, o.Notification, o.Tenant, o.Manager, o.Risk, "dedup-"+label, "Тело уведомления", seedAt)
	must(t, pool, `INSERT INTO notification_deliveries(id, tenant_id, notification_id, channel, destination, title, body, attempt, status, available_at, created_at, updated_at, kind)
		VALUES ($1, $2, $3, 'IN_APP', 'inbox', 'Риск', 'Тело', 1, 'PENDING', $4, $4, $4, 'RISK_OPENED')`, newID(t), o.Tenant, o.Notification, seedAt)
	must(t, pool, `INSERT INTO notification_digest_items(id, tenant_id, user_id, risk_id, risk_type, reason, slot, deliver_at, in_app_enabled, telegram_enabled, created_at)
		VALUES ($1, $2, $3, $4, 'NO_RESPONSE', 'DIGEST', '2026-10-02T09:30', $5, TRUE, FALSE, $5)`, newID(t), o.Tenant, o.Manager, o.Risk, seedAt)
	must(t, pool, `INSERT INTO telegram_user_links(id, tenant_id, user_id, telegram_user_id, chat_id, linked_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $6)`,
		newID(t), o.Tenant, o.Manager, int64(7000000)+int64(len(label)), int64(9000000)+int64(len(label)), seedAt)
	must(t, pool, `INSERT INTO telegram_link_tokens(id, tenant_id, user_id, token_hash, expires_at, created_at) VALUES ($1, $2, $3, $4, $5, $6)`,
		newID(t), o.Tenant, o.Manager, digest("SENTINEL-TOKEN-HASH-telegram-"+label), seedAt.Add(time.Hour), seedAt)
	must(t, pool, `INSERT INTO telegram_callback_commands(id, tenant_id, user_id, notification_id, risk_id, idempotency_key, action, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, 'ACKNOWLEDGE', $7)`, newID(t), o.Tenant, o.Manager, o.Notification, o.Risk, "callback-"+label, seedAt)
	return o
}

// countOf возвращает число строк по запросу.
func countOf(t testing.TB, pool *pgxpool.Pool, query string, arguments ...any) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(), query, arguments...).Scan(&count); err != nil {
		t.Fatalf("%s: %v", strings.Join(strings.Fields(query), " "), err)
	}
	return count
}

func describe(o org) string { return fmt.Sprintf("%s(%s)", o.Label, o.Tenant) }

// tenantContext — контекст запроса с организацией, как его задаёт промежуточный слой API.
func tenantContext(tenant string) context.Context {
	return tenantctx.WithTenant(context.Background(), tenant)
}
