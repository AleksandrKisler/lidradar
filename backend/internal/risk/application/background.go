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
	"lidradar/backend/internal/risk/domain"
)

const (
	ConversationChangedEventType = "conversation.changed.v1"
	OpportunityCreatedEventType  = "opportunity.created.v1"
	OpportunityStageEventType    = "opportunity.stage_changed.v1"
	RefreshJobType               = "risk.refresh-no-response-plan.v1"
	NoResponseCheckType          = "NO_RESPONSE_DUE"
	NoResponseEvaluationJobType  = "risk.evaluate-no-response.v1"
	BookingCheckType             = "BOOKING_NOT_CONFIRMED_DUE"
	BookingEvaluationJobType     = "risk.evaluate-booking-not-confirmed.v1"
	PromiseCheckType             = "PROMISE_NOT_FULFILLED_DUE"
	PromiseEvaluationJobType     = "risk.evaluate-promise-not-fulfilled.v1"
	PriceCheckType               = "CUSTOMER_SILENT_AFTER_PRICE_DUE"
	PriceEvaluationJobType       = "risk.evaluate-customer-silent-after-price.v1"
	FollowUpCheckType            = "FOLLOW_UP_CANDIDATE_DUE"
	FollowUpEvaluationJobType    = "risk.evaluate-follow-up-candidate.v1"
)

type refreshPayload struct {
	ConversationID string `json:"conversationId,omitempty"`
	OpportunityID  string `json:"opportunityId,omitempty"`
}

type conversationEventData struct {
	ConversationID string `json:"conversationId"`
	MessageID      string `json:"messageId"`
	Revision       int64  `json:"revision"`
}

type analysisAppliedEventData struct {
	ConversationID           string `json:"conversationId"`
	RunID                    string `json:"runId"`
	BaseConversationRevision int64  `json:"baseConversationRevision"`
	AnalysisThroughMessageID string `json:"analysisThroughMessageId"`
}

type CheckScheduler interface {
	Schedule(context.Context, jobsdomain.ScheduledCheck) (jobsdomain.ScheduledCheck, bool, error)
}

type OpportunityLocator interface {
	ActiveOpportunityByConversation(context.Context, string, string) (string, bool, error)
}

type Planner struct {
	locator   OpportunityLocator
	states    StateReader
	scheduler CheckScheduler
	evaluator Evaluator
	policy    domain.Policy
	ids       IDs
	now       func() time.Time
}

func NewPlanner(
	locator OpportunityLocator,
	states StateReader,
	scheduler CheckScheduler,
	evaluator Evaluator,
	policy domain.Policy,
	ids IDs,
	now func() time.Time,
) Planner {
	return Planner{locator: locator, states: states, scheduler: scheduler, evaluator: evaluator, policy: policy, ids: ids, now: now}
}

func (planner Planner) RefreshConversation(ctx context.Context, tenantID, conversationID string) error {
	if planner.locator == nil || tenantID == "" || conversationID == "" {
		return ErrInvalidCheck
	}
	opportunityID, found, err := planner.locator.ActiveOpportunityByConversation(ctx, tenantID, conversationID)
	if err != nil || !found {
		return err
	}
	return planner.RefreshOpportunity(ctx, tenantID, opportunityID)
}

// RefreshOpportunity перечитывает состояние. Для ответа/закрытой сделки риск
// закрывается сразу; для входящего сообщения сохраняется только проверка с ID.
func (planner Planner) RefreshOpportunity(ctx context.Context, tenantID, opportunityID string) error {
	if planner.states == nil || planner.scheduler == nil || planner.policy == nil || planner.ids == nil ||
		planner.now == nil || tenantID == "" || opportunityID == "" {
		return ErrInvalidCheck
	}
	state, err := planner.states.CurrentState(ctx, tenantID, opportunityID)
	if err != nil {
		return err
	}
	if state.TenantID != tenantID || state.OpportunityID != opportunityID {
		return ErrInvalidCheck
	}
	decision, err := planner.policy.Evaluate(state, planner.now().UTC())
	if err != nil {
		return err
	}
	// Закрытие уже активного риска и планирование новой проверки не исключают
	// друг друга: новое обещание после выполненного старого закрывает прежний
	// риск и получает собственный срок.
	if decision.Resolve {
		if _, _, err := planner.evaluator.EvaluateDue(ctx, tenantID, opportunityID); err != nil {
			return err
		}
	}
	if decision.TriggerMessageID == "" && decision.Finding == nil {
		decision.TriggerMessageID = state.LastMeaningfulID
	}
	if err := planner.schedule(ctx, tenantID, opportunityID, decision, decision.DueAt, ""); err != nil {
		return err
	}
	return planner.scheduleNext(ctx, tenantID, opportunityID, decision)
}

