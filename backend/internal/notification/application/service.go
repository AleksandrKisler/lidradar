// Package application координирует создание уведомлений, политику шумности
// получателя и устойчивую доставку.
package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	eventsapplication "lidradar/backend/internal/events/application"
	eventsdomain "lidradar/backend/internal/events/domain"
	jobsapplication "lidradar/backend/internal/jobs/application"
	jobsdomain "lidradar/backend/internal/jobs/domain"
	"lidradar/backend/internal/notification/domain"
	riskdomain "lidradar/backend/internal/risk/domain"
)

var (
	ErrNotFound       = errors.New("ресурс уведомления не найден")
	ErrForbidden      = errors.New("нет разрешения на уведомления")
	ErrUnsafeCallback = errors.New("небезопасная команда Telegram")
	ErrLeaseLost      = errors.New("аренда доставки потеряна")
)

const (
	RiskOpenedEventType  = "risk.opened.v1"
	DefaultDeliveryLease = 30 * time.Second
	DefaultLinkTokenTTL  = 15 * time.Minute

	// Отложенная доставка живёт в общих проверках по расписанию (этап 6):
	// сводка слота получателя и эскалация риска владельцу.
	DigestCheckType     = "NOTIFICATION_DIGEST_DUE"
	DigestJobType       = "notification.digest.v1"
	EscalationCheckType = "NOTIFICATION_ESCALATION_DUE"
	EscalationJobType   = "notification.escalate.v1"
)

type Store interface {
	// Create атомарно сохраняет логический факт и его первые попытки. Уникальный
	// (tenant_id, dedup_key) превращает повтор события в безопасное чтение.
	Create(context.Context, domain.Notification, []domain.Delivery) (domain.Notification, bool, error)
	ClaimDue(context.Context, string, time.Time, time.Time, int, []domain.Channel) ([]domain.Delivery, error)
	Complete(context.Context, string, domain.Delivery, *domain.Delivery) error
}

type LinkStore interface {
	TelegramDestination(context.Context, string, string) (string, bool, error)
}

type Transport interface {
	// Send доставляет текст; actions добавляет кнопки управления одним риском.
	Send(ctx context.Context, destination, title, body, notificationID string, actions bool) (providerMessageID string, retryable bool, err error)
}

type IDs interface{ NewID() (string, error) }

// Recipient — активный участник организации с необязательной Telegram-привязкой
// и явно сохранёнными настройками; отсутствующая настройка действует по умолчанию.
type Recipient struct {
	UserID              string
	TelegramDestination string
	Preferences         map[domain.RiskType]domain.Preference
}

func (recipient Recipient) Preference(tenantID string, riskType domain.RiskType) domain.Preference {
	if preference, found := recipient.Preferences[riskType]; found {
		return preference
	}
	return domain.DefaultPreference(tenantID, recipient.UserID, riskType)
}

// TenantPolicy — часовой пояс организации (ТЗ §3.7) и её получатели.
type TenantPolicy struct {
	Timezone   string
	Recipients []Recipient
}

type PolicyStore interface {
	TenantPolicy(context.Context, string) (TenantPolicy, error)
	OwnerRecipient(context.Context, string) (Recipient, bool, error)
	Timezone(context.Context, string) (string, bool, error)
	EnqueueDigestItem(context.Context, domain.DigestItem) (bool, error)
	PendingDigestEntries(context.Context, string, string, string) ([]domain.DigestEntry, error)
	// CreateDigest атомарно сохраняет сводку, её попытки и помечает элементы
	// очереди доставленными; повтор слота возвращает существующую сводку.
	CreateDigest(context.Context, domain.Notification, []domain.Delivery, []string) (domain.Notification, bool, error)
	ConsumeDigestItems(context.Context, string, []string, time.Time) error
	RiskAwaitingAction(context.Context, string, string) (bool, error)
}

type Scheduler interface {
	Schedule(context.Context, jobsdomain.ScheduledCheck) (jobsdomain.ScheduledCheck, bool, error)
}

