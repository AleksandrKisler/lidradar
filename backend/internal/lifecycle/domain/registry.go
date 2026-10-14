// Package domain describes what LidRadar holds about a user and an organization, what leaves in an export and
// what happens to it when an account is closed (ADR 0059).
package domain

// Реестр данных — единственное место, где записано, как таблицы других модулей участвуют в выгрузке и
// обезличивании. Тест на живой схеме (infrastructure/schema_test.go) сверяет реестр с базой: новая ссылка на
// пользователя, новая таблица с tenant_id или новая колонка выгружаемой таблицы без записи здесь ломает сборку.

// Disposition — что происходит со ссылкой на пользователя при обезличивании учётной записи.
type Disposition string

const (
	// Erase: строки пользователя удаляются.
	Erase Disposition = "ERASE"
	// Anonymize: строка остаётся, личные поля заменяются.
	Anonymize Disposition = "ANONYMIZE"
	// Revoke: строка остаётся, доступ закрывается отметкой (членство, неиспользованные приглашения).
	Revoke Disposition = "REVOKE"
	// Retain: строка остаётся целиком — личного в ней, кроме идентификатора, нет, а идентификатор после
	// обезличивания пользователя не ведёт к человеку. Строки нельзя удалить: на них ссылаются неизменяемые факты.
	Retain Disposition = "RETAIN"
)

// UserReference — одна колонка, указывающая на пользователя (прямо или через членство), и судьба строк.
type UserReference struct {
	Table       string
	Column      string
	Disposition Disposition
	// Export — раздел личной выгрузки, куда попадают эти данные; пусто, если они в неё не входят.
	Export string
	// Reason объясняет решение, если оно не очевидно.
	Reason string
}

// UserReferences перечисляет все колонки, ссылающиеся на users(id) или memberships(user_id), и дополнительно
// идентификаторы администратора без внешнего ключа (`discarded_by`).
var UserReferences = []UserReference{
	{Table: "users", Column: "id", Disposition: Anonymize, Export: "account",
		Reason: "строка остаётся: на неё ссылаются журналы; адрес заменяется, имя «Удалён», пароль непригоден для входа"},
	{Table: "sessions", Column: "user_id", Disposition: Erase, Export: "sessions", Reason: "IP и user agent"},
	{Table: "account_tokens", Column: "user_id", Disposition: Erase, Reason: "только хэши одноразовых токенов"},
	{Table: "memberships", Column: "user_id", Disposition: Revoke, Export: "memberships",
		Reason: "членство не удаляется (триггер): на него ссылаются неизменяемые факты; доступ закрывается DISABLED и revoked_at"},
	{Table: "notifications", Column: "user_id", Disposition: Erase,
		Reason: "производные от рисков организации; тексты в личную выгрузку не входят, доставки уходят каскадом"},
	{Table: "notification_preferences", Column: "user_id", Disposition: Erase, Export: "notificationSettings"},
	{Table: "notification_digest_items", Column: "user_id", Disposition: Erase,
		Reason: "отложенные уведомления пользователя; производные от рисков организации, личных данных нет"},
	{Table: "telegram_user_links", Column: "user_id", Disposition: Erase, Export: "telegramLinks", Reason: "идентификатор Telegram и чата"},
	{Table: "telegram_link_tokens", Column: "user_id", Disposition: Erase, Reason: "только хэши одноразовых токенов привязки"},
	{Table: "telegram_callback_commands", Column: "user_id", Disposition: Erase, Reason: "служебные ключи повторов нажатий кнопок"},
	{Table: "actions", Column: "actor_user_id", Disposition: Retain, Export: "authoredRecords",
		Reason: "действие — факт организации; в личной выгрузке только число"},
	{Table: "outcomes", Column: "actor_user_id", Disposition: Retain, Export: "authoredRecords"},
	{Table: "audit_log", Column: "actor_user_id", Disposition: Retain, Export: "authoredRecords"},
	{Table: "revenue_events", Column: "confirmed_by_user_id", Disposition: Retain, Export: "authoredRecords"},
	{Table: "risk_feedback", Column: "actor_user_id", Disposition: Retain, Export: "authoredRecords"},
	{Table: "opportunity_stage_history", Column: "actor_user_id", Disposition: Retain, Export: "authoredRecords"},
	{Table: "ml_consents", Column: "granted_by", Disposition: Retain, Export: "consentDecisions"},
	{Table: "ml_consents", Column: "revoked_by", Disposition: Retain, Export: "consentDecisions"},
	{Table: "membership_invitations", Column: "created_by_user_id", Disposition: Revoke, Export: "authoredRecords",
		Reason: "неиспользованные приглашения пользователя отзываются, строки остаются"},
	{Table: "membership_invitations", Column: "accepted_by_user_id", Disposition: Retain},
	{Table: "membership_invitations", Column: "revoked_by_user_id", Disposition: Retain},
	{Table: "platform_admins", Column: "user_id", Disposition: Retain,
		Reason: "действующий платформенный администратор не может закрыть учётную запись; история выдач остаётся"},
	{Table: "platform_admins", Column: "granted_by", Disposition: Retain},
	{Table: "platform_admins", Column: "revoked_by", Disposition: Retain},
	{Table: "platform_invitations", Column: "consumed_by_user_id", Disposition: Retain},
	{Table: "admin_audit_log", Column: "actor_user_id", Disposition: Retain, Reason: "журнал оператора платформы"},
	{Table: "auth_audit_log", Column: "user_id", Disposition: Retain, Export: "authEvents",
		Reason: "журнал безопасности, 24 месяца; IP обнуляются механизмом 3 ADR 0056, которого ещё нет"},
	{Table: "ai_jobs", Column: "discarded_by", Disposition: Retain, Reason: "идентификатор администратора платформы, без ключа"},
	{Table: "jobs", Column: "discarded_by", Disposition: Retain, Reason: "идентификатор администратора платформы, без ключа"},
	{Table: "notification_deliveries", Column: "discarded_by", Disposition: Retain, Reason: "идентификатор администратора платформы, без ключа"},
	{Table: "outbox_events", Column: "discarded_by", Disposition: Retain, Reason: "идентификатор администратора платформы, без ключа"},
}