// Evaluate выполняет наступившую проверку и планирует следующую проверку того
// же основания, если правило назначило её (например эскалацию важности).
func (planner Planner) Evaluate(ctx context.Context, tenantID, opportunityID string) error {
	if planner.scheduler == nil || planner.policy == nil || planner.ids == nil || planner.now == nil {
		return ErrInvalidCheck
	}
	decision, _, _, err := planner.evaluator.Apply(ctx, tenantID, opportunityID)
	if err != nil {
		return err
	}
	return planner.scheduleNext(ctx, tenantID, opportunityID, decision)
}

func (planner Planner) scheduleNext(ctx context.Context, tenantID, opportunityID string, decision domain.Decision) error {
	if decision.NextDueAt.IsZero() || !decision.NextDueAt.After(planner.now().UTC()) {
		return nil
	}
	return planner.schedule(ctx, tenantID, opportunityID, decision, decision.NextDueAt, decision.NextCheckSuffix)
}

// schedule сохраняет долговечную проверку с ключом из Opportunity, сообщения-
// основания, версии правила и необязательного суффикса.
func (planner Planner) schedule(
	ctx context.Context,
	tenantID, opportunityID string,
	decision domain.Decision,
	dueAt time.Time,
	suffix string,
) error {
	if dueAt.IsZero() {
		return nil
	}
	triggerID := decision.TriggerMessageID
	if decision.Finding != nil {
		triggerID = decision.Finding.TriggerMessageID
	}
	if triggerID == "" {
		return nil
	}
	checkID, err := planner.ids.NewID()
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(refreshPayload{OpportunityID: opportunityID})
	dedupKey := fmt.Sprintf("opportunity:%s:message:%s:policy:%s", opportunityID, triggerID, planner.policy.Version())
	if suffix != "" {
		dedupKey += ":" + suffix
	}
	checkType, jobType, err := workTypes(planner.policy.Type())
	if err != nil {
		return err
	}
	check, err := jobsdomain.NewScheduledCheck(
		checkID, tenantID, checkType, "opportunity", opportunityID,
		jobType, dedupKey, payload, dueAt, planner.now().UTC(),
	)
	if err != nil {
		return err
	}
	_, _, err = planner.scheduler.Schedule(ctx, check)
	return err
}

func workTypes(riskType domain.Type) (string, string, error) {
	switch riskType {
	case domain.TypeNoResponse:
		return NoResponseCheckType, NoResponseEvaluationJobType, nil
	case domain.TypeBookingNotConfirmed:
		return BookingCheckType, BookingEvaluationJobType, nil
	case domain.TypePromiseNotFulfilled:
		return PromiseCheckType, PromiseEvaluationJobType, nil
	case domain.TypeCustomerSilentAfterPrice:
		return PriceCheckType, PriceEvaluationJobType, nil
	case domain.TypeFollowUpCandidate:
		return FollowUpCheckType, FollowUpEvaluationJobType, nil
	default:
		return "", "", ErrInvalidCheck
	}
}

type JobQueue interface {
	Enqueue(context.Context, jobsdomain.Job) (jobsdomain.Job, bool, error)
}

// ConversationChangedEventHandler планирует перепроверку после любого
// канонического изменения: как нового входящего, так и ответа бизнеса.
func ConversationChangedEventHandler(queue JobQueue, ids IDs) eventsapplication.Handler {
	return func(ctx context.Context, event eventsdomain.Event) error {
		var data conversationEventData
		if queue == nil || ids == nil {
			return jobsdomain.Permanent("BACKGROUND_CONFIGURATION_INVALID", errors.New("обработчик риска не настроен"))
		}
		if json.Unmarshal(event.Data, &data) != nil || data.ConversationID == "" || data.MessageID == "" || data.Revision < 1 ||
			event.AggregateType != "conversation" || event.AggregateID != data.ConversationID {
			return jobsdomain.Permanent("INVALID_OUTBOX_PAYLOAD", errors.New("некорректное событие переписки для риска"))
		}
		return enqueueRefresh(ctx, queue, ids, event, refreshPayload{ConversationID: data.ConversationID},
			fmt.Sprintf("conversation:%s:revision:%d", data.ConversationID, data.Revision))
	}
}

// OpportunityEventHandler гарантирует первичное планирование после создания
// Opportunity и немедленную перепроверку после изменения её активности.
func OpportunityEventHandler(queue JobQueue, ids IDs) eventsapplication.Handler {
	return func(ctx context.Context, event eventsdomain.Event) error {
		if queue == nil || ids == nil {
			return jobsdomain.Permanent("BACKGROUND_CONFIGURATION_INVALID", errors.New("обработчик риска не настроен"))
		}
		if event.AggregateType != "opportunity" || event.AggregateID == "" {
			return jobsdomain.Permanent("INVALID_OUTBOX_PAYLOAD", errors.New("некорректное событие возможности для риска"))
		}
		return enqueueRefresh(ctx, queue, ids, event, refreshPayload{OpportunityID: event.AggregateID}, "event:"+event.ID)
	}
}