type Service struct {
	store      Store
	links      LinkStore
	telegram   Transport
	ids        IDs
	now        func() time.Time
	policies   PolicyStore
	scheduler  Scheduler
	escalation domain.EscalationPolicy
}

func NewService(store Store, links LinkStore, telegram Transport, ids IDs, now func() time.Time) Service {
	return Service{store: store, links: links, telegram: telegram, ids: ids, now: now}
}

// WithPolicy подключает настройки получателей и очередь отложенной доставки.
func (service Service) WithPolicy(policies PolicyStore, scheduler Scheduler) Service {
	service.policies, service.scheduler = policies, scheduler
	return service
}

// WithEscalation задаёт основу эскалации владельцу (LR-BE-2010); по умолчанию выключена.
func (service Service) WithEscalation(policy domain.EscalationPolicy) Service {
	service.escalation = policy
	return service
}

// NotifyRisk фиксирует намерение Telegram-доставки до внешнего запроса. Повтор
// возвращает уже существующее логическое уведомление и не создаёт новую доставку.
func (service Service) NotifyRisk(
	ctx context.Context,
	tenantID, userID, riskID, title, body string,
) (domain.Notification, bool, error) {
	if service.store == nil || service.links == nil || service.ids == nil || service.now == nil {
		return domain.Notification{}, false, domain.ErrInvalid
	}
	destination, found, err := service.links.TelegramDestination(ctx, tenantID, userID)
	if err != nil {
		return domain.Notification{}, false, err
	}
	if !found {
		return domain.Notification{}, false, ErrNotFound
	}
	notificationID, err := service.ids.NewID()
	if err != nil {
		return domain.Notification{}, false, err
	}
	now := service.now().UTC()
	notification, err := domain.NewNotification(notificationID, tenantID, userID, riskID, title, body, now)
	if err != nil {
		return domain.Notification{}, false, err
	}
	deliveries, err := service.deliveries(notification, destination, false, true, now)
	if err != nil {
		return domain.Notification{}, false, err
	}
	return service.store.Create(ctx, notification, deliveries)
}

// deliveries собирает первые попытки по включённым каналам: in-app всегда
// локален и адресован самому пользователю, Telegram требует активной привязки.
func (service Service) deliveries(
	notification domain.Notification,
	telegramDestination string,
	inApp, telegram bool,
	at time.Time,
) ([]domain.Delivery, error) {
	deliveries := make([]domain.Delivery, 0, 2)
	if inApp {
		id, err := service.ids.NewID()
		if err != nil {
			return nil, err
		}
		delivery, err := domain.NewDelivery(id, notification, notification.UserID, domain.ChannelInApp, at)
		if err != nil {
			return nil, err
		}
		deliveries = append(deliveries, delivery)
	}
	if telegram && telegramDestination != "" {
		id, err := service.ids.NewID()
		if err != nil {
			return nil, err
		}
		delivery, err := domain.NewDelivery(id, notification, telegramDestination, domain.ChannelTelegram, at)
		if err != nil {
			return nil, err
		}
		deliveries = append(deliveries, delivery)
	}
	if len(deliveries) == 0 {
		return nil, ErrNotFound
	}
	return deliveries, nil
}

