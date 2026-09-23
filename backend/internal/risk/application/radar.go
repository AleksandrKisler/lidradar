package application

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	auditapplication "lidradar/backend/internal/audit/application"
	"lidradar/backend/internal/risk/domain"
)

var (
	ErrForbidden      = errors.New("нет разрешения на работу с рисками")
	ErrNotFound       = errors.New("риск не найден")
	ErrInvalidCommand = errors.New("некорректная команда риска")
)

const (
	PermissionRead   = "risks.read"
	PermissionManage = "risks.manage"
)

// Authorizer преобразует участие в организации в именованные разрешения.
// Конкретные роли не проникают в модуль Risk.
type Authorizer interface {
	Allowed(ctx context.Context, actorID, tenantID, permission string) (bool, error)
}

type ListQuery struct {
	Filters
	Limit int
	After string
}

// Filters — общий набор фильтров списка и сводки Radar. Statuses ограничивает
// выборку перечисленными статусами; пустой список означает все статусы.
// Курсор списка привязан к нормализованному набору фильтров.
type Filters struct {
	LocationID string
	Severity   domain.Severity
	RiskType   domain.Type
	Statuses   []domain.Status
}

// Normalized возвращает фильтры с отсортированными уникальными статусами:
// одинаковые наборы дают одинаковый ключ курсора независимо от порядка.
func (filters Filters) Normalized() Filters {
	if len(filters.Statuses) == 0 {
		filters.Statuses = nil
		return filters
	}
	seen := make(map[domain.Status]struct{}, len(filters.Statuses))
	statuses := make([]domain.Status, 0, len(filters.Statuses))
	for _, status := range filters.Statuses {
		if _, duplicate := seen[status]; duplicate {
			continue
		}
		seen[status] = struct{}{}
		statuses = append(statuses, status)
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i] < statuses[j] })
	filters.Statuses = statuses
	return filters
}

// StatusKey — каноническое текстовое представление набора статусов.
func (filters Filters) StatusKey() string {
	normalized := filters.Normalized()
	parts := make([]string, 0, len(normalized.Statuses))
	for _, status := range normalized.Statuses {
		parts = append(parts, string(status))
	}
	return strings.Join(parts, ",")
}

// Matches сообщает, проходит ли риск фильтры (без учёта курсора).
func (filters Filters) Matches(risk domain.Risk) bool {
	if filters.LocationID != "" && risk.LocationID != filters.LocationID {
		return false
	}
	if filters.Severity != "" && risk.Severity != filters.Severity {
		return false
	}
	if filters.RiskType != "" && risk.Type != filters.RiskType {
		return false
	}
	if len(filters.Statuses) == 0 {
		return true
	}
	for _, status := range filters.Statuses {
		if risk.Status == status {
			return true
		}
	}
	return false
}

// Opportunity — снимок сделки для карточки риска. ServiceID дублирует ссылку
// сделки на услугу каталога; сама услуга приходит в Detail.Service.
type Opportunity struct {
	ID               string  `json:"id"`
	Stage            string  `json:"stage"`
	LocationID       string  `json:"locationId"`
	ServiceID        *string `json:"serviceId"`
	PotentialRevenue *string `json:"potentialRevenue"`
	Currency         string  `json:"currency"`
}

// MessagePreview — последнее сообщение переписки без вложений и метаданных:
// Preview — первые 140 символов текста с нормализованными пробелами либо null
// для сообщений без текста.
type MessagePreview struct {
	ID        string    `json:"id"`
	Direction string    `json:"direction"`
	Type      string    `json:"type"`
	Preview   *string   `json:"preview"`
	SentAt    time.Time `json:"sentAt"`
}

type Conversation struct {
	ID          string          `json:"id"`
	ContactID   string          `json:"contactId"`
	LastMessage *MessagePreview `json:"lastMessage"`
}

// Contact — отображаемое имя без телефона и почты: карточка риска не
// раскрывает контактные данные, они доступны в деталях переписки.
type Contact struct {
	ID          string  `json:"id"`
	DisplayName *string `json:"displayName"`
}

// Service — снимок услуги каталога, видимый любому читателю Radar: имя
// услуги нужно менеджеру для контекста риска, а управление каталогом остаётся
// отдельным правом.
type Service struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Active bool   `json:"active"`
}

