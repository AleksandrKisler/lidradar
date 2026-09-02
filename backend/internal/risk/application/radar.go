package application

import (
	"context"
	"errors"
	"strings"
	"time"

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
	Status domain.Status
	Limit  int
	After  string
}

type Filters struct {
	LocationID string
	Severity   domain.Severity
	RiskType   domain.Type
}

type Opportunity struct {
	ID               string  `json:"id"`
	Stage            string  `json:"stage"`
	LocationID       string  `json:"locationId"`
	PotentialRevenue *string `json:"potentialRevenue"`
	Currency         string  `json:"currency"`
}
type Conversation struct {
	ID        string `json:"id"`
	ContactID string `json:"contactId"`
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

// Detail — модель чтения Radar. Необязательные связанные записи отсутствуют,
// пока владеющие ими модули их не создали.
type Detail struct {
	Risk           domain.Risk     `json:"risk"`
	Opportunity    *Opportunity    `json:"opportunity,omitempty"`
	Conversation   *Conversation   `json:"conversation,omitempty"`
	Recommendation *Recommendation `json:"recommendation,omitempty"`
	Actions        []Action        `json:"actions"`
	Outcome        *Outcome        `json:"outcome,omitempty"`
	Revenue        *Revenue        `json:"revenue,omitempty"`
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
	store  RadarStore
	auth   Authorizer
	events Invalidator
	now    func() time.Time
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
	if !validStatus(q.Status) || !validFilters(q.Filters) {
		return Page{}, ErrInvalidCommand
	}
	if s.store == nil {
		return Page{}, ErrInvalidCommand
	}
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
	return s.store.Summary(ctx, tenant, filters)
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
	return filters.RiskType == "" || domain.SupportedType(filters.RiskType)
}

func validStatus(status domain.Status) bool {
	switch status {
	case "", domain.StatusOpen, domain.StatusAcknowledged, domain.StatusActed, domain.StatusResolved,
		domain.StatusFalsePositive, domain.StatusIgnored, domain.StatusExpired:
		return true
	default:
		return false
	}
}