// DispatchOne арендует и выполняет не более одной попытки по указанным каналам.
// In-app попытка завершается локально: логический факт уже виден в приложении.
// Провайдер никак не меняет Risk: успех и отказ отражаются только в доставке.
func (service Service) DispatchOne(ctx context.Context, owner string, lease time.Duration, channels ...domain.Channel) (bool, error) {
	if service.store == nil || service.ids == nil || service.now == nil || owner == "" {
		return false, domain.ErrInvalid
	}
	if len(channels) == 0 {
		channels = []domain.Channel{domain.ChannelInApp, domain.ChannelTelegram}
	}
	for _, channel := range channels {
		if !domain.ValidChannel(channel) || (channel == domain.ChannelTelegram && service.telegram == nil) {
			return false, domain.ErrInvalid
		}
	}
	if lease <= 0 {
		lease = DefaultDeliveryLease
	}
	now := service.now().UTC()
	deliveries, err := service.store.ClaimDue(ctx, owner, now, now.Add(lease), 1, channels)
	if err != nil || len(deliveries) == 0 {
		return false, err
	}
	delivery := deliveries[0]
	var messageID string
	var retryable bool
	var sendErr error
	if delivery.Channel == domain.ChannelInApp {
		messageID = "in-app:" + delivery.NotificationID
	} else {
		messageID, retryable, sendErr = service.telegram.Send(
			ctx, delivery.Destination, delivery.Title, delivery.Body, delivery.NotificationID, delivery.Actions(),
		)
	}
	finishedAt := service.now().UTC()
	delivery.LeasedBy, delivery.LeaseUntil = nil, nil
	delivery.AttemptedAt = &finishedAt
	delivery.UpdatedAt = finishedAt
	var retry *domain.Delivery
	if sendErr == nil {
		delivery.Status = domain.DeliverySucceeded
		delivery.ProviderMessageID = messageID
	} else if retryable && delivery.Attempt < 5 {
		delivery.Status = domain.DeliveryRetry
		delivery.FailureCode = "TELEGRAM_PROVIDER_ERROR"
		retryID, idErr := service.ids.NewID()
		if idErr != nil {
			return true, idErr
		}
		next := delivery
		next.ID = retryID
		next.Attempt++
		next.Status = domain.DeliveryPending
		next.AvailableAt = finishedAt.Add(jobsdomain.RetryDelay(delivery.Attempt))
		next.AttemptedAt = nil
		next.ProviderMessageID = ""
		next.FailureCode = ""
		next.CreatedAt, next.UpdatedAt = finishedAt, finishedAt
		retry = &next
	} else {
		delivery.Status = domain.DeliveryDead
		delivery.FailureCode = "TELEGRAM_PROVIDER_ERROR"
	}
	if err := service.store.Complete(ctx, owner, delivery, retry); err != nil {
		return true, err
	}
	return true, nil
}

// RiskOpened — данные события открытия риска, нужные политике уведомлений.
type RiskOpened struct {
	RiskID   string
	Type     domain.RiskType
	Severity domain.Severity
}

// ApplyRiskOpened применяет настройку каждого активного участника к открытому
// риску: немедленная доставка, элемент сводки в слот либо ничего. Повтор
// события безопасен: ключи уведомлений и элементов детерминированы (ТЗ §47).
func (service Service) ApplyRiskOpened(ctx context.Context, tenantID string, risk RiskOpened) error {
	if service.store == nil || service.policies == nil || service.scheduler == nil || service.ids == nil || service.now == nil ||
		tenantID == "" || risk.RiskID == "" || !domain.ValidRiskType(risk.Type) || !domain.ValidSeverity(risk.Severity) {
		return domain.ErrInvalid
	}
	policy, err := service.policies.TenantPolicy(ctx, tenantID)
	if err != nil {
		return err
	}
	location, err := time.LoadLocation(policy.Timezone)
	if err != nil {
		return fmt.Errorf("%w: часовой пояс организации", domain.ErrInvalid)
	}
	now := service.now().UTC()
	for _, recipient := range policy.Recipients {
		preference := recipient.Preference(tenantID, risk.Type)
		decision := preference.Decide(risk.Severity, now, location, recipient.TelegramDestination != "")
		if !decision.Deliver {
			continue
		}
		if decision.Immediate() {
			if err := service.notifyImmediately(ctx, tenantID, recipient, risk, decision, now); err != nil {
				return err
			}
			continue
		}
		if err := service.deferToSlot(ctx, tenantID, recipient, risk, decision, now); err != nil {
			return err
		}
	}
	if service.escalation.Applies(risk.Severity) {
		return service.scheduleEscalation(ctx, tenantID, risk.RiskID, now)
	}
	return nil
}