// Channel — канал, из которого пришла переписка.
type Channel struct {
	ConnectionID string `json:"connectionId"`
	Provider     string `json:"provider"`
	Name         string `json:"name"`
	Status       string `json:"status"`
}

// ExternalLink — безопасная ссылка на собеседника во внешнем клиенте. URL и
// Kind заполняются только сервером по разрешённой схеме; при отсутствии
// ссылки UnavailableReason объясняет причину.
type ExternalLink struct {
	URL               *string `json:"url"`
	Kind              *string `json:"kind"`
	UnavailableReason *string `json:"unavailableReason"`
}

type Recommendation struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}
type Action struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	CreatedAt time.Time `json:"createdAt"`
}
type Outcome struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	CreatedAt time.Time `json:"createdAt"`
}
type Revenue struct {
	Currency           string `json:"currency"`
	Potential          string `json:"potential"`
	ConfirmedRecovered string `json:"confirmedRecovered"`
}

// Detail — модель чтения Radar для списка и карточки риска. Все связанные
// объекты присутствуют в JSON всегда: null означает, что владеющий модуль ещё
// не создал запись (рекомендация, исход, выручка) либо связь отсутствует
// (услуга у сделки, последнее сообщение). Список и карточка собираются без
// запросов на каждую строку (ADR 0044).
type Detail struct {
	Risk           domain.Risk     `json:"risk"`
	Opportunity    *Opportunity    `json:"opportunity"`
	Conversation   *Conversation   `json:"conversation"`
	Contact        *Contact        `json:"contact"`
	Service        *Service        `json:"service"`
	Channel        *Channel        `json:"channel"`
	ExternalLink   ExternalLink    `json:"externalLink"`
	Recommendation *Recommendation `json:"recommendation"`
	Actions        []Action        `json:"actions"`
	Outcome        *Outcome        `json:"outcome"`
	Revenue        *Revenue        `json:"revenue"`
}

type Page struct {
	Items      []Detail
	NextCursor string
}
type Summary struct {
	OpenRisks                 int    `json:"openRisks"`
	CriticalRisks             int    `json:"criticalRisks"`
	PotentialRevenue          string `json:"potentialRevenue"`
	ConfirmedRecoveredRevenue string `json:"confirmedRecoveredRevenue"`
}

type Mutation struct {
	Risk    domain.Risk
	Found   bool
	Changed bool
}

// RadarStore описывает PostgreSQL-чтение и команды Radar. Каждая операция,
// включая поиск по ID риска, явно ограничена организацией.
type RadarStore interface {
	List(ctx context.Context, tenantID string, query ListQuery) (Page, error)
	Get(ctx context.Context, tenantID, riskID string) (Detail, bool, error)
	Summary(ctx context.Context, tenantID string, filters Filters) (Summary, error)
	Acknowledge(ctx context.Context, tenantID, riskID string, at time.Time) (Mutation, error)
	Resolve(ctx context.Context, tenantID, riskID string, at time.Time) (Mutation, error)
}

type Invalidator interface {
	Publish(tenantID, eventType, resourceID string)
}

type Radar struct {
	store   RadarStore
	auth    Authorizer
	events  Invalidator
	now     func() time.Time
	auditor auditapplication.Recorder
}

// WithAuditor включает аудит подтверждения и закрытия риска (ТЗ §65).
func (s Radar) WithAuditor(auditor auditapplication.Recorder) Radar {
	s.auditor = auditor
	return s
}

func NewRadar(store RadarStore, auth Authorizer, events Invalidator, now func() time.Time) Radar {
	return Radar{store: store, auth: auth, events: events, now: now}
}

func (s Radar) permit(ctx context.Context, actor, tenant, permission string) error {
	if actor == "" || tenant == "" || s.auth == nil {
		return ErrForbidden
	}
	ok, err := s.auth.Allowed(ctx, actor, tenant, permission)
	if err != nil {
		return err
	}
	if !ok {
		return ErrForbidden
	}
	return nil
}

func (s Radar) CanRead(ctx context.Context, actor, tenant string) error {
	return s.permit(ctx, actor, tenant, PermissionRead)
}