// NotUserColumns — колонки, которые по имени похожи на ссылку на пользователя, но ею не являются.
var NotUserColumns = map[string]string{
	"ai_jobs.leased_by": "идентификатор AI-узла, а не пользователя",
}

// Dataset — набор выгрузки организации: одна таблица (или особый источник) и явный список колонок.
type Dataset struct {
	// File — имя файла в архиве без каталога и расширения; Table — таблица-источник.
	File  string
	Table string
	// Description — строка о наборе в README архива.
	Description string
	// Columns — выгружаемые колонки в порядке полей JSON. Колонки tenant_id среди них нет никогда.
	Columns []string
	// OrderBy — порядок строк; по умолчанию id.
	OrderBy string
	// Source отличает особые наборы от выборки одной таблицы по tenant_id.
	Source SourceKind
}

// SourceKind — способ выбрать строки набора.
type SourceKind string

const (
	// SourceTable: выборка таблицы по условию tenant_id.
	SourceTable SourceKind = ""
	// SourceTeam: членства организации вместе с адресом и именем из учётной записи.
	SourceTeam SourceKind = "TEAM"
)

// OrganizationDatasets — всё, что владелец получает в выгрузке организации, в порядке файлов архива.
var OrganizationDatasets = []Dataset{
	{File: "team", Description: "участники организации и их роли; адрес и имя берутся из учётной записи (у закрытых записей адрес пуст)", Table: "memberships", Source: SourceTeam, OrderBy: "created_at, membership_id",
		Columns: []string{"membership_id", "user_id", "email", "display_name", "role", "status", "created_at", "updated_at", "revoked_at"}},
	{File: "invitations", Description: "приглашения сотрудников без кодов: роль, срок, кто создал, принято или отозвано", Table: "membership_invitations",
		Columns: []string{"id", "role", "note", "created_by_user_id", "created_at", "expires_at", "accepted_at", "accepted_by_user_id", "revoked_at", "revoked_by_user_id"}},
	{File: "locations", Description: "точки и пороги ответа", Table: "locations",
		Columns: []string{"id", "name", "timezone", "response_threshold_minutes", "agreement_threshold_minutes", "active", "created_at", "updated_at"}},
	{File: "business-hours", Description: "часы работы точек по дням недели", Table: "location_business_hours",
		Columns: []string{"id", "location_id", "weekday", "is_closed", "opens_at", "closes_at", "created_at", "updated_at"}},
	{File: "services", Description: "каталог услуг и цены", Table: "service_catalog_items",
		Columns: []string{"id", "location_id", "name", "price_from", "price_to", "currency", "active", "created_at", "updated_at"}},
	{File: "channels", Description: "подключённые каналы без реквизитов доступа", Table: "channel_connections",
		Columns: []string{"id", "location_id", "provider", "name", "status", "capabilities", "last_event_at", "last_success_at", "last_error_at", "last_error_code", "created_at", "updated_at"}},
	{File: "contacts", Description: "контакты клиентов: имя, нормализованные телефон и почта", Table: "contacts",
		Columns: []string{"id", "display_name", "phone_normalized", "email_normalized", "created_at", "updated_at"}},
	{File: "external-identities", Description: "идентификаторы контактов у провайдеров каналов", Table: "external_identities",
		Columns: []string{"id", "contact_id", "provider", "connection_id", "external_id", "metadata", "created_at"}},
	{File: "conversations", Description: "переписки: канал, контакт, статус, времена", Table: "conversations",
		Columns: []string{"id", "location_id", "connection_id", "contact_id", "external_id", "status", "first_message_at", "last_message_at", "last_message_direction", "revision", "created_at", "updated_at"}},
	{File: "messages", Description: "сообщения переписок с текстом", Table: "messages",
		Columns: []string{"id", "conversation_id", "connection_id", "external_id", "direction", "type", "text", "sender_external_id", "reply_to_message_id", "sent_at", "received_at", "provider_deleted_at", "metadata", "created_at"}},
	{File: "attachments", Description: "метаданные вложений: тип, размер, контрольная сумма (файлы не хранятся)", Table: "attachments",
		Columns: []string{"id", "message_id", "mime_type", "size_bytes", "sha256", "created_at"}},
	{File: "conversation-summaries", Description: "сводки переписок и зафиксированные договорённости", Table: "conversation_summaries", OrderBy: "conversation_id",
		Columns: []string{"conversation_id", "summary_text", "semantic_facts", "agreements", "base_conversation_revision", "analysis_through_message_id", "updated_at"}},
	{File: "opportunities", Description: "коммерческие возможности (сделки): стадия, оценка, времена", Table: "opportunities",
		Columns: []string{"id", "conversation_id", "service_id", "stage", "estimated_amount", "estimated_amount_confidence", "currency", "opened_at", "closed_at", "created_at", "updated_at"}},
	{File: "opportunity-stage-history", Description: "история стадий сделок", Table: "opportunity_stage_history",
		Columns: []string{"id", "opportunity_id", "from_stage", "to_stage", "source", "confidence", "actor_user_id", "created_at"}},
	{File: "risks", Description: "риски потери выручки: тип, важность, статус, причина, времена", Table: "risk_signals",
		Columns: []string{"id", "opportunity_id", "location_id", "type", "severity", "status", "reason_code", "reason_text", "confidence", "source", "risk_engine_version", "trigger_message_id", "detected_at", "due_at", "acknowledged_at", "acted_at", "resolved_at", "created_at", "updated_at"}},
	{File: "recommendations", Description: "рекомендации по рискам", Table: "recommendations",
		Columns: []string{"id", "risk_id", "text", "source", "created_at"}},
	{File: "risk-feedback", Description: "вердикты команды по рискам и пояснения к ним", Table: "risk_feedback",
		Columns: []string{"id", "risk_id", "opportunity_id", "actor_user_id", "verdict", "reason", "note", "risk_type", "severity", "risk_status", "source", "risk_engine_version", "trigger_message_id", "opportunity_stage", "detected_at", "dataset_eligible", "created_at"}},
	{File: "actions", Description: "действия команды по рискам", Table: "actions",
		Columns: []string{"id", "risk_id", "opportunity_id", "actor_user_id", "type", "note", "created_at"}},
	{File: "outcomes", Description: "исходы по сделкам", Table: "outcomes",
		Columns: []string{"id", "opportunity_id", "actor_user_id", "status", "note", "created_at"}},
	{File: "revenue-events", Description: "подтверждённая выручка", Table: "revenue_events",
		Columns: []string{"id", "opportunity_id", "amount", "currency", "status", "source", "confirmed_by_user_id", "confirmed_at", "created_at"}},
	{File: "revenue-attributions", Description: "привязка выручки к рискам, действиям и исходам", Table: "revenue_attributions",
		Columns: []string{"id", "revenue_event_id", "opportunity_id", "type", "risk_id", "action_id", "outcome_id", "created_at"}},
	{File: "ml-consents", Description: "история согласия организации на использование данных в наборах", Table: "ml_consents",
		Columns: []string{"id", "scope", "granted_by", "granted_at", "revoked_by", "revoked_at"}},
	{File: "audit-log", Description: "журнал действий участников организации", Table: "audit_log",
		Columns: []string{"id", "actor_user_id", "operation", "entity_type", "entity_id", "created_at"}},
}