func (service Service) notifyImmediately(
	ctx context.Context,
	tenantID string,
	recipient Recipient,
	risk RiskOpened,
	decision domain.PolicyDecision,
	now time.Time,
) error {
	title, body := RiskOpenedText(risk.Type, risk.Severity)
	notificationID, err := service.ids.NewID()
	if err != nil {
		return err
	}
	notification, err := domain.NewNotification(notificationID, tenantID, recipient.UserID, risk.RiskID, title, body, now)
	if err != nil {
		return err
	}
	deliveries, err := service.deliveries(notification, recipient.TelegramDestination, decision.InApp, decision.Telegram, now)
	if err != nil {
		return err
	}
	_, _, err = service.store.Create(ctx, notification, deliveries)
	return err
}

func (service Service) deferToSlot(
	ctx context.Context,
	tenantID string,
	recipient Recipient,
	risk RiskOpened,
	decision domain.PolicyDecision,
	now time.Time,
) error {
	itemID, err := service.ids.NewID()
	if err != nil {
		return err
	}
	item, err := domain.NewDigestItem(itemID, tenantID, recipient.UserID, risk.RiskID, risk.Type, decision, now)
	if err != nil {
		return err
	}
	if _, err := service.policies.EnqueueDigestItem(ctx, item); err != nil {
		return err
	}
	return service.scheduleDigest(ctx, tenantID, recipient.UserID, decision.Slot, decision.DeliverAt, now)
}

type digestPayload struct {
	UserID string `json:"userId"`
	Slot   string `json:"slot"`
}

type escalationPayload struct {
	RiskID string `json:"riskId"`
}

func (service Service) scheduleDigest(ctx context.Context, tenantID, userID, slot string, dueAt, now time.Time) error {
	checkID, err := service.ids.NewID()
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(digestPayload{UserID: userID, Slot: slot})
	check, err := jobsdomain.NewScheduledCheck(
		checkID, tenantID, DigestCheckType, "user", userID, DigestJobType,
		domain.DigestDedupKey(userID, slot), payload, dueAt, now,
	)
	if err != nil {
		return err
	}
	return ignoreScheduleConflict(service.scheduler.Schedule(ctx, check))
}

func (service Service) scheduleEscalation(ctx context.Context, tenantID, riskID string, now time.Time) error {
	checkID, err := service.ids.NewID()
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(escalationPayload{RiskID: riskID})
	check, err := jobsdomain.NewScheduledCheck(
		checkID, tenantID, EscalationCheckType, "risk", riskID, EscalationJobType,
		"risk:"+riskID+":escalation", payload, now.Add(service.escalation.After), now,
	)
	if err != nil {
		return err
	}
	return ignoreScheduleConflict(service.scheduler.Schedule(ctx, check))
}

// ignoreScheduleConflict: проверка того же слота уже существует, пусть и с
// другим сроком (например, после смены часового пояса); элемент очереди всё
// равно попадёт в неё, потому что выборка идёт по слоту.
func ignoreScheduleConflict(_ jobsdomain.ScheduledCheck, _ bool, err error) error {
	if errors.Is(err, jobsdomain.ErrConflict) {
		return nil
	}
	return err
}

