//go:build load

package integration_test

import (
	"bytes"
	"context"
	"fmt"
	"lidradar/backend/platform/tenantctx"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"
)

func TestLoadWebhookPersistenceBurst(t *testing.T) {
	fixture := newAPIFixture(t)
	owner := register(t, fixture.handler, "load-webhook@example.com", "Нагрузочный владелец")
	tenantID := createOrganization(t, fixture, owner, "Нагрузочная организация")
	locationID := createLocation(t, fixture, owner, tenantID, "Нагрузочная точка")
	secret := "load-webhook-secret-123"
	connected := request(t, fixture.handler, http.MethodPost, "/api/v1/integrations/GENERIC_WEBHOOK/connect", `{
		"name":"Нагрузочный вход","locationId":"`+locationID+`","webhookSecret":"`+secret+`"
	}`, owner.Cookie, tenantID)
	requireStatus(t, connected, http.StatusCreated)
	connectionID := jsonID(t, connected)
	path := "/api/v1/webhooks/GENERIC_WEBHOOK/" + tenantID + "/" + connectionID

	const requests = 150
	durations := make([]time.Duration, requests)
	errorsChannel := make(chan error, requests)
	started := time.Now()
	var wait sync.WaitGroup
	for index := 0; index < requests; index++ {
		payload := canonicalWebhook(
			fmt.Sprintf("load-event-%d", index), "message.received.v1", "load-dialog",
			fmt.Sprintf("load-message-%d", index), "load-contact", "INCOMING", "TEXT",
			"Нужна консультация", "2026-08-25T13:00:00Z", "",
		)
		wait.Add(1)
		go func(index int, payload string) {
			defer wait.Done()
			request := httptest.NewRequest(http.MethodPost, "http://api.example"+path, bytes.NewBufferString(payload))
			request.Header.Set("X-LidRadar-Webhook-Secret", secret)
			response := httptest.NewRecorder()
			requestStarted := time.Now()
			fixture.handler.ServeHTTP(response, request)
			durations[index] = time.Since(requestStarted)
			if response.Code != http.StatusAccepted {
				errorsChannel <- fmt.Errorf("запрос %d: статус %d, тело %s", index, response.Code, response.Body.String())
			}
		}(index, payload)
	}
	wait.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		t.Error(err)
	}
	if t.Failed() {
		t.FailNow()
	}

	var rawCount, outboxCount int
	if err := fixture.pool.QueryRow(context.Background(), `SELECT count(*) FROM raw_events WHERE connection_id = $1`, connectionID).Scan(&rawCount); err != nil {
		t.Fatal(err)
	}
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND aggregate_type = 'raw_event'`, tenantID).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if rawCount != requests || outboxCount != requests {
		t.Fatalf("raw=%d, outbox=%d, нужно %d", rawCount, outboxCount, requests)
	}
	t.Logf("150 параллельных webhook: всего=%s, p50=%s, p95=%s, max=%s",
		time.Since(started), percentile(durations, 50), percentile(durations, 95), percentile(durations, 100))
}

func TestLoadCommercialCandidateContention(t *testing.T) {
	fixture := newAPIFixture(t)
	owner := register(t, fixture.handler, "load-candidate@example.com", "Владелец кандидатов")
	tenantID := createOrganization(t, fixture, owner, "Организация кандидатов")
	locationID := createLocation(t, fixture, owner, tenantID, "Точка кандидатов")
	requireStatus(t, request(t, fixture.handler, http.MethodPost, "/api/v1/services", `{
		"name":"Полировка","locationId":"`+locationID+`","priceFrom":"5000","priceTo":"5000"
	}`, owner.Cookie, tenantID), http.StatusCreated)
	secret := "load-candidate-secret-123"
	connected := request(t, fixture.handler, http.MethodPost, "/api/v1/integrations/GENERIC_WEBHOOK/connect", `{
		"name":"Вход кандидатов","locationId":"`+locationID+`","webhookSecret":"`+secret+`"
	}`, owner.Cookie, tenantID)
	requireStatus(t, connected, http.StatusCreated)
	connectionID := jsonID(t, connected)
	path := "/api/v1/webhooks/GENERIC_WEBHOOK/" + tenantID + "/" + connectionID
	payload := canonicalWebhook(
		"candidate-load-event", "message.received.v1", "candidate-dialog", "candidate-message", "candidate-contact",
		"INCOMING", "TEXT", "Нужна полировка", "2026-08-25T13:10:00Z", "",
	)
	requireStatus(t, webhookRequest(t, fixture.handler, path, payload, "X-LidRadar-Webhook-Secret", secret), http.StatusAccepted)
	if dispatched, err := fixture.dispatcher.RunOne(context.Background()); err != nil || !dispatched {
		t.Fatalf("диспетчер = %v, %v", dispatched, err)
	}
	if processed, err := fixture.worker.RunOne(context.Background()); err != nil || !processed {
		t.Fatalf("канонизация = %v, %v", processed, err)
	}
	var conversationID string
	if err := fixture.pool.QueryRow(context.Background(), `SELECT id FROM conversations WHERE tenant_id = $1`, tenantID).Scan(&conversationID); err != nil {
		t.Fatal(err)
	}

	const workers = 120
	durations := make([]time.Duration, workers)
	errorsChannel := make(chan error, workers)
	createdChannel := make(chan bool, workers)
	var wait sync.WaitGroup
	started := time.Now()
	for index := 0; index < workers; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			requestStarted := time.Now()
			_, created, err := fixture.candidates.Evaluate(tenantctx.WithTenant(context.Background(), tenantID), tenantID, conversationID)
			durations[index] = time.Since(requestStarted)
			errorsChannel <- err
			createdChannel <- created
		}(index)
	}
	wait.Wait()
	close(errorsChannel)
	close(createdChannel)
	for err := range errorsChannel {
		if err != nil {
			t.Errorf("Evaluate(): %v", err)
		}
	}
	createdCount := 0
	for created := range createdChannel {
		if created {
			createdCount++
		}
	}
	var opportunities, history int
	if err := fixture.pool.QueryRow(context.Background(), `SELECT count(*) FROM opportunities WHERE tenant_id = $1`, tenantID).Scan(&opportunities); err != nil {
		t.Fatal(err)
	}
	if err := fixture.pool.QueryRow(context.Background(), `SELECT count(*) FROM opportunity_stage_history WHERE tenant_id = $1`, tenantID).Scan(&history); err != nil {
		t.Fatal(err)
	}
	if t.Failed() || createdCount != 1 || opportunities != 1 || history != 1 {
		t.Fatalf("created=%d, opportunities=%d, history=%d", createdCount, opportunities, history)
	}
	t.Logf("120 конкурентных кандидатов: всего=%s, p50=%s, p95=%s, max=%s",
		time.Since(started), percentile(durations, 50), percentile(durations, 95), percentile(durations, 100))
}

func percentile(values []time.Duration, percent int) time.Duration {
	ordered := append([]time.Duration(nil), values...)
	sort.Slice(ordered, func(left, right int) bool { return ordered[left] < ordered[right] })
	index := (len(ordered)*percent + 99) / 100
	if index < 1 {
		index = 1
	}
	return ordered[index-1]
}
