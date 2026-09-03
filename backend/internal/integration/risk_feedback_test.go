package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// TestRiskFeedbackPrecisionAndLeadCorrectionFlow доказывает выходной критерий
// этапа 21 на PostgreSQL: вердикты сохраняются append-only со снимком, ложное
// срабатывание закрывает риск, причина NOT_A_LEAD закрывает сделку, а precision
// и доля ложных считаются по типу риска по последнему вердикту с покрытием.
func TestRiskFeedbackPrecisionAndLeadCorrectionFlow(t *testing.T) {
	fixture := newAPIFixture(t)
	owner := register(t, fixture.handler, "feedback-owner@example.com", "Владелец качества")
	tenantID := createOrganization(t, fixture, owner, "Организация качества")
	stranger := register(t, fixture.handler, "feedback-stranger@example.com", "Посторонний")

	locationResponse := request(t, fixture.handler, http.MethodPost, "/api/v1/locations", `{
		"name":"Точка качества","timezone":"UTC","responseThresholdMinutes":45
	}`, owner.Cookie, tenantID)
	requireStatus(t, locationResponse, http.StatusCreated)
	locationID := jsonID(t, locationResponse)
	requireStatus(t, request(t, fixture.handler, http.MethodPut, "/api/v1/locations/"+locationID+"/business-hours", `{
		"timezone":"UTC","days":[
			{"weekday":1,"closed":false,"opensAt":"00:00","closesAt":"23:59"},
			{"weekday":2,"closed":false,"opensAt":"00:00","closesAt":"23:59"},
			{"weekday":3,"closed":false,"opensAt":"00:00","closesAt":"23:59"},
			{"weekday":4,"closed":false,"opensAt":"00:00","closesAt":"23:59"},
			{"weekday":5,"closed":false,"opensAt":"00:00","closesAt":"23:59"},
			{"weekday":6,"closed":false,"opensAt":"00:00","closesAt":"23:59"},
			{"weekday":7,"closed":false,"opensAt":"00:00","closesAt":"23:59"}
		]
	}`, owner.Cookie, tenantID), http.StatusOK)
	requireStatus(t, request(t, fixture.handler, http.MethodPost, "/api/v1/services", `{
		"name":"Полировка","locationId":"`+locationID+`","priceFrom":"5000","priceTo":"5000"
	}`, owner.Cookie, tenantID), http.StatusCreated)
	secret := "feedback-flow-secret-123"
	connected := request(t, fixture.handler, http.MethodPost, "/api/v1/integrations/GENERIC_WEBHOOK/connect", `{
		"name":"Канал качества","locationId":"`+locationID+`","webhookSecret":"`+secret+`"
	}`, owner.Cookie, tenantID)
	requireStatus(t, connected, http.StatusCreated)
	path := "/api/v1/webhooks/GENERIC_WEBHOOK/" + tenantID + "/" + jsonID(t, connected)
	incomingAt := time.Now().UTC().Add(-60 * time.Minute).Format(time.RFC3339Nano)
	incoming := canonicalWebhook(
		"feedback-event-incoming", "message.received.v1", "feedback-dialog", "feedback-message-incoming", "feedback-contact",
		"INCOMING", "TEXT", "Нужна полировка, подскажите цену", incomingAt, "",
	)
	requireStatus(t, webhookRequest(t, fixture.handler, path, incoming, "X-LidRadar-Webhook-Secret", secret), http.StatusAccepted)
	processExactly(t, fixture, 3)
	if promoted, err := fixture.scheduler.RunOnce(context.Background(), 100); err != nil || promoted < 1 {
		t.Fatalf("перенос проверки R1 = %d, %v", promoted, err)
	}
	processExactly(t, fixture, 1)
	var riskID, opportunityID string
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT id::text, opportunity_id::text FROM risk_signals WHERE tenant_id = $1 AND type = 'NO_RESPONSE'`, tenantID).Scan(&riskID, &opportunityID); err != nil {
		t.Fatal(err)
	}

	// ML-согласие: явное, активное, отзываемое; повтор выдачи не создаёт второго.
	consent := request(t, fixture.handler, http.MethodGet, "/api/v1/organization/ml-consent", "", owner.Cookie, tenantID)
	requireStatus(t, consent, http.StatusOK)
	var consentStatus struct {
		Active  bool            `json:"active"`
		Consent json.RawMessage `json:"consent"`
	}
	if err := json.Unmarshal(consent.Body.Bytes(), &consentStatus); err != nil || consentStatus.Active || string(consentStatus.Consent) != "null" {
		t.Fatalf("согласие до выдачи: %s, %v", consent.Body.String(), err)
	}
	requireStatus(t, request(t, fixture.handler, http.MethodPost, "/api/v1/organization/ml-consent", "", stranger.Cookie, tenantID), http.StatusForbidden)
	requireStatus(t, request(t, fixture.handler, http.MethodPost, "/api/v1/organization/ml-consent", "", owner.Cookie, tenantID), http.StatusCreated)
	requireStatus(t, request(t, fixture.handler, http.MethodPost, "/api/v1/organization/ml-consent", "", owner.Cookie, tenantID), http.StatusOK)

	feedbackPath := "/api/v1/risks/" + riskID + "/feedback"
	requireStatus(t, request(t, fixture.handler, http.MethodPost, feedbackPath, `{"verdict":"TRUE_POSITIVE"}`, stranger.Cookie, tenantID), http.StatusForbidden)
	requireStatus(t, request(t, fixture.handler, http.MethodPost, feedbackPath, `{"verdict":"FALSE_POSITIVE"}`, owner.Cookie, tenantID), http.StatusBadRequest)
	requireStatus(t, request(t, fixture.handler, http.MethodPost, feedbackPath, `{"verdict":"MAYBE"}`, owner.Cookie, tenantID), http.StatusBadRequest)
	requireStatus(t, request(t, fixture.handler, http.MethodPost, "/api/v1/risks/"+opportunityID+"/feedback", `{"verdict":"TRUE_POSITIVE"}`, owner.Cookie, tenantID), http.StatusNotFound)
	truePositive := request(t, fixture.handler, http.MethodPost, feedbackPath, `{"verdict":"TRUE_POSITIVE","note":" клиент ждал ответа "}`, owner.Cookie, tenantID)
	requireStatus(t, truePositive, http.StatusCreated)
	var recorded struct {
		Verdict         string `json:"verdict"`
		Note            string `json:"note"`
		DatasetEligible bool   `json:"datasetEligible"`
		Context         struct {
			Status           string `json:"status"`
			OpportunityStage string `json:"opportunityStage"`
			Type             string `json:"type"`
		} `json:"context"`
	}
	if err := json.Unmarshal(truePositive.Body.Bytes(), &recorded); err != nil || recorded.Verdict != "TRUE_POSITIVE" || recorded.Note != "клиент ждал ответа" ||
		!recorded.DatasetEligible || recorded.Context.Status != "OPEN" || recorded.Context.Type != "NO_RESPONSE" || recorded.Context.OpportunityStage == "" {
		t.Fatalf("настоящий риск: %s, %v", truePositive.Body.String(), err)
	}
	var status string
	if err := fixture.pool.QueryRow(context.Background(), `SELECT status FROM risk_signals WHERE tenant_id = $1 AND id = $2`, tenantID, riskID).Scan(&status); err != nil || status != "OPEN" {
		t.Fatalf("TRUE_POSITIVE изменил риск: %s, %v", status, err)
	}
	requireStatus(t, request(t, fixture.handler, http.MethodDelete, "/api/v1/organization/ml-consent", "", owner.Cookie, tenantID), http.StatusNoContent)

	// Ложное срабатывание «не лид»: риск закрыт, сделка потеряна с источником USER.
	falsePositive := request(t, fixture.handler, http.MethodPost, feedbackPath, `{"verdict":"FALSE_POSITIVE","reason":"NOT_A_LEAD","note":"спрашивали дорогу"}`, owner.Cookie, tenantID)
	requireStatus(t, falsePositive, http.StatusCreated)
	if err := json.Unmarshal(falsePositive.Body.Bytes(), &recorded); err != nil || recorded.DatasetEligible || recorded.Context.Status != "OPEN" {
		t.Fatalf("ложное срабатывание: %s, %v", falsePositive.Body.String(), err)
	}
	var stage, historySource, historyActor string
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT risk.status, opportunity.stage,
		       COALESCE((SELECT source FROM opportunity_stage_history AS history
		                 WHERE history.tenant_id = opportunity.tenant_id AND history.opportunity_id = opportunity.id AND history.to_stage = 'LOST'), ''),
		       COALESCE((SELECT actor_user_id::text FROM opportunity_stage_history AS history
		                 WHERE history.tenant_id = opportunity.tenant_id AND history.opportunity_id = opportunity.id AND history.to_stage = 'LOST'), '')
		FROM risk_signals AS risk
		JOIN opportunities AS opportunity ON opportunity.tenant_id = risk.tenant_id AND opportunity.id = risk.opportunity_id
		WHERE risk.tenant_id = $1 AND risk.id = $2`, tenantID, riskID).Scan(&status, &stage, &historySource, &historyActor); err != nil {
		t.Fatal(err)
	}
	if status != "FALSE_POSITIVE" || stage != "LOST" || historySource != "USER" || historyActor != owner.ID {
		t.Fatalf("после «не лид»: риск=%s сделка=%s источник=%s автор=%s", status, stage, historySource, historyActor)
	}
	detail := request(t, fixture.handler, http.MethodGet, "/api/v1/risks/"+riskID, "", owner.Cookie, tenantID)
	requireStatus(t, detail, http.StatusOK)
	var radar struct {
		Risk struct {
			Status string `json:"status"`
		} `json:"risk"`
	}
	if err := json.Unmarshal(detail.Body.Bytes(), &radar); err != nil || radar.Risk.Status != "FALSE_POSITIVE" {
		t.Fatalf("Radar после обратной связи: %s, %v", detail.Body.String(), err)
	}
	if _, err := fixture.pool.Exec(context.Background(), `UPDATE risk_feedback SET verdict = 'TRUE_POSITIVE' WHERE tenant_id = $1`, tenantID); err == nil {
		t.Fatal("обратная связь изменена вопреки append-only")
	}

	// Precision по типу: две записи на один риск дают один учёт по последнему вердикту.
	precision := request(t, fixture.handler, http.MethodGet, "/api/v1/risks/precision", "", owner.Cookie, tenantID)
	requireStatus(t, precision, http.StatusOK)
	var report struct {
		MinimumCoverage float64 `json:"minimumCoverage"`
		Items           []struct {
			RiskType          string   `json:"riskType"`
			TotalRisks        int      `json:"totalRisks"`
			WithFeedback      int      `json:"withFeedback"`
			TruePositives     int      `json:"truePositives"`
			FalsePositives    int      `json:"falsePositives"`
			Precision         *float64 `json:"precision"`
			FalsePositiveRate *float64 `json:"falsePositiveRate"`
			CoverageRate      float64  `json:"coverageRate"`
			Reliable          bool     `json:"reliable"`
		} `json:"items"`
	}
	if err := json.Unmarshal(precision.Body.Bytes(), &report); err != nil || len(report.Items) != 5 || report.MinimumCoverage != 0.5 {
		t.Fatalf("отчёт точности: %s, %v", precision.Body.String(), err)
	}
	first := report.Items[0]
	if first.RiskType != "NO_RESPONSE" || first.TotalRisks != 1 || first.WithFeedback != 1 || first.TruePositives != 0 || first.FalsePositives != 1 ||
		first.Precision == nil || *first.Precision != 0 || first.FalsePositiveRate == nil || *first.FalsePositiveRate != 1 ||
		first.CoverageRate != 1 || !first.Reliable {
		t.Fatalf("строка NO_RESPONSE: %+v", first)
	}
	if empty := report.Items[1]; empty.RiskType != "CUSTOMER_SILENT_AFTER_PRICE" || empty.Precision != nil || empty.Reliable {
		t.Fatalf("пустой тип: %+v", empty)
	}
	requireStatus(t, request(t, fixture.handler, http.MethodGet, "/api/v1/risks/precision?from=2026-01-01T00:00:00Z&to=2026-01-01T00:00:00Z", "", owner.Cookie, tenantID), http.StatusBadRequest)
	requireStatus(t, request(t, fixture.handler, http.MethodGet, "/api/v1/risks/precision", "", stranger.Cookie, tenantID), http.StatusForbidden)
	future := request(t, fixture.handler, http.MethodGet, "/api/v1/risks/precision?from="+time.Now().UTC().Add(time.Hour).Format(time.RFC3339)+
		"&to="+time.Now().UTC().Add(2*time.Hour).Format(time.RFC3339), "", owner.Cookie, tenantID)
	requireStatus(t, future, http.StatusOK)
	if err := json.Unmarshal(future.Body.Bytes(), &report); err != nil || report.Items[0].TotalRisks != 0 {
		t.Fatalf("окно в будущем: %s, %v", future.Body.String(), err)
	}
	var feedbackAudits, consentAudits int
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT count(*) FILTER (WHERE operation = 'RISK_FEEDBACK_RECORDED'),
		       count(*) FILTER (WHERE operation IN ('ML_CONSENT_GRANTED', 'ML_CONSENT_REVOKED'))
		FROM audit_log WHERE tenant_id = $1`, tenantID).Scan(&feedbackAudits, &consentAudits); err != nil {
		t.Fatal(err)
	}
	if feedbackAudits != 2 || consentAudits != 2 {
		t.Fatalf("аудит: обратная связь=%d согласие=%d", feedbackAudits, consentAudits)
	}
}