// DeliverDigest доставляет одним сообщением все элементы слота с актуальным на
// момент отправки состоянием рисков (LR-BE-RM-020). Закрытые риски выпадают;
// пустая сводка не создаёт уведомления, но помечает элементы обработанными.
func (service Service) DeliverDigest(ctx context.Context, tenantID, userID, slot string) error {
	if service.policies == nil || service.links == nil || service.ids == nil || service.now == nil ||
		tenantID == "" || userID == "" || !domain.ValidSlot(slot) {
		return domain.ErrInvalid
	}
	entries, err := service.policies.PendingDigestEntries(ctx, tenantID, userID, slot)
	if err != nil || len(entries) == 0 {
		return err
	}
	now := service.now().UTC()
	itemIDs := make([]string, 0, len(entries))
	active := make([]domain.DigestEntry, 0, len(entries))
	inApp, telegram := false, false
	for _, entry := range entries {
		itemIDs = append(itemIDs, entry.Item.ID)
		if !entry.Active() {
			continue
		}
		active = append(active, entry)
		inApp = inApp || entry.Item.InApp
		telegram = telegram || entry.Item.Telegram
	}
	destination := ""
	if telegram {
		linked, found, err := service.links.TelegramDestination(ctx, tenantID, userID)
		if err != nil {
			return err
		}
		if found {
			destination = linked
		} else {
			telegram = false
		}
	}
	if len(active) == 0 || (!inApp && !telegram) {
		return service.policies.ConsumeDigestItems(ctx, tenantID, itemIDs, now)
	}
	location := time.UTC
	if timezone, found, err := service.policies.Timezone(ctx, tenantID); err != nil {
		return err
	} else if found {
		if loaded, loadErr := time.LoadLocation(timezone); loadErr == nil {
			location = loaded
		}
	}
	title, body := domain.ComposeDigest(active, location)
	notificationID, err := service.ids.NewID()
	if err != nil {
		return err
	}
	notification, err := domain.NewDigest(notificationID, tenantID, userID, slot, title, body, now)
	if err != nil {
		return err
	}
	deliveries, err := service.deliveries(notification, destination, inApp, telegram, now)
	if err != nil {
		return err
	}
	_, _, err = service.policies.CreateDigest(ctx, notification, deliveries, itemIDs)
	return err
}

// Escalate уведомляет владельца о риске, который никто не принял в работу за
// отведённое время. Принятый, отработанный или закрытый риск эскалации не даёт.
func (service Service) Escalate(ctx context.Context, tenantID, riskID string) error {
	if service.store == nil || service.policies == nil || service.ids == nil || service.now == nil || tenantID == "" || riskID == "" {
		return domain.ErrInvalid
	}
	awaiting, err := service.policies.RiskAwaitingAction(ctx, tenantID, riskID)
	if err != nil || !awaiting {
		return err
	}
	owner, found, err := service.policies.OwnerRecipient(ctx, tenantID)
	if err != nil || !found {
		return err
	}
	now := service.now().UTC()
	notificationID, err := service.ids.NewID()
	if err != nil {
		return err
	}
	body := fmt.Sprintf(
		"Риск открыт дольше %d минут и никем не принят в работу. Откройте Radar для подробностей.",
		int(service.escalation.After.Minutes()),
	)
	notification, err := domain.NewEscalation(notificationID, tenantID, owner.UserID, riskID, "Эскалация: риск без реакции", body, now)
	if err != nil {
		return err
	}
	deliveries, err := service.deliveries(notification, owner.TelegramDestination, true, owner.TelegramDestination != "", now)
	if err != nil {
		return err
	}
	_, _, err = service.store.Create(ctx, notification, deliveries)
	return err
}

// RiskOpenedText возвращает заголовок и текст немедленного уведомления (ТЗ §46).
func RiskOpenedText(riskType domain.RiskType, severity domain.Severity) (string, string) {
	switch riskType {
	case domain.RiskPromiseNotFulfilled:
		return "Риск: обещание клиенту не выполнено",
			"Выполните обещанное клиенту или сообщите новый точный срок. Откройте Radar для подробностей."
	case domain.RiskBookingNotConfirmed:
		return "Критический риск: запись не подтверждена",
			"Предложите клиенту конкретный свободный слот. Откройте Radar для подробностей."
	case domain.RiskCustomerSilentAfterPrice:
		return "Риск: клиент молчит после цены",
			"Напомните клиенту о предложении и уточните, актуальна ли услуга. Откройте Radar для подробностей."
	case domain.RiskFollowUpCandidate:
		return "Риск: стоит напомнить о себе",
			"Уточните, остаётся ли услуга актуальной. Откройте Radar для подробностей."
	default:
		title := "Риск: клиент ждёт ответа"
		if severity == domain.SeverityCritical {
			title = "Критический риск: клиент ждёт ответа"
		}
		return title, "Ответьте клиенту как можно скорее. Откройте Radar для подробностей."
	}
}