// List отдаёт страницу рисков в серверном порядке приоритета. Денежные поля
// модели чтения видны любому обладателю risks.read: менеджер расставляет
// приоритеты по потенциалу сделки, а общие итоги организации остаются за
// revenue.read и analytics.read (ADR 0044).
func (s Radar) List(ctx context.Context, actor, tenant string, q ListQuery) (Page, error) {
	if err := s.permit(ctx, actor, tenant, PermissionRead); err != nil {
		return Page{}, err
	}
	if q.Limit == 0 {
		q.Limit = 50
	}
	if q.Limit < 1 || q.Limit > 100 {
		return Page{}, ErrInvalidCommand
	}
	if !validFilters(q.Filters) {
		return Page{}, ErrInvalidCommand
	}
	if s.store == nil {
		return Page{}, ErrInvalidCommand
	}
	q.Filters = q.Filters.Normalized()
	return s.store.List(ctx, tenant, q)
}
func (s Radar) Get(ctx context.Context, actor, tenant, id string) (Detail, error) {
	if id == "" || s.store == nil {
		return Detail{}, ErrInvalidCommand
	}
	if err := s.permit(ctx, actor, tenant, PermissionRead); err != nil {
		return Detail{}, err
	}
	d, ok, err := s.store.Get(ctx, tenant, id)
	if err != nil {
		return Detail{}, err
	}
	if !ok {
		return Detail{}, ErrNotFound
	}
	return d, nil
}
func (s Radar) Summary(ctx context.Context, actor, tenant string, filters Filters) (Summary, error) {
	if err := s.permit(ctx, actor, tenant, PermissionRead); err != nil {
		return Summary{}, err
	}
	if s.store == nil || !validFilters(filters) {
		return Summary{}, ErrInvalidCommand
	}
	return s.store.Summary(ctx, tenant, filters.Normalized())
}
func (s Radar) Acknowledge(ctx context.Context, actor, tenant, id string) (domain.Risk, error) {
	if s.store == nil {
		return domain.Risk{}, ErrInvalidCommand
	}
	return s.change(ctx, actor, tenant, id, "risk.acknowledged", s.store.Acknowledge)
}
func (s Radar) Resolve(ctx context.Context, actor, tenant, id string) (domain.Risk, error) {
	if s.store == nil {
		return domain.Risk{}, ErrInvalidCommand
	}
	return s.change(ctx, actor, tenant, id, "risk.resolved", s.store.Resolve)
}
func (s Radar) change(ctx context.Context, actor, tenant, id, event string, fn func(context.Context, string, string, time.Time) (Mutation, error)) (domain.Risk, error) {
	if id == "" || s.store == nil || s.now == nil {
		return domain.Risk{}, ErrInvalidCommand
	}
	if err := s.permit(ctx, actor, tenant, PermissionManage); err != nil {
		return domain.Risk{}, err
	}
	mutation, err := fn(ctx, tenant, id, s.now().UTC())
	if err != nil {
		return domain.Risk{}, err
	}
	if !mutation.Found {
		return domain.Risk{}, ErrNotFound
	}
	if mutation.Changed && s.events != nil {
		s.events.Publish(tenant, event, id)
	}
	if mutation.Changed && s.auditor != nil {
		operation := "RISK_RESOLVED"
		if event == "risk.acknowledged" {
			operation = "RISK_ACKNOWLEDGED"
		}
		if err := s.auditor.Tenant(ctx, auditapplication.TenantEntry(tenant, actor, operation, "RISK", id, s.now())); err != nil {
			return domain.Risk{}, err
		}
	}
	return mutation.Risk, nil
}

func validFilters(filters Filters) bool {
	if filters.LocationID != "" && strings.TrimSpace(filters.LocationID) != filters.LocationID {
		return false
	}
	if filters.Severity != "" && filters.Severity != domain.SeverityLow && filters.Severity != domain.SeverityMedium &&
		filters.Severity != domain.SeverityHigh && filters.Severity != domain.SeverityCritical {
		return false
	}
	for _, status := range filters.Statuses {
		if !status.Valid() {
			return false
		}
	}
	return filters.RiskType == "" || domain.SupportedType(filters.RiskType)
}
