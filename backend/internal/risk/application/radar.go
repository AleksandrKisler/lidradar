package application

import (
	"context"
	"errors"
	"time"

	"lidradar/backend/internal/risk/domain"
)

var (
	ErrForbidden      = errors.New("forbidden")
	ErrNotFound       = errors.New("risk not found")
	ErrInvalidCommand = errors.New("invalid risk command")
)

const (
	PermissionRead   = "risks.read"
	PermissionManage = "risks.manage"
)

// Authorizer resolves tenant membership into named permissions. Roles never
// leak into the Risk module.
type Authorizer interface {
	Allowed(ctx context.Context, actorID, tenantID, permission string) (bool, error)
}

type ListQuery struct {
	Status domain.Status
	Limit  int
	After  string
}

type Opportunity struct {
	ID               string `json:"id"`
	Stage            string `json:"stage"`
	LocationID       string `json:"locationId"`
	PotentialRevenue string `json:"potentialRevenue"`
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

// Detail is the Radar read model. Optional related records are omitted until
// their owning modules have produced them.
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
	OpenRisks                 int
	CriticalRisks             int
	PotentialRevenue          string
	ConfirmedRecoveredRevenue string
}

// RadarStore owns the PostgreSQL-backed read/command operations needed by the
// use case. Every operation is tenant-scoped, including lookups by risk ID.
type RadarStore interface {
	List(ctx context.Context, tenantID string, query ListQuery) (Page, error)
	Get(ctx context.Context, tenantID, riskID string) (Detail, bool, error)
	Summary(ctx context.Context, tenantID string) (Summary, error)
	Acknowledge(ctx context.Context, tenantID, riskID string, at time.Time) (domain.Risk, bool, error)
	Resolve(ctx context.Context, tenantID, riskID string, at time.Time) (domain.Risk, bool, error)
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
	if q.Status != "" && q.Status != domain.StatusOpen && q.Status != domain.StatusAcknowledged && q.Status != domain.StatusActed && q.Status != domain.StatusResolved {
		return Page{}, ErrInvalidCommand
	}
	if s.store == nil {
		return Page{}, ErrInvalidCommand
	}
	return s.store.List(ctx, tenant, q)
}
func (s Radar) Get(ctx context.Context, actor, tenant, id string) (Detail, error) {
	if id == "" {
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
func (s Radar) Summary(ctx context.Context, actor, tenant string) (Summary, error) {
	if err := s.permit(ctx, actor, tenant, PermissionRead); err != nil {
		return Summary{}, err
	}
	return s.store.Summary(ctx, tenant)
}
func (s Radar) Acknowledge(ctx context.Context, actor, tenant, id string) (domain.Risk, error) {
	return s.change(ctx, actor, tenant, id, "risk.acknowledged", s.store.Acknowledge)
}
func (s Radar) Resolve(ctx context.Context, actor, tenant, id string) (domain.Risk, error) {
	return s.change(ctx, actor, tenant, id, "risk.resolved", s.store.Resolve)
}
func (s Radar) change(ctx context.Context, actor, tenant, id, event string, fn func(context.Context, string, string, time.Time) (domain.Risk, bool, error)) (domain.Risk, error) {
	if id == "" || s.store == nil || s.now == nil {
		return domain.Risk{}, ErrInvalidCommand
	}
	if err := s.permit(ctx, actor, tenant, PermissionManage); err != nil {
		return domain.Risk{}, err
	}
	r, ok, err := fn(ctx, tenant, id, s.now().UTC())
	if err != nil {
		return domain.Risk{}, err
	}
	if !ok {
		return domain.Risk{}, ErrNotFound
	}
	if s.events != nil {
		s.events.Publish(tenant, event, id)
	}
	return r, nil
}