// OrganizationColumns — колонки organizations, попадающие в organization.json. Идентификатор организации
// наружу не выходит.
var OrganizationColumns = []string{"name", "default_timezone", "default_currency", "status", "created_at", "updated_at"}

// ExcludedTables — таблицы с tenant_id, которых нет в выгрузке организации, и причина.
var ExcludedTables = map[string]string{
	"admin_audit_log":            "журнал команд оператора платформы, не данные организации",
	"ai_jobs":                    "очередь анализа: служебное состояние обработки",
	"ai_node_tenants":            "допуск AI-узлов: служебная настройка платформы",
	"ai_runs":                    "ответы модели: служебная копия; результат разбора уже в сводках и рисках",
	"idempotency_keys":           "служебные ответы повторов запросов",
	"jobs":                       "очередь фоновых заданий: служебное состояние",
	"notification_deliveries":    "очередь доставки уведомлений: служебное состояние",
	"notification_digest_items":  "личное каждого участника: входит в его личную выгрузку, а не в выгрузку организации",
	"notification_preferences":   "личное каждого участника: входит в его личную выгрузку",
	"notifications":              "личный ящик каждого участника; производные от рисков, которые выгружены",
	"outbox_events":              "исходящий журнал событий: служебное состояние",
	"raw_events":                 "сырой буфер приёма вебхуков (14 суток); содержимое уже в сообщениях и контактах",
	"scheduled_checks":           "отложенные проверки: служебное состояние",
	"telegram_callback_commands": "личное каждого участника и служебные ключи повторов",
	"telegram_link_tokens":       "секреты привязки Telegram",
	"telegram_user_links":        "личное каждого участника: входит в его личную выгрузку",
}

