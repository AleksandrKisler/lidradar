// Package application координирует коммерческие возможности и ручные переходы.
package application

import (
	"context"
	"errors"
	"strings"
	"time"

	"lidradar/backend/internal/opportunity/domain"
)

var (
	ErrInvalid           = errors.New("некорректный запрос возможности")
	ErrForbidden         = errors.New("нет разрешения на управление возможностью")
	ErrNotFound          = errors.New("возможность не найдена")
	ErrConflict          = errors.New("конфликт возможности")
	ErrInvalidTransition = errors.New("недопустимый переход этапа")
)

const PermissionManage = "opportunity.manage"

type Authorizer interface {
	Allowed(context.Context, string, string, string) (bool, error)
}

type IDs interface{ NewID() (string, error) }

type Service struct {
	repository domain.Repository
	authorizer Authorizer
	ids        IDs
	now        func() time.Time
}

func NewService(repository domain.Repository, authorizer Authorizer, ids IDs, now func() time.Time) Service {
	return Service{repository: repository, authorizer: authorizer, ids: ids, now: now}
}

func (service Service) Detail(
	ctx context.Context,
	actorID, tenantID, opportunityID string,
) (domain.Detail, error) {
	if err := service.requireManage(ctx, actorID, tenantID); err != nil {
		return domain.Detail{}, err
	}
	if strings.TrimSpace(opportunityID) == "" {
		return domain.Detail{}, ErrInvalid
	}
	detail, found, err := service.repository.Detail(ctx, tenantID, opportunityID)
	if err != nil {
		return domain.Detail{}, mapDomainError(err)
	}
	if !found {
		return domain.Detail{}, ErrNotFound
	}
	if detail.History == nil {
		detail.History = []domain.StageHistory{}
	}
	return detail, nil
}

// ChangeStage выполняет ручной переход и всегда записывает источник USER.
func (service Service) ChangeStage(
	ctx context.Context,
	actorID, tenantID, opportunityID string,
	stage domain.Stage,
) (domain.Opportunity, bool, error) {
	if err := service.requireManage(ctx, actorID, tenantID); err != nil {
		return domain.Opportunity{}, false, err
	}
	if strings.TrimSpace(opportunityID) == "" || !stage.Valid() || service.ids == nil || service.now == nil {
		return domain.Opportunity{}, false, ErrInvalid
	}
	historyID, err := service.ids.NewID()
	if err != nil {
		return domain.Opportunity{}, false, err
	}
	actor := actorID
	opportunity, changed, err := service.repository.Transition(ctx, domain.TransitionCommand{
		TenantID: tenantID, OpportunityID: opportunityID, HistoryID: historyID,
		ToStage: stage, Source: domain.SourceUser, ActorUserID: &actor, At: service.now().UTC(),
	})
	if err != nil {
		return domain.Opportunity{}, false, mapDomainError(err)
	}
	return opportunity, changed, nil
}

// MarkNotALead закрывает активную сделку как LOST по обратной связи «не лид»
// (LR-BE-2103): ARCHIVED достижим только из WON/LOST, а уже закрытая сделка
// не меняется. Переход записывается с источником USER и автором вердикта.
func (service Service) MarkNotALead(ctx context.Context, actorID, tenantID, opportunityID string) error {
	if err := service.requireManage(ctx, actorID, tenantID); err != nil {
		return err
	}
	if strings.TrimSpace(opportunityID) == "" {
		return ErrInvalid
	}
	detail, found, err := service.repository.Detail(ctx, tenantID, opportunityID)
	if err != nil {
		return mapDomainError(err)
	}
	if !found {
		return ErrNotFound
	}
	if !detail.Opportunity.Stage.Active() {
		return nil
	}
	_, _, err = service.ChangeStage(ctx, actorID, tenantID, opportunityID, domain.StageLost)
	return err
}

func (service Service) requireManage(ctx context.Context, actorID, tenantID string) error {
	if service.repository == nil || service.authorizer == nil || actorID == "" || tenantID == "" {
		return ErrForbidden
	}
	allowed, err := service.authorizer.Allowed(ctx, actorID, tenantID, PermissionManage)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrForbidden
	}
	return nil
}

func mapDomainError(err error) error {
	switch {
	case errors.Is(err, domain.ErrInvalid):
		return ErrInvalid
	case errors.Is(err, domain.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, domain.ErrConflict):
		return ErrConflict
	case errors.Is(err, domain.ErrInvalidTransition):
		return ErrInvalidTransition
	default:
		return err
	}
}
