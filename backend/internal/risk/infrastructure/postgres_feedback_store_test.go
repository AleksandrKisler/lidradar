package infrastructure

import (
	"context"
	"testing"
	"time"

	"lidradar/backend/internal/risk/application"
	"lidradar/backend/internal/risk/domain"
	"lidradar/backend/internal/testsupport"
	"lidradar/backend/platform/ids"
)

// LR-BE-RM-019: риск с тремя записями обратной связи учитывается один раз по
// последнему вердикту, метрика возвращает покрытие; обратная связь append-only,
// ложное срабатывание закрывает риск, согласие фиксируется в снимке.
func TestPostgresFeedbackAppendOnlyLatestVerdictAndConsent(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	pair := testsupport.TwoTenants(t, ctx, pool)
	fixture := insertRiskFixture(t, pool, pair.A.TenantID, pair.A.LocationID, domain.DirectionIncoming)
	now := fixture.messageAt.Add(60 * time.Minute)
	risk := storeRadarRisk(t, pool, fixture, domain.SeverityHigh, now.Add(-15*time.Minute), now)
	store := NewPostgresFeedbackStore(pool)
	newID := func() string {
		value, err := (ids.Generator{}).NewID()
		if err != nil {
			t.Fatal(err)
		}
		return value
	}

	loaded, stage, found, err := store.RiskForFeedback(ctx, pair.A.TenantID, risk.ID)
	if err != nil || !found || loaded.ID != risk.ID || stage != "NEW" {
		t.Fatalf("риск для обратной связи: found=%v stage=%s err=%v", found, stage, err)
	}
	if _, _, found, err := store.RiskForFeedback(ctx, pair.B.TenantID, risk.ID); err != nil || found {
		t.Fatalf("чужая организация увидела риск: found=%v err=%v", found, err)
	}
	if active, err := store.DatasetConsentActive(ctx, pair.A.TenantID); err != nil || active {
		t.Fatalf("согласие без выдачи активно: %v, %v", active, err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO ml_consents(id, tenant_id, scope, granted_by, granted_at) VALUES ($1, $2, 'DATASETS', $3, $4)`,
		newID(), pair.A.TenantID, pair.A.UserID, now); err != nil {
		t.Fatal(err)
	}
	if active, err := store.DatasetConsentActive(ctx, pair.A.TenantID); err != nil || !active {
		t.Fatalf("выданное согласие неактивно: %v, %v", active, err)
	}

	record := func(verdict domain.Verdict, reason domain.FeedbackReason, at time.Time) (domain.Risk, bool) {
		t.Helper()
		current, stage, _, err := store.RiskForFeedback(ctx, pair.A.TenantID, risk.ID)
		if err != nil {
			t.Fatal(err)
		}
		feedback, err := domain.NewFeedback(newID(), current, stage, pair.A.UserID, verdict, reason, "заметка", true, at)
		if err != nil {
			t.Fatal(err)
		}
		_, updated, changed, err := store.AppendFeedback(ctx, feedback, application.AuditRecord{
			ID: newID(), TenantID: pair.A.TenantID, ActorID: pair.A.UserID, Operation: "RISK_FEEDBACK_RECORDED",
			EntityType: "RISK_FEEDBACK", EntityID: feedback.ID, At: at,
		})
		if err != nil {
			t.Fatal(err)
		}
		return updated, changed
	}
	if updated, changed := record(domain.VerdictFalsePositive, domain.ReasonWrongInterpretation, now.Add(time.Minute)); !changed ||
		updated.Status != domain.StatusFalsePositive || updated.ResolvedAt == nil {
		t.Fatalf("ложное срабатывание не закрыло риск: changed=%v risk=%#v", changed, updated)
	}
	if updated, changed := record(domain.VerdictTruePositive, "", now.Add(2*time.Minute)); changed || updated.Status != domain.StatusFalsePositive {
		t.Fatalf("повторный вердикт изменил закрытый риск: changed=%v status=%s", changed, updated.Status)
	}
	if _, changed := record(domain.VerdictFalsePositive, domain.ReasonNotALead, now.Add(3*time.Minute)); changed {
		t.Fatal("третий вердикт снова изменил риск")
	}
	if _, err := pool.Exec(ctx, `UPDATE risk_feedback SET note = 'x' WHERE tenant_id = $1`, pair.A.TenantID); err == nil {
		t.Fatal("обратная связь изменена вопреки append-only")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM risk_feedback WHERE tenant_id = $1`, pair.A.TenantID); err == nil {
		t.Fatal("обратная связь удалена вопреки append-only")
	}
	var snapshotStatus, snapshotStage string
	var eligible bool
	var audits int
	if err := pool.QueryRow(ctx, `
		SELECT risk_status, opportunity_stage, dataset_eligible,
		       (SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND operation = 'RISK_FEEDBACK_RECORDED')
		FROM risk_feedback WHERE tenant_id = $1 ORDER BY created_at LIMIT 1`, pair.A.TenantID).Scan(&snapshotStatus, &snapshotStage, &eligible, &audits); err != nil {
		t.Fatal(err)
	}
	if snapshotStatus != "OPEN" || snapshotStage != "NEW" || !eligible || audits != 3 {
		t.Fatalf("снимок первой записи: status=%s stage=%s eligible=%v audits=%d", snapshotStatus, snapshotStage, eligible, audits)
	}

	// Второй риск без обратной связи: покрытие 1/2, ложных — 1 по последнему вердикту.
	other := insertRiskFixture(t, pool, pair.A.TenantID, pair.A.LocationID, domain.DirectionIncoming)
	storeRadarRisk(t, pool, other, domain.SeverityHigh, now.Add(-15*time.Minute), now.Add(5*time.Minute))
	rows, err := store.Precision(ctx, pair.A.TenantID, fixture.messageAt, now.Add(time.Hour))
	if err != nil || len(rows) != 1 || rows[0].Type != domain.TypeNoResponse || rows[0].TotalRisks != 2 || rows[0].WithFeedback != 1 ||
		rows[0].TruePositives != 0 || rows[0].FalsePositives != 1 || rows[0].CoverageRate() != 0.5 || !rows[0].Reliable() {
		t.Fatalf("точность: %#v, %v", rows, err)
	}
	if rows, err := store.Precision(ctx, pair.A.TenantID, now.Add(2*time.Hour), now.Add(3*time.Hour)); err != nil || len(rows) != 0 {
		t.Fatalf("окно без рисков: %#v, %v", rows, err)
	}
	if rows, err := store.Precision(ctx, pair.B.TenantID, fixture.messageAt, now.Add(time.Hour)); err != nil || len(rows) != 0 {
		t.Fatalf("чужая организация видит точность: %#v, %v", rows, err)
	}
}
