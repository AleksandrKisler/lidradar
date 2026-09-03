// Package application выполняет команды платформенного администратора и
// отдаёт модели чтения для диагностики контура без SQL и SSH.
package application

import (
	"context"
	"errors"
	"strings"
	"time"

	"lidradar/backend/internal/admin/domain"
)

var (
	ErrForbidden = errors.New("нет права платформенного администратора")
	ErrNotFound  = errors.New("объект администрирования не найден")
	ErrInvalid   = errors.New("некорректная административная команда")
	ErrConflict  = errors.New("состояние объекта не допускает команду")
)

const (
	DefaultUsageDays = 30
	MaxUsageDays     = 366
	DefaultLimit     = 50
	MaxLimit         = 200
)

type IDs interface{ NewID() (string, error) }

type JobFilter struct {
	TenantID, Status, Type string
	Limit                  int
}

type RunFilter struct {
	TenantID, Status, ApplicationStatus string
	Limit                               int
}

type UserRecord struct {
	ID, Email, DisplayName string
}

// Store читает таблицы модулей только для чтения и выполняет команды над
// мёртвыми элементами очередей вместе с записью аудита в одной транзакции.
type Store interface {
	IsAdmin(ctx context.Context, userID string) (bool, error)
	Admins(ctx context.Context) ([]domain.Admin, error)
	UserByEmail(ctx context.Context, email string) (UserRecord, bool, error)
	GrantAdmin(ctx context.Context, admin domain.Admin, audit domain.AuditEntry) (domain.Admin, bool, error)
	RevokeAdmin(ctx context.Context, userID string, revokedBy *string, at time.Time, audit domain.AuditEntry) (bool, error)
	Organizations(ctx context.Context, now time.Time) ([]domain.OrganizationSummary, error)
	Connections(ctx context.Context) ([]domain.ConnectionHealth, error)
	QueueStats(ctx context.Context, now time.Time) (domain.QueueStats, error)
	Jobs(ctx context.Context, filter JobFilter) ([]domain.JobRecord, error)
	DeadLetters(ctx context.Context, limit int) (domain.DeadLetters, error)
	RetryJob(ctx context.Context, jobID string, at time.Time, audit domain.AuditEntry) (domain.JobRecord, error)
	DiscardJob(ctx context.Context, jobID, actorID string, at time.Time, audit domain.AuditEntry) (domain.JobRecord, error)
	ReplayEvent(ctx context.Context, eventID string, at time.Time, audit domain.AuditEntry) (domain.OutboxRecord, error)
	DiscardEvent(ctx context.Context, eventID, actorID string, at time.Time, audit domain.AuditEntry) (domain.OutboxRecord, error)
	RetryAIJob(ctx context.Context, jobID string, at time.Time, audit domain.AuditEntry) (domain.AIJobRecord, error)
	DiscardAIJob(ctx context.Context, jobID, actorID string, at time.Time, audit domain.AuditEntry) (domain.AIJobRecord, error)
	DiscardDelivery(ctx context.Context, deliveryID, actorID string, at time.Time, audit domain.AuditEntry) (domain.DeliveryRecord, error)
	AINodes(ctx context.Context) ([]domain.AINode, error)
	AIRuns(ctx context.Context, filter RunFilter) ([]domain.AIRun, error)
	ConversationSummary(ctx context.Context, tenantID, conversationID string) (domain.ConversationSummary, bool, error)
	Usage(ctx context.Context, from, to time.Time) ([]domain.TenantUsage, error)
	Trace(ctx context.Context, tenantID, messageID string) (domain.Trace, bool, error)
}

type Service struct {
	store Store
	ids   IDs
	now   func() time.Time
}

func NewService(store Store, ids IDs, now func() time.Time) Service {
	return Service{store: store, ids: ids, now: now}
}

func (service Service) ready() bool {
	return service.store != nil && service.ids != nil && service.now != nil
}

// require пропускает только активного платформенного администратора.
func (service Service) require(ctx context.Context, actor string) error {
	if !service.ready() || actor == "" {
		return ErrForbidden
	}
	admin, err := service.store.IsAdmin(ctx, actor)
	if err != nil {
		return err
	}
	if !admin {
		return ErrForbidden
	}
	return nil
}