// tenantColumn не выгружается ни из одной таблицы: организация и так известна, а идентификатор наружу не выходит.
const tenantColumn = "tenant_id"

// ExcludedColumns — колонки выгружаемых таблиц, которые в выгрузку не входят, и причина. tenant_id отдельно не
// перечисляется: он исключён из каждого набора.
var ExcludedColumns = map[string]map[string]string{
	"channel_connections": {
		"encrypted_credentials":    "секрет доступа к каналу",
		"verification_secret_hash": "секрет проверки вебхуков",
	},
	"membership_invitations": {"code_hash": "хэш кода приглашения: по нему код можно подобрать оффлайн"},
	"service_catalog_items":  {"normalized_name": "технический ключ поиска"},
	"attachments": {
		"object_key":       "ключ внутреннего хранилища",
		"provider_file_id": "идентификатор файла у провайдера",
	},
	"conversation_summaries": {
		"model_version":  "сведения об обработке",
		"prompt_version": "сведения об обработке",
		"schema_version": "сведения об обработке",
		"ai_run_id":      "ссылка на служебный запуск модели",
	},
	"opportunity_stage_history": {"ai_run_id": "ссылка на служебный запуск модели"},
	"risk_signals":              {"ai_run_id": "ссылка на служебный запуск модели"},
	"risk_feedback":             {"ai_run_id": "ссылка на служебный запуск модели"},
}

// TenantColumn возвращает имя колонки, исключённой из всех наборов.
func TenantColumn() string { return tenantColumn }

// DatasetByFile находит набор по имени файла.
func DatasetByFile(file string) (Dataset, bool) {
	for _, dataset := range OrganizationDatasets {
		if dataset.File == file {
			return dataset, true
		}
	}
	return Dataset{}, false
}

// Order возвращает порядок строк набора.
func (dataset Dataset) Order() string {
	if dataset.OrderBy != "" {
		return dataset.OrderBy
	}
	return "id"
}
