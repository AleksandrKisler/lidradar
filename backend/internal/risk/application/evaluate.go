// Package application согласует сценарии Risk через порты предметной области.
package application

import (
	"context"
	"errors"
	"time"

	"lidradar/backend/internal/risk/domain"
)

var ErrInvalidCheck = errors.New("некорректная проверка риска")

var (
	ErrStateNotFound   = errors.New("состояние риска не найдено")
	ErrStateIncomplete = errors.New("состояние риска не настроено полностью")
)

// StateReader заново читает каноническое состояние переписки и Opportunity.
// Поэтому отложенный worker не принимает устаревшую нагрузку задания за истину.
type StateReader interface {
	CurrentState(ctx context.Context, tenantID, opportunityID string) (domain.ConversationState, error)
}

type IDs interface{ NewID() (string, error) }

type Evaluator struct {
	repository domain.Repository
	states     StateReader
	policy     domain.Policy
	ids        IDs
	now        func() time.Time
	events     Invalidator
}

// WithInvalidator отправляет временный сигнал перечитать данные после
// долговечного изменения. Сохранение не зависит от подключения живого канала.
func (e Evaluator) WithInvalidator(events Invalidator) Evaluator { e.events = events; return e }

func NewEvaluator(repository domain.Repository, states StateReader, policy domain.Policy, ids IDs, now func() time.Time) Evaluator {
	return Evaluator{repository: repository, states: states, policy: policy, ids: ids, now: now}
}

// EvaluateDue перечитывает авторитетное состояние, применяет версионированное
// правило, атомарно устраняет повтор и закрывает утративший силу риск.
func (e Evaluator) EvaluateDue(ctx context.Context, tenantID, opportunityID string) (domain.Risk, bool, error) {
	if tenantID == "" || opportunityID == "" || e.repository == nil || e.states == nil || e.policy == nil || e.ids == nil || e.now == nil {
		return domain.Risk{}, false, ErrInvalidCheck
	}
	state, err := e.states.CurrentState(ctx, tenantID, opportunityID)
	if err != nil {
		return domain.Risk{}, false, err
	}
	// Другой tenant или объект является нарушением границы, а не безопасным
	// промахом кэша.
	if state.TenantID != tenantID || state.OpportunityID != opportunityID {
		return domain.Risk{}, false, ErrInvalidCheck
	}
	now := e.now().UTC()
	decision, err := e.policy.Evaluate(state, now)
	if err != nil {
		return domain.Risk{}, false, err
	}
	if decision.Finding == nil {
		if !decision.Resolve {
			return domain.Risk{}, false, nil
		}
		active, _, findErr := e.repository.FindActive(ctx, tenantID, opportunityID, e.policy.Type())
		if findErr != nil {
			return domain.Risk{}, false, findErr
		}
		resolved, resolveErr := e.repository.ResolveActive(ctx, tenantID, opportunityID, e.policy.Type(), now)
		if resolveErr == nil && resolved && e.events != nil {
			e.events.Publish(tenantID, "risk.resolved", active.ID)
		}
		err = resolveErr
		return domain.Risk{}, false, err
	}
	riskID, err := e.ids.NewID()
	if err != nil {
		return domain.Risk{}, false, err
	}
	risk, err := domain.NewNoResponse(riskID, *decision.Finding, now)
	if err != nil {
		return domain.Risk{}, false, err
	}
	stored, created, err := e.repository.UpsertActive(ctx, risk)
	if err == nil && e.events != nil {
		e.events.Publish(tenantID, "risk.changed", stored.ID)
	}
	return stored, created, err
}

// DueAt вычисляет срок долговечной проверки. Задание сохраняет tenant,
// идентификатор Opportunity и этот момент, но не снимок состояния.
func (e Evaluator) DueAt(state domain.ConversationState) (time.Time, error) {
	decision, err := e.policy.Evaluate(state, state.LastMeaningfulAt)
	return decision.DueAt, err
}