func (service Service) audit(actor *string, source domain.AuditSource, operation, entityType, entityID string) (domain.AuditEntry, error) {
	id, err := service.ids.NewID()
	if err != nil {
		return domain.AuditEntry{}, err
	}
	entry := domain.AuditEntry{
		ID: id, ActorUserID: actor, Source: source, Operation: operation, EntityType: entityType, EntityID: entityID,
		Details: map[string]any{}, At: service.now().UTC(),
	}
	if err := entry.Validate(); err != nil {
		return domain.AuditEntry{}, ErrInvalid
	}
	return entry, nil
}

// Me сообщает, является ли пользователь активным администратором.
func (service Service) Me(ctx context.Context, actor string) (bool, error) {
	if !service.ready() || actor == "" {
		return false, ErrInvalid
	}
	return service.store.IsAdmin(ctx, actor)
}

func (service Service) Admins(ctx context.Context, actor string) ([]domain.Admin, error) {
	if err := service.require(ctx, actor); err != nil {
		return nil, err
	}
	return service.store.Admins(ctx)
}

// ListAdmins обслуживает CLI, у которого нет пользовательской сессии.
func (service Service) ListAdmins(ctx context.Context) ([]domain.Admin, error) {
	if !service.ready() {
		return nil, ErrInvalid
	}
	return service.store.Admins(ctx)
}

func (service Service) Grant(ctx context.Context, actor, email, note string) (domain.Admin, bool, error) {
	if err := service.require(ctx, actor); err != nil {
		return domain.Admin{}, false, err
	}
	return service.grant(ctx, &actor, domain.SourceAPI, email, note)
}

// GrantFromCLI выдаёт право без сессии: так появляется первый администратор.
func (service Service) GrantFromCLI(ctx context.Context, email, note string) (domain.Admin, bool, error) {
	if !service.ready() {
		return domain.Admin{}, false, ErrInvalid
	}
	return service.grant(ctx, nil, domain.SourceCLI, email, note)
}

func (service Service) grant(ctx context.Context, actor *string, source domain.AuditSource, email, note string) (domain.Admin, bool, error) {
	user, found, err := service.store.UserByEmail(ctx, strings.ToLower(strings.TrimSpace(email)))
	if err != nil {
		return domain.Admin{}, false, err
	}
	if !found {
		return domain.Admin{}, false, ErrNotFound
	}
	adminID, err := service.ids.NewID()
	if err != nil {
		return domain.Admin{}, false, err
	}
	admin, err := domain.NewAdmin(adminID, user.ID, actor, note, service.now())
	if err != nil {
		return domain.Admin{}, false, ErrInvalid
	}
	audit, err := service.audit(actor, source, "PLATFORM_ADMIN_GRANTED", "PLATFORM_ADMIN", admin.ID)
	if err != nil {
		return domain.Admin{}, false, err
	}
	stored, created, err := service.store.GrantAdmin(ctx, admin, audit)
	if err != nil {
		return domain.Admin{}, false, err
	}
	stored.Email, stored.DisplayName = user.Email, user.DisplayName
	return stored, created, nil
}

func (service Service) Revoke(ctx context.Context, actor, userID string) (bool, error) {
	if err := service.require(ctx, actor); err != nil {
		return false, err
	}
	if strings.TrimSpace(userID) == "" {
		return false, ErrInvalid
	}
	return service.revoke(ctx, &actor, domain.SourceAPI, userID)
}

func (service Service) RevokeFromCLI(ctx context.Context, email string) (bool, error) {
	if !service.ready() {
		return false, ErrInvalid
	}
	user, found, err := service.store.UserByEmail(ctx, strings.ToLower(strings.TrimSpace(email)))
	if err != nil {
		return false, err
	}
	if !found {
		return false, ErrNotFound
	}
	return service.revoke(ctx, nil, domain.SourceCLI, user.ID)
}

