package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"lidradar/backend/internal/risk/domain"
)

// PermissionAnalytics открывает метрики качества; она есть только у владельца.
const PermissionAnalytics = "analytics.read"

var ErrLeadCorrection = errors.New("сделка не закрыта после обратной связи")

// AuditRecord — запись аудита, сохраняемая в той же транзакции, что и факт.
type AuditRecord struct {
	ID, TenantID, ActorID, Operation, EntityType, EntityID string
	At                                                     time.Time
}

type FeedbackStore interface {
	// RiskForFeedback читает риск и текущую стадию его сделки для снимка.
	RiskForFeedback(ctx context.Context, tenantID, riskID string) (domain.Risk, string, bool, error)
	DatasetConsentActive(ctx context.Context, tenantID string) (bool, error)
	// AppendFeedback атомарно сохраняет факт, применяет вердикт к активному
	// риску и пишет аудит; возвращает актуальный риск и признак его изменения.
	AppendFeedback(ctx context.Context, feedback domain.Feedback, audit AuditRecord) (domain.Feedback, domain.Risk, bool, error)
	Precision(ctx context.Context, tenantID string, from, to time.Time) ([]domain.PrecisionRow, error)
}

// LeadCorrector закрывает сделку, признанную не лидом; реализует модуль Opportunity.
type LeadCorrector interface {
	MarkNotALead(ctx context.Context, actorID, tenantID, opportunityID string) error
}

type FeedbackCommand struct {
	Verdict, Reason, Note string
}

type Feedback struct {
	store  FeedbackStore
	auth   Authorizer
	events Invalidator
	leads  LeadCorrector
	ids    IDs
	now    func() time.Time
}

func NewFeedback(store FeedbackStore, auth Authorizer, ids IDs, now func() time.Time) Feedback {
	return Feedback{store: store, auth: auth, ids: ids, now: now}
}

func (f Feedback) WithInvalidator(events Invalidator) Feedback { f.events = events; return f }

func (f Feedback) WithLeadCorrector(leads LeadCorrector) Feedback { f.leads = leads; return f }

func permit(ctx context.Context, auth Authorizer, actor, tenant, permission string) error {
	if auth == nil || actor == "" || tenant == "" {
		return ErrForbidden
	}
	allowed, err := auth.Allowed(ctx, actor, tenant, permission)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrForbidden
	}
	return nil
}

// Record сохраняет вердикт. FALSE_POSITIVE закрывает активный риск как
// ложное срабатывание, а причина NOT_A_LEAD дополнительно закрывает сделку.
func (f Feedback) Record(ctx context.Context, actor, tenant, riskID string, command FeedbackCommand) (domain.Feedback, error) {
	if f.store == nil || f.ids == nil || f.now == nil || strings.TrimSpace(riskID) == "" {
		return domain.Feedback{}, ErrInvalidCommand
	}
	if err := permit(ctx, f.auth, actor, tenant, PermissionManage); err != nil {
		return domain.Feedback{}, err
	}
	risk, stage, found, err := f.store.RiskForFeedback(ctx, tenant, riskID)
	if err != nil {
		return domain.Feedback{}, err
	}
	if !found {
		return domain.Feedback{}, ErrNotFound
	}
	eligible, err := f.store.DatasetConsentActive(ctx, tenant)
	if err != nil {
		return domain.Feedback{}, err
	}
	feedbackID, err := f.ids.NewID()
	if err != nil {
		return domain.Feedback{}, err
	}
	auditID, err := f.ids.NewID()
	if err != nil {
		return domain.Feedback{}, err
	}
	now := f.now().UTC()
	feedback, err := domain.NewFeedback(
		feedbackID, risk, stage, actor, domain.Verdict(strings.TrimSpace(command.Verdict)),
		domain.FeedbackReason(strings.TrimSpace(command.Reason)), command.Note, eligible, now,
	)
	if err != nil {
		return domain.Feedback{}, fmt.Errorf("%w: %v", ErrInvalidCommand, err)
	}
	stored, updated, changed, err := f.store.AppendFeedback(ctx, feedback, AuditRecord{
		ID: auditID, TenantID: tenant, ActorID: actor, Operation: "RISK_FEEDBACK_RECORDED",
		EntityType: "RISK_FEEDBACK", EntityID: feedback.ID, At: now,
	})
	if err != nil {
		return domain.Feedback{}, err
	}
	if changed && f.events != nil {
		f.events.Publish(tenant, "risk.false_positive", riskID)
	}
	if stored.CorrectsLead() && f.leads != nil {
		if err := f.leads.MarkNotALead(ctx, actor, tenant, updated.OpportunityID); err != nil {
			return stored, fmt.Errorf("%w: %v", ErrLeadCorrection, err)
		}
	}
	return stored, nil
}

type PrecisionItem struct {
	RiskType          domain.Type `json:"riskType"`
	TotalRisks        int         `json:"totalRisks"`
	WithFeedback      int         `json:"withFeedback"`
	TruePositives     int         `json:"truePositives"`
	FalsePositives    int         `json:"falsePositives"`
	Precision         *float64    `json:"precision"`
	FalsePositiveRate *float64    `json:"falsePositiveRate"`
	CoverageRate      float64     `json:"coverageRate"`
	Reliable          bool        `json:"reliable"`
}

type PrecisionReport struct {
	From            time.Time       `json:"from"`
	To              time.Time       `json:"to"`
	MinimumCoverage float64         `json:"minimumCoverage"`
	Items           []PrecisionItem `json:"items"`
}

// Precision считает точность по типу риска за окно обнаружения [from, to).
// Пустые границы означают «с начала» и «до сейчас».
func (f Feedback) Precision(ctx context.Context, actor, tenant string, from, to time.Time) (PrecisionReport, error) {
	if f.store == nil || f.now == nil {
		return PrecisionReport{}, ErrInvalidCommand
	}
	if err := permit(ctx, f.auth, actor, tenant, PermissionAnalytics); err != nil {
		return PrecisionReport{}, err
	}
	if to.IsZero() {
		to = f.now()
	}
	if from.IsZero() {
		from = time.Unix(0, 0)
	}
	from, to = from.UTC(), to.UTC()
	if !from.Before(to) {
		return PrecisionReport{}, ErrInvalidCommand
	}
	rows, err := f.store.Precision(ctx, tenant, from, to)
	if err != nil {
		return PrecisionReport{}, err
	}
	byType := make(map[domain.Type]domain.PrecisionRow, len(rows))
	for _, row := range rows {
		byType[row.Type] = row
	}
	report := PrecisionReport{From: from, To: to, MinimumCoverage: domain.MilestoneCoverage}
	for _, riskType := range domain.Types() {
		row := byType[riskType]
		row.Type = riskType
		report.Items = append(report.Items, PrecisionItem{
			RiskType: riskType, TotalRisks: row.TotalRisks, WithFeedback: row.WithFeedback,
			TruePositives: row.TruePositives, FalsePositives: row.FalsePositives,
			Precision: row.Precision(), FalsePositiveRate: row.FalsePositiveRate(),
			CoverageRate: row.CoverageRate(), Reliable: row.Reliable(),
		})
	}
	return report, nil
}