type riskOpenedData struct {
	RiskID        string              `json:"riskId"`
	OpportunityID string              `json:"opportunityId"`
	LocationID    string              `json:"locationId"`
	Type          riskdomain.Type     `json:"type"`
	Severity      riskdomain.Severity `json:"severity"`
}

// RiskOpenedEventHandler применяет политику уведомлений к открытому риску.
func RiskOpenedEventHandler(service Service) eventsapplication.Handler {
	return func(ctx context.Context, event eventsdomain.Event) error {
		var data riskOpenedData
		if json.Unmarshal(event.Data, &data) != nil || event.AggregateType != "risk" ||
			event.AggregateID == "" || data.RiskID != event.AggregateID || data.OpportunityID == "" || data.LocationID == "" ||
			!riskdomain.SupportedType(data.Type) {
			return jobsdomain.Permanent("INVALID_RISK_OPENED_EVENT", errors.New("некорректное событие открытия риска"))
		}
		err := service.ApplyRiskOpened(ctx, event.TenantID, RiskOpened{
			RiskID: data.RiskID, Type: domain.RiskType(data.Type), Severity: domain.Severity(data.Severity),
		})
		return classifyPolicyError(err, "NOTIFICATION_POLICY")
	}
}

// DigestJobHandler доставляет сводку слота, когда её проверка наступила.
func DigestJobHandler(service Service) jobsapplication.Handler {
	return func(ctx context.Context, job jobsdomain.Job) error {
		var payload digestPayload
		if json.Unmarshal(job.Payload, &payload) != nil || payload.UserID == "" || !domain.ValidSlot(payload.Slot) {
			return jobsdomain.Permanent("INVALID_JOB_PAYLOAD", errors.New("некорректное задание сводки"))
		}
		return classifyPolicyError(service.DeliverDigest(ctx, job.TenantID, payload.UserID, payload.Slot), "NOTIFICATION_DIGEST")
	}
}

// EscalationJobHandler проверяет реакцию на риск и при её отсутствии уведомляет владельца.
func EscalationJobHandler(service Service) jobsapplication.Handler {
	return func(ctx context.Context, job jobsdomain.Job) error {
		var payload escalationPayload
		if json.Unmarshal(job.Payload, &payload) != nil || payload.RiskID == "" {
			return jobsdomain.Permanent("INVALID_JOB_PAYLOAD", errors.New("некорректное задание эскалации"))
		}
		return classifyPolicyError(service.Escalate(ctx, job.TenantID, payload.RiskID), "NOTIFICATION_ESCALATION")
	}
}

func classifyPolicyError(err error, prefix string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, domain.ErrInvalid) || errors.Is(err, domain.ErrInvalidPreference) ||
		errors.Is(err, ErrNotFound) || errors.Is(err, jobsdomain.ErrInvalid) {
		return jobsdomain.Permanent(prefix+"_INVALID", err)
	}
	return jobsdomain.Retryable(prefix+"_TEMPORARY", err)
}

type CallbackAction string

const (
	CallbackOpen        CallbackAction = "OPEN_RISK"
	CallbackAcknowledge CallbackAction = "ACKNOWLEDGE"
	CallbackSnooze      CallbackAction = "SNOOZE"
)

type CallbackCommand struct {
	TenantID, UserID, NotificationID, IdempotencyKey, RiskID string
	Action                                                   CallbackAction
}

type CallbackExecutor interface {
	ExecuteSafeCallback(context.Context, CallbackCommand) error
}

// HandleCallback пропускает только явно разрешённый идемпотентный набор.
func HandleCallback(ctx context.Context, executor CallbackExecutor, command CallbackCommand) error {
	if executor == nil || command.TenantID == "" || command.UserID == "" || command.NotificationID == "" ||
		command.RiskID == "" || command.IdempotencyKey == "" {
		return ErrUnsafeCallback
	}
	switch command.Action {
	case CallbackOpen, CallbackAcknowledge, CallbackSnooze:
	default:
		return fmt.Errorf("%w: действие", ErrUnsafeCallback)
	}
	return executor.ExecuteSafeCallback(ctx, command)
}
