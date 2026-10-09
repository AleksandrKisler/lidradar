package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	aiapplication "lidradar/backend/internal/ai/application"
	aidomain "lidradar/backend/internal/ai/domain"
	aiinfrastructure "lidradar/backend/internal/ai/infrastructure"
	"lidradar/backend/platform/ids"
)

// Synthetic webhook -> canonical messages -> versioned AI result -> unknown
// opportunity -> scheduled risk -> Radar -> semantic resolution. No model or
// Telegram network is used; provider quality is checked by the separate DEV set.
func TestUnpricedAgreementRiskPipeline(t *testing.T) {
	f := newAPIFixtureForAnalysis(t, true)
	ctx := context.Background()
	owner := register(t, f.handler, "synthetic-agreement@example.com", "Синтетический владелец")
	tenant := createOrganization(t, f, owner, "Синтетическая организация")
	location := request(t, f.handler, http.MethodPost, "/api/v1/locations", `{"name":"Тестовая точка","timezone":"UTC","responseThresholdMinutes":1440,"agreementThresholdMinutes":120}`, owner.Cookie, tenant)
	requireStatus(t, location, http.StatusCreated)
	locationID := jsonID(t, location)
	days := []map[string]any{}
	for weekday := 1; weekday <= 7; weekday++ {
		days = append(days, map[string]any{"weekday": weekday, "closed": false, "opensAt": "00:00", "closesAt": "23:59"})
	}
	hours, _ := json.Marshal(map[string]any{"timezone": "UTC", "days": days})
	requireStatus(t, request(t, f.handler, http.MethodPut, "/api/v1/locations/"+locationID+"/business-hours", string(hours), owner.Cookie, tenant), http.StatusOK)
	secret := "synthetic-agreement-webhook"
	connected := request(t, f.handler, http.MethodPost, "/api/v1/integrations/GENERIC_WEBHOOK/connect", `{"name":"Синтетический канал","locationId":"`+locationID+`","webhookSecret":"`+secret+`"}`, owner.Cookie, tenant)
	requireStatus(t, connected, http.StatusCreated)
	webhook := "/api/v1/webhooks/GENERIC_WEBHOOK/" + tenant + "/" + jsonID(t, connected)
	send := func(index int, direction, body string, at time.Time) {
		t.Helper()
		external := fmt.Sprintf("synthetic-message-%d", index)
		payload := canonicalWebhook(fmt.Sprintf("synthetic-event-%d", index), "message.received.v1", "synthetic-dialog", external, "synthetic-contact", direction, "TEXT", body, at.Format(time.RFC3339Nano), "")
		requireStatus(t, webhookRequest(t, f.handler, webhook, payload, "X-LidRadar-Webhook-Secret", secret), http.StatusAccepted)
		processExactly(t, f, 1)
	}
	at := time.Now().UTC().Add(-3 * time.Hour)
	send(1, "INCOMING", "Хочу записаться на диагностику.", at.Add(-time.Minute))
	send(2, "OUTGOING", "Свободно в четверг в 16:45. Вам подходит?", at)
	var before int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM opportunities WHERE tenant_id=$1`, tenant).Scan(&before); err != nil || before != 0 {
		t.Fatalf("catalogue-free input created deal before trusted analysis: %d %v", before, err)
	}
	service := aiapplication.NewService(aiinfrastructure.NewPostgresStore(f.pool), ids.Generator{}, time.Now, aiapplication.DefaultLease).WithAnalysisDebounce(0).WithStaleJobBuilder(aiinfrastructure.NewPostgresAnalysisJobBuilder(f.pool, aiapplication.DefaultModelVersion))
	nodeSecret := "synthetic-node-secret-with-at-least-32-chars"
	node, err := service.RegisterNode(ctx, tenant, "SYNTHETIC-AGREEMENT-NODE", nodeSecret)
	if err != nil {
		t.Fatal(err)
	}
	analyze := func(status aidomain.AgreementStatus) {
		t.Helper()
		if err := service.Heartbeat(ctx, node.ID, nodeSecret, aiapplication.HeartbeatCommand{Status: aidomain.NodeReady, ModelVersion: aiapplication.DefaultModelVersion, AvailableSlots: 1}); err != nil {
			t.Fatal(err)
		}
		job, found, err := service.Claim(ctx, node.ID, nodeSecret)
		if err != nil || !found {
			t.Fatalf("claim: %v %v", found, err)
		}
		input := decodeAnalysisJob(t, job.Prompt)
		if input.SchemaVersion != aiapplication.AnalysisSchemaV2 || input.PromptVersion != aiapplication.CurrentAnalysisPrompt {
			t.Fatalf("wrong current contract: %+v", input)
		}
		messageIDs := map[string]string{}
		for _, m := range input.Messages {
			messageIDs[m.Body] = m.ID
		}
		intent := messageIDs["Хочу записаться на диагностику."]
		offer := messageIDs["Свободно в четверг в 16:45. Вам подходит?"]
		evidence := []string{offer}
		if status == aidomain.AgreementResolved {
			intent = messageIDs["Да, время подходит, запишите."]
			evidence = append(evidence, intent)
		}
		result := aidomain.AnalysisResultV2{SchemaVersion: aiapplication.AnalysisSchemaV2, AnalysisThroughMessageID: input.AnalysisThroughMessageID, Summary: "Синтетическая запись.", Facts: []aidomain.SemanticFact{{Type: aidomain.FactBookingIntent, Value: true, Confidence: .99, EvidenceMessageIDs: []string{intent}}}, Agreements: []aidomain.AgreementObservation{{Kind: aidomain.AgreementBookingConfirmation, WaitingFor: aidomain.AgreementCustomer, Status: status, TriggerMessageID: offer, EvidenceMessageIDs: evidence, Confidence: .99}}}
		raw, _ := json.Marshal(result)
		run, err := service.Started(ctx, node.ID, nodeSecret, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		completed, err := service.Complete(ctx, node.ID, nodeSecret, job.ID, run.ID, string(raw))
		if err != nil || completed.ApplicationStatus != aidomain.ApplicationApplied {
			t.Fatalf("complete: %+v %v", completed, err)
		}
		processExactly(t, f, 1)
	}
	analyze(aidomain.AgreementPending)
	promoted, err := f.scheduler.RunOnce(ctx, 100)
	if err != nil || promoted != 1 {
		t.Fatalf("scheduled agreement check: %d %v", promoted, err)
	}
	processExactly(t, f, 1)
	var opportunity, risk, status string
	var amount, serviceID *string
	if err := f.pool.QueryRow(ctx, `SELECT id,estimated_amount::text,service_id::text FROM opportunities WHERE tenant_id=$1`, tenant).Scan(&opportunity, &amount, &serviceID); err != nil || amount != nil || serviceID != nil {
		t.Fatalf("unknown deal: amount=%v service=%v err=%v", amount, serviceID, err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT id,status FROM risk_signals WHERE tenant_id=$1 AND type='UNFINISHED_AGREEMENT'`, tenant).Scan(&risk, &status); err != nil || status != "OPEN" {
		t.Fatalf("risk: %s %v", status, err)
	}
	detail := request(t, f.handler, http.MethodGet, "/api/v1/risks/"+risk, "", owner.Cookie, tenant)
	requireStatus(t, detail, http.StatusOK)
	var count int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM risk_signals WHERE tenant_id=$1 AND status IN ('OPEN','ACKNOWLEDGED','ACTED')`, tenant).Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicated specialized risk: %d %v", count, err)
	}
	send(3, "INCOMING", "Да, время подходит, запишите.", time.Now().UTC())
	analyze(aidomain.AgreementResolved)
	if err := f.pool.QueryRow(ctx, `SELECT status FROM risk_signals WHERE id=$1`, risk).Scan(&status); err != nil || status != "RESOLVED" {
		t.Fatalf("confirmation did not resolve risk: %s %v", status, err)
	}
	requireStatus(t, request(t, f.handler, http.MethodPatch, "/api/v1/opportunities/"+opportunity, `{"stage":"LOST"}`, owner.Cookie, tenant), http.StatusOK)
	processExactly(t, f, 1)
	send(4, "INCOMING", "Спасибо.", time.Now().UTC())
	analyze(aidomain.AgreementResolved)
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM opportunities WHERE tenant_id=$1`, tenant).Scan(&count); err != nil || count != 1 {
		t.Fatalf("old intent reopened a manually closed deal: %d %v", count, err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM risk_signals WHERE tenant_id=$1 AND status IN ('OPEN','ACKNOWLEDGED','ACTED')`, tenant).Scan(&count); err != nil || count != 0 {
		t.Fatalf("closed synthetic risk reopened: %d %v", count, err)
	}
}