func (service Service) revoke(ctx context.Context, actor *string, source domain.AuditSource, userID string) (bool, error) {
	audit, err := service.audit(actor, source, "PLATFORM_ADMIN_REVOKED", "PLATFORM_ADMIN", userID)
	if err != nil {
		return false, err
	}
	return service.store.RevokeAdmin(ctx, userID, actor, service.now().UTC(), audit)
}

func (service Service) Organizations(ctx context.Context, actor string) ([]domain.OrganizationSummary, error) {
	if err := service.require(ctx, actor); err != nil {
		return nil, err
	}
	return service.store.Organizations(ctx, service.now().UTC())
}

func (service Service) Connections(ctx context.Context, actor string) ([]domain.ConnectionHealth, error) {
	if err := service.require(ctx, actor); err != nil {
		return nil, err
	}
	return service.store.Connections(ctx)
}

func (service Service) Queue(ctx context.Context, actor string) (domain.QueueStats, error) {
	if err := service.require(ctx, actor); err != nil {
		return domain.QueueStats{}, err
	}
	return service.store.QueueStats(ctx, service.now().UTC())
}

func clampLimit(limit int) int {
	if limit <= 0 {
		return DefaultLimit
	}
	if limit > MaxLimit {
		return MaxLimit
	}
	return limit
}

func (service Service) Jobs(ctx context.Context, actor string, filter JobFilter) ([]domain.JobRecord, error) {
	if err := service.require(ctx, actor); err != nil {
		return nil, err
	}
	filter.Limit = clampLimit(filter.Limit)
	return service.store.Jobs(ctx, filter)
}

func (service Service) DeadLetters(ctx context.Context, actor string, limit int) (domain.DeadLetters, error) {
	if err := service.require(ctx, actor); err != nil {
		return domain.DeadLetters{}, err
	}
	return service.store.DeadLetters(ctx, clampLimit(limit))
}

func (service Service) RetryJob(ctx context.Context, actor, jobID string) (domain.JobRecord, error) {
	if err := service.require(ctx, actor); err != nil {
		return domain.JobRecord{}, err
	}
	audit, err := service.audit(&actor, domain.SourceAPI, "ADMIN_JOB_RETRIED", "JOB", jobID)
	if err != nil {
		return domain.JobRecord{}, err
	}
	return service.store.RetryJob(ctx, jobID, audit.At, audit)
}

func (service Service) DiscardJob(ctx context.Context, actor, jobID string) (domain.JobRecord, error) {
	if err := service.require(ctx, actor); err != nil {
		return domain.JobRecord{}, err
	}
	audit, err := service.audit(&actor, domain.SourceAPI, "ADMIN_JOB_DISCARDED", "JOB", jobID)
	if err != nil {
		return domain.JobRecord{}, err
	}
	return service.store.DiscardJob(ctx, jobID, actor, audit.At, audit)
}

func (service Service) ReplayEvent(ctx context.Context, actor, eventID string) (domain.OutboxRecord, error) {
	if err := service.require(ctx, actor); err != nil {
		return domain.OutboxRecord{}, err
	}
	audit, err := service.audit(&actor, domain.SourceAPI, "ADMIN_EVENT_REPLAYED", "OUTBOX_EVENT", eventID)
	if err != nil {
		return domain.OutboxRecord{}, err
	}
	return service.store.ReplayEvent(ctx, eventID, audit.At, audit)
}

func (service Service) DiscardEvent(ctx context.Context, actor, eventID string) (domain.OutboxRecord, error) {
	if err := service.require(ctx, actor); err != nil {
		return domain.OutboxRecord{}, err
	}
	audit, err := service.audit(&actor, domain.SourceAPI, "ADMIN_EVENT_DISCARDED", "OUTBOX_EVENT", eventID)
	if err != nil {
		return domain.OutboxRecord{}, err
	}
	return service.store.DiscardEvent(ctx, eventID, actor, audit.At, audit)
}