// AIAnalysisAppliedEventHandler запускает R3 только после атомарно применённого
// свежего результата. Отклонённые и устаревшие AI-run такого события не имеют.
func AIAnalysisAppliedEventHandler(queue JobQueue, ids IDs) eventsapplication.Handler {
	return func(ctx context.Context, event eventsdomain.Event) error {
		if queue == nil || ids == nil {
			return jobsdomain.Permanent("BACKGROUND_CONFIGURATION_INVALID", errors.New("обработчик риска не настроен"))
		}
		var data analysisAppliedEventData
		if json.Unmarshal(event.Data, &data) != nil || event.AggregateType != "ai_run" ||
			event.AggregateID == "" || data.RunID != event.AggregateID || data.ConversationID == "" ||
			data.BaseConversationRevision < 1 || data.AnalysisThroughMessageID == "" {
			return jobsdomain.Permanent("INVALID_OUTBOX_PAYLOAD", errors.New("некорректное событие применённого анализа"))
		}
		return enqueueRefresh(
			ctx, queue, ids, event, refreshPayload{ConversationID: data.ConversationID},
			"ai-run:"+data.RunID,
		)
	}
}

func enqueueRefresh(
	ctx context.Context,
	queue JobQueue,
	ids IDs,
	event eventsdomain.Event,
	payload refreshPayload,
	dedupKey string,
) error {
	jobID, err := ids.NewID()
	if err != nil {
		return jobsdomain.Retryable("JOB_ID_UNAVAILABLE", err)
	}
	data, _ := json.Marshal(payload)
	job, err := jobsdomain.NewJob(jobID, event.TenantID, RefreshJobType, dedupKey, data, 0, event.OccurredAt, event.OccurredAt)
	if err != nil {
		return jobsdomain.Permanent("INVALID_JOB_PAYLOAD", err)
	}
	_, _, err = queue.Enqueue(ctx, job)
	if errors.Is(err, jobsdomain.ErrConflict) || errors.Is(err, jobsdomain.ErrInvalid) {
		return jobsdomain.Permanent("JOB_DEDUP_CONFLICT", err)
	}
	if err != nil {
		return jobsdomain.Retryable("JOB_ENQUEUE_FAILED", err)
	}
	return nil
}

func RefreshJobHandler(planner Planner) jobsapplication.Handler {
	return RefreshPlansJobHandler(planner)
}

// RefreshPlansJobHandler перечитывает один и тот же авторитетный снимок через
// независимые политики. Одна очередь планирования не размножает задания на
// каждое правило и сохраняет совместимость с уже записанными событиями.
func RefreshPlansJobHandler(planners ...Planner) jobsapplication.Handler {
	return func(ctx context.Context, job jobsdomain.Job) error {
		var payload refreshPayload
		if json.Unmarshal(job.Payload, &payload) != nil || (payload.ConversationID == "") == (payload.OpportunityID == "") {
			return jobsdomain.Permanent("INVALID_JOB_PAYLOAD", errors.New("некорректное задание планирования риска"))
		}
		for _, planner := range planners {
			var err error
			if payload.ConversationID != "" {
				err = planner.RefreshConversation(ctx, job.TenantID, payload.ConversationID)
			} else {
				err = planner.RefreshOpportunity(ctx, job.TenantID, payload.OpportunityID)
			}
			if classified := classifyRiskWork(err, "RISK_PLAN"); classified != nil {
				return classified
			}
		}
		return nil
	}
}

// EvaluationJobHandler выполняет наступившую проверку через планер, чтобы
// правило могло назначить следующую проверку того же основания.
func EvaluationJobHandler(planner Planner) jobsapplication.Handler {
	return func(ctx context.Context, job jobsdomain.Job) error {
		var payload refreshPayload
		if json.Unmarshal(job.Payload, &payload) != nil || payload.OpportunityID == "" || payload.ConversationID != "" {
			return jobsdomain.Permanent("INVALID_JOB_PAYLOAD", errors.New("некорректное задание проверки риска"))
		}
		return classifyRiskWork(planner.Evaluate(ctx, job.TenantID, payload.OpportunityID), "RISK_EVALUATION")
	}
}

func classifyRiskWork(err error, prefix string) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, ErrInvalidCheck), errors.Is(err, ErrStateNotFound),
		errors.Is(err, ErrStateIncomplete), errors.Is(err, domain.ErrInvalidRisk),
		errors.Is(err, domain.ErrInvalidBusinessHours), errors.Is(err, jobsdomain.ErrInvalid),
		errors.Is(err, jobsdomain.ErrConflict):
		return jobsdomain.Permanent(prefix+"_INVALID", err)
	default:
		return jobsdomain.Retryable(prefix+"_TEMPORARY", err)
	}
}
