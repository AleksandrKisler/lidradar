package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	adminapplication "lidradar/backend/internal/admin/application"
	admininfrastructure "lidradar/backend/internal/admin/infrastructure"
	"lidradar/backend/platform/ids"
)

// TestAdminObservabilityDiagnosesPipelineWithoutDatabaseAccess доказывает
// выходной критерий этапа 23 на PostgreSQL: платформенный администратор через
// API видит организации, каналы, очереди, восстанавливает цепочку от сообщения
// до выручки и чинит искусственно сломанный контур повтором мёртвого задания.
func TestAdminObservabilityDiagnosesPipelineWithoutDatabaseAccess(t *testing.T) {
	fixture := newAPIFixture(t)
	owner := register(t, fixture.handler, "admin-owner@example.com", "Владелец")
	tenantID := createOrganization(t, fixture, owner, "Организация наблюдаемости")
	operator := register(t, fixture.handler, "platform-operator@example.com", "Оператор платформы")

	locationResponse := request(t, fixture.handler, http.MethodPost, "/api/v1/locations", `{
		"name":"Точка наблюдаемости","timezone":"UTC","responseThresholdMinutes":45
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
	secret := "admin-flow-secret-123"
	connected := request(t, fixture.handler, http.MethodPost, "/api/v1/integrations/GENERIC_WEBHOOK/connect", `{
		"name":"Канал наблюдаемости","locationId":"`+locationID+`","webhookSecret":"`+secret+`"
	}`, owner.Cookie, tenantID)
	requireStatus(t, connected, http.StatusCreated)
	connectionID := jsonID(t, connected)
	path := "/api/v1/webhooks/GENERIC_WEBHOOK/" + tenantID + "/" + connectionID
	incomingAt := time.Now().UTC().Add(-60 * time.Minute).Format(time.RFC3339Nano)
	incoming := canonicalWebhook(
		"admin-event-incoming", "message.received.v1", "admin-dialog", "admin-message-incoming", "admin-contact",
		"INCOMING", "TEXT", "Нужна полировка", incomingAt, "",
	)
	requireStatus(t, webhookRequest(t, fixture.handler, path, incoming, "X-LidRadar-Webhook-Secret", secret), http.StatusAccepted)
	processExactly(t, fixture, 3)
	if promoted, err := fixture.scheduler.RunOnce(context.Background(), 100); err != nil || promoted < 1 {
		t.Fatalf("перенос проверки R1 = %d, %v", promoted, err)
	}
	processExactly(t, fixture, 1)
	var riskID, opportunityID, messageID string
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT id::text, opportunity_id::text, trigger_message_id::text FROM risk_signals WHERE tenant_id = $1 AND type = 'NO_RESPONSE'`, tenantID).Scan(&riskID, &opportunityID, &messageID); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, request(t, fixture.handler, http.MethodPost, "/api/v1/risks/"+riskID+"/recommendation", "", owner.Cookie, tenantID), http.StatusOK)
	action := idempotentRequest(t, fixture.handler, http.MethodPost, "/api/v1/risks/"+riskID+"/actions",
		`{"type":"MARK_CONTACTED","note":"Связались"}`, owner.Cookie, tenantID, "admin-action")
	requireStatus(t, action, http.StatusCreated)
	paid := idempotentRequest(t, fixture.handler, http.MethodPost, "/api/v1/opportunities/"+opportunityID+"/outcomes",
		`{"status":"PAID","note":"Оплачено"}`, owner.Cookie, tenantID, "admin-paid")
	requireStatus(t, paid, http.StatusCreated)
	requireStatus(t, idempotentRequest(t, fixture.handler, http.MethodPost, "/api/v1/opportunities/"+opportunityID+"/revenue",
		`{"amount":"47000","currency":"RUB","attributionType":"RECOVERED","riskId":"`+riskID+`","actionId":"`+jsonID(t, action)+`","outcomeId":"`+jsonID(t, paid)+`"}`,
		owner.Cookie, tenantID, "admin-revenue"), http.StatusCreated)

	// Без права PLATFORM_ADMIN административный API закрыт даже для владельца.
	requireStatus(t, request(t, fixture.handler, http.MethodGet, "/api/v1/admin/organizations", "", owner.Cookie, ""), http.StatusForbidden)
	requireStatus(t, request(t, fixture.handler, http.MethodGet, "/api/v1/admin/organizations", "", nil, ""), http.StatusUnauthorized)
	me := request(t, fixture.handler, http.MethodGet, "/api/v1/admin/me", "", operator.Cookie, "")
	requireStatus(t, me, http.StatusOK)
	if !strings.Contains(me.Body.String(), `"platformAdmin":false`) {
		t.Fatalf("статус до выдачи: %s", me.Body.String())
	}
	// Первый администратор появляется через CLI-путь без сессии.
	bootstrap := adminapplication.NewService(admininfrastructure.NewPostgresStore(fixture.pool), ids.Generator{}, time.Now)
	if _, created, err := bootstrap.GrantFromCLI(context.Background(), "platform-operator@example.com", "первый администратор"); err != nil || !created {
		t.Fatalf("выдача через CLI: created=%v, %v", created, err)
	}

	organizations := request(t, fixture.handler, http.MethodGet, "/api/v1/admin/organizations", "", operator.Cookie, "")
	requireStatus(t, organizations, http.StatusOK)
	var organizationPage struct {
		Items []struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Members     int    `json:"members"`
			Connections int    `json:"connections"`
			OpenRisks   int    `json:"openRisks"`
		} `json:"items"`
	}
	if err := json.Unmarshal(organizations.Body.Bytes(), &organizationPage); err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, item := range organizationPage.Items {
		if item.ID == tenantID {
			seen = item.Members == 1 && item.Connections == 1 && item.OpenRisks == 1 && item.Name == "Организация наблюдаемости"
		}
	}
	if !seen {
		t.Fatalf("организации: %s", organizations.Body.String())
	}
	connections := request(t, fixture.handler, http.MethodGet, "/api/v1/admin/connections", "", operator.Cookie, "")
	requireStatus(t, connections, http.StatusOK)
	if body := connections.Body.String(); !strings.Contains(body, `"id":"`+connectionID+`"`) || !strings.Contains(body, `"provider":"GENERIC_WEBHOOK"`) {
		t.Fatalf("каналы: %s", body)
	}

	// Цепочка LR-BE-2310 от сообщения до выручки без текста сообщения.
	trace := request(t, fixture.handler, http.MethodGet, "/api/v1/admin/trace/tenants/"+tenantID+"/messages/"+messageID, "", operator.Cookie, "")
	requireStatus(t, trace, http.StatusOK)
	var chain struct {
		Message struct {
			ID        string `json:"id"`
			Direction string `json:"direction"`
		} `json:"message"`
		Jobs          []json.RawMessage             `json:"jobs"`
		AIJobs        []json.RawMessage             `json:"aiJobs"`
		Risks         []struct{ ID, Status string } `json:"risks"`
		Notifications []struct {
			Kind       string            `json:"kind"`
			Deliveries []json.RawMessage `json:"deliveries"`
		} `json:"notifications"`
		Actions  []json.RawMessage `json:"actions"`
		Outcomes []json.RawMessage `json:"outcomes"`
		Revenue  []struct {
			Amount      string  `json:"amount"`
			Attribution *string `json:"attribution"`
		} `json:"revenue"`
	}
	if err := json.Unmarshal(trace.Body.Bytes(), &chain); err != nil {
		t.Fatal(err)
	}
	if chain.Message.ID != messageID || chain.Message.Direction != "INCOMING" || len(chain.Jobs) < 2 || len(chain.Risks) != 1 ||
		chain.Risks[0].ID != riskID || chain.Risks[0].Status != "ACTED" || len(chain.Notifications) != 1 || chain.Notifications[0].Kind != "RISK_OPENED" ||
		len(chain.Notifications[0].Deliveries) != 1 || len(chain.Actions) != 1 || len(chain.Outcomes) != 1 || len(chain.Revenue) != 1 ||
		chain.Revenue[0].Amount != "47000.00" || chain.Revenue[0].Attribution == nil || *chain.Revenue[0].Attribution != "RECOVERED" ||
		strings.Contains(trace.Body.String(), "Нужна полировка") {
		t.Fatalf("трасса: %s", trace.Body.String())
	}
	requireStatus(t, request(t, fixture.handler, http.MethodGet, "/api/v1/admin/trace/tenants/"+tenantID+"/messages/"+riskID, "", operator.Cookie, ""), http.StatusNotFound)

	// Искусственно сломанный контур: мёртвое задание видно и чинится без SQL.
	deadJobID, _ := (ids.Generator{}).NewID()
	if _, err := fixture.pool.Exec(context.Background(), `
		INSERT INTO jobs(id, tenant_id, job_type, dedup_key, payload, status, priority, available_at, attempt_count, max_attempts,
		                 last_error_code, completed_at, created_at, updated_at)
		VALUES ($1, $2, 'risk.evaluate-no-response.v1', 'admin-broken-pipeline', $3::jsonb, 'DEAD', 0, now(), 5, 5, 'SIMULATED_CRASH', now(), now(), now())`,
		deadJobID, tenantID, `{"opportunityId":"`+opportunityID+`"}`); err != nil {
		t.Fatal(err)
	}
	queue := request(t, fixture.handler, http.MethodGet, "/api/v1/admin/queue", "", operator.Cookie, "")
	requireStatus(t, queue, http.StatusOK)
	if !strings.Contains(queue.Body.String(), `"deadUnhandled":1`) {
		t.Fatalf("очереди: %s", queue.Body.String())
	}
	dead := request(t, fixture.handler, http.MethodGet, "/api/v1/admin/dead-letters", "", operator.Cookie, "")
	requireStatus(t, dead, http.StatusOK)
	if body := dead.Body.String(); !strings.Contains(body, `"id":"`+deadJobID+`"`) || !strings.Contains(body, `"lastErrorCode":"SIMULATED_CRASH"`) {
		t.Fatalf("мёртвые элементы: %s", body)
	}
	requireStatus(t, request(t, fixture.handler, http.MethodPost, "/api/v1/admin/jobs/"+deadJobID+"/retry", "", owner.Cookie, ""), http.StatusForbidden)
	retried := request(t, fixture.handler, http.MethodPost, "/api/v1/admin/jobs/"+deadJobID+"/retry", "", operator.Cookie, "")
	requireStatus(t, retried, http.StatusOK)
	if !strings.Contains(retried.Body.String(), `"status":"PENDING"`) {
		t.Fatalf("повтор задания: %s", retried.Body.String())
	}
	requireStatus(t, request(t, fixture.handler, http.MethodPost, "/api/v1/admin/jobs/"+deadJobID+"/retry", "", operator.Cookie, ""), http.StatusConflict)
	processExactly(t, fixture, 1)
	var status string
	if err := fixture.pool.QueryRow(context.Background(), `SELECT status FROM jobs WHERE id = $1`, deadJobID).Scan(&status); err != nil || status != "SUCCEEDED" {
		t.Fatalf("повторённое задание: %s, %v", status, err)
	}
	requireStatus(t, request(t, fixture.handler, http.MethodGet, "/api/v1/admin/dead-letters", "", operator.Cookie, ""), http.StatusOK)
	if body := request(t, fixture.handler, http.MethodGet, "/api/v1/admin/dead-letters", "", operator.Cookie, "").Body.String(); strings.Contains(body, deadJobID) {
		t.Fatalf("починенное задание осталось в панели: %s", body)
	}

	usage := request(t, fixture.handler, http.MethodGet, "/api/v1/admin/usage", "", operator.Cookie, "")
	requireStatus(t, usage, http.StatusOK)
	var usageReport struct {
		Tenants []struct {
			TenantID string `json:"tenantId"`
			Messages int    `json:"messages"`
			Jobs     int    `json:"jobs"`
			Risks    int    `json:"risks"`
		} `json:"tenants"`
	}
	if err := json.Unmarshal(usage.Body.Bytes(), &usageReport); err != nil {
		t.Fatal(err)
	}
	usageSeen := false
	for _, tenant := range usageReport.Tenants {
		if tenant.TenantID == tenantID {
			usageSeen = tenant.Messages >= 1 && tenant.Jobs >= 3 && tenant.Risks == 1
		}
	}
	if !usageSeen {
		t.Fatalf("потребление: %s", usage.Body.String())
	}
	requireStatus(t, request(t, fixture.handler, http.MethodGet, "/api/v1/admin/usage?from=2026-09-10T00:00:00Z&to=2026-09-01T00:00:00Z", "", operator.Cookie, ""), http.StatusBadRequest)
	nodes := request(t, fixture.handler, http.MethodGet, "/api/v1/admin/ai/nodes", "", operator.Cookie, "")
	requireStatus(t, nodes, http.StatusOK)
	requireStatus(t, request(t, fixture.handler, http.MethodGet, "/api/v1/admin/ai/runs?limit=5", "", operator.Cookie, ""), http.StatusOK)
	requireStatus(t, request(t, fixture.handler, http.MethodGet, "/api/v1/admin/ai/tenants/"+tenantID+"/conversations/"+opportunityID+"/summary", "", operator.Cookie, ""), http.StatusNotFound)

	// Выдача и отзыв через API с аудитом; отозванный оператор теряет доступ.
	granted := request(t, fixture.handler, http.MethodPost, "/api/v1/admin/admins", `{"email":"admin-owner@example.com","note":"дежурный"}`, operator.Cookie, "")
	requireStatus(t, granted, http.StatusCreated)
	requireStatus(t, request(t, fixture.handler, http.MethodPost, "/api/v1/admin/admins", `{"email":"admin-owner@example.com"}`, operator.Cookie, ""), http.StatusOK)
	requireStatus(t, request(t, fixture.handler, http.MethodDelete, "/api/v1/admin/admins/"+operator.ID, "", owner.Cookie, ""), http.StatusNoContent)
	requireStatus(t, request(t, fixture.handler, http.MethodGet, "/api/v1/admin/queue", "", operator.Cookie, ""), http.StatusForbidden)
	admins := request(t, fixture.handler, http.MethodGet, "/api/v1/admin/admins", "", owner.Cookie, "")
	requireStatus(t, admins, http.StatusOK)
	var audits int
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM admin_audit_log
		WHERE operation IN ('PLATFORM_ADMIN_GRANTED', 'PLATFORM_ADMIN_REVOKED', 'ADMIN_JOB_RETRIED')`).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 4 {
		t.Fatalf("аудит административных команд = %d", audits)
	}
}