func (service Service) RetryAIJob(ctx context.Context, actor, jobID string) (domain.AIJobRecord, error) {
	if err := service.require(ctx, actor); err != nil {
		return domain.AIJobRecord{}, err
	}
	audit, err := service.audit(&actor, domain.SourceAPI, "ADMIN_AI_JOB_RETRIED", "AI_JOB", jobID)
	if err != nil {
		return domain.AIJobRecord{}, err
	}
	return service.store.RetryAIJob(ctx, jobID, audit.At, audit)
}

func (service Service) DiscardAIJob(ctx context.Context, actor, jobID string) (domain.AIJobRecord, error) {
	if err := service.require(ctx, actor); err != nil {
		return domain.AIJobRecord{}, err
	}
	audit, err := service.audit(&actor, domain.SourceAPI, "ADMIN_AI_JOB_DISCARDED", "AI_JOB", jobID)
	if err != nil {
		return domain.AIJobRecord{}, err
	}
	return service.store.DiscardAIJob(ctx, jobID, actor, audit.At, audit)
}

func (service Service) DiscardDelivery(ctx context.Context, actor, deliveryID string) (domain.DeliveryRecord, error) {
	if err := service.require(ctx, actor); err != nil {
		return domain.DeliveryRecord{}, err
	}
	audit, err := service.audit(&actor, domain.SourceAPI, "ADMIN_DELIVERY_DISCARDED", "NOTIFICATION_DELIVERY", deliveryID)
	if err != nil {
		return domain.DeliveryRecord{}, err
	}
	return service.store.DiscardDelivery(ctx, deliveryID, actor, audit.At, audit)
}

func (service Service) AINodes(ctx context.Context, actor string) ([]domain.AINode, error) {
	if err := service.require(ctx, actor); err != nil {
		return nil, err
	}
	return service.store.AINodes(ctx)
}

func (service Service) AIRuns(ctx context.Context, actor string, filter RunFilter) ([]domain.AIRun, error) {
	if err := service.require(ctx, actor); err != nil {
		return nil, err
	}
	filter.Limit = clampLimit(filter.Limit)
	return service.store.AIRuns(ctx, filter)
}

func (service Service) Summary(ctx context.Context, actor, tenantID, conversationID string) (domain.ConversationSummary, error) {
	if err := service.require(ctx, actor); err != nil {
		return domain.ConversationSummary{}, err
	}
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(conversationID) == "" {
		return domain.ConversationSummary{}, ErrInvalid
	}
	summary, found, err := service.store.ConversationSummary(ctx, tenantID, conversationID)
	if err != nil {
		return domain.ConversationSummary{}, err
	}
	if !found {
		return domain.ConversationSummary{}, ErrNotFound
	}
	return summary, nil
}

// Usage считает потребление за окно [from, to) в UTC; по умолчанию 30 дней.
func (service Service) Usage(ctx context.Context, actor string, from, to time.Time) (domain.UsageReport, error) {
	if err := service.require(ctx, actor); err != nil {
		return domain.UsageReport{}, err
	}
	now := service.now().UTC()
	if to.IsZero() {
		to = now
	}
	if from.IsZero() {
		from = to.AddDate(0, 0, -DefaultUsageDays)
	}
	from, to = from.UTC(), to.UTC()
	if !from.Before(to) || to.After(from.AddDate(0, 0, MaxUsageDays)) {
		return domain.UsageReport{}, ErrInvalid
	}
	tenants, err := service.store.Usage(ctx, from, to)
	if err != nil {
		return domain.UsageReport{}, err
	}
	if tenants == nil {
		tenants = []domain.TenantUsage{}
	}
	return domain.UsageReport{From: from, To: to, Tenants: tenants}, nil
}

func (service Service) Trace(ctx context.Context, actor, tenantID, messageID string) (domain.Trace, error) {
	if err := service.require(ctx, actor); err != nil {
		return domain.Trace{}, err
	}
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(messageID) == "" {
		return domain.Trace{}, ErrInvalid
	}
	trace, found, err := service.store.Trace(ctx, tenantID, messageID)
	if err != nil {
		return domain.Trace{}, err
	}
	if !found {
		return domain.Trace{}, ErrNotFound
	}
	return trace, nil
}
