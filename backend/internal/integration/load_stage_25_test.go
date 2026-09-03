//go:build load

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	aiapplication "lidradar/backend/internal/ai/application"
	aidomain "lidradar/backend/internal/ai/domain"
	aiinfrastructure "lidradar/backend/internal/ai/infrastructure"
	identityinfrastructure "lidradar/backend/internal/identity/infrastructure"
	identitytransport "lidradar/backend/internal/identity/transport"
	jobsdomain "lidradar/backend/internal/jobs/domain"
	jobsinfrastructure "lidradar/backend/internal/jobs/infrastructure"
	"lidradar/backend/internal/loadgen"
	"lidradar/backend/internal/testsupport"
	"lidradar/backend/platform/ids"
)

func envInt(name string, fallback int) int {
	if raw := os.Getenv(name); raw != "" {
		if value, err := strconv.Atoi(raw); err == nil && value > 0 {
			return value
		}
	}
	return fallback
}

type latencySummary struct {
	Count int     `json:"count"`
	P50Ms float64 `json:"p50Ms"`
	P95Ms float64 `json:"p95Ms"`
	P99Ms float64 `json:"p99Ms"`
	MaxMs float64 `json:"maxMs"`
}

func summarize(durations []time.Duration) latencySummary {
	if len(durations) == 0 {
		return latencySummary{}
	}
	sorted := append([]time.Duration(nil), durations...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	pick := func(percentile float64) float64 {
		return float64(sorted[int(float64(len(sorted)-1)*percentile/100)].Microseconds()) / 1000
	}
	return latencySummary{Count: len(sorted), P50Ms: pick(50), P95Ms: pick(95), P99Ms: pick(99), MaxMs: pick(100)}
}

func (summary latencySummary) String() string {
	return fmt.Sprintf("n=%d p50=%.1fms p95=%.1fms p99=%.1fms max=%.1fms", summary.Count, summary.P50Ms, summary.P95Ms, summary.P99Ms, summary.MaxMs)
}

// drain выполняет диспетчер и worker до пустых очередей, считая задания.
func drain(ctx context.Context, fixture apiFixture, goroutines int) (int64, time.Duration, error) {
	var processed atomic.Int64
	var failure atomic.Value
	started := time.Now()
	var wait sync.WaitGroup
	for index := 0; index < goroutines; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			idle := 0
			for idle < 3 {
				dispatched, err := fixture.dispatcher.RunOne(ctx)
				if err != nil {
					failure.Store(err)
					return
				}
				done, err := fixture.worker.RunOne(ctx)
				if err != nil {
					failure.Store(err)
					return
				}
				if done {
					processed.Add(1)
				}
				if !dispatched && !done {
					idle++
					time.Sleep(20 * time.Millisecond)
				} else {
					idle = 0
				}
			}
		}()
	}
	wait.Wait()
	if err, ok := failure.Load().(error); ok && err != nil {
		return processed.Load(), time.Since(started), err
	}
	return processed.Load(), time.Since(started), nil
}

// TestLoadCapacityBaseline измеряет базовые показатели этапа 25 (ТЗ §72) на
// синтетическом наборе: API p50/p95/p99, DB p95, вебхуки, пропускную
// способность worker, лаг планировщика и AI-очередь с имитацией узла.
func TestLoadCapacityBaseline(t *testing.T) {
	fixture := newAPIFixture(t)
	ctx := context.Background()
	plan := loadgen.Plan{
		Label: "cap", Organizations: envInt("LIDRADAR_LOAD_ORGANIZATIONS", 20),
		Conversations: envInt("LIDRADAR_LOAD_CONVERSATIONS", 50), Messages: envInt("LIDRADAR_LOAD_MESSAGES", 10),
	}
	dataset, err := loadgen.Generate(ctx, fixture.pool, plan)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("набор: %s", dataset.Summary())
	report := map[string]any{
		"dataset": map[string]any{
			"organizations": len(dataset.Organizations), "conversations": dataset.Conversations, "messages": dataset.Messages,
			"opportunities": dataset.Opportunities, "risks": dataset.Risks, "generationMs": dataset.Duration.Milliseconds(),
		},
		"targets": map[string]any{"apiP95Ms": 300, "webhookP95Ms": 200, "ruleRiskAfterDueS": 10, "aiQueueP95S": 60},
	}

	tokens := identityinfrastructure.SessionTokens{}
	cookies := make(map[string]*http.Cookie, len(dataset.Organizations))
	for _, organization := range dataset.Organizations {
		plaintext, hash, err := tokens.NewToken()
		if err != nil {
			t.Fatal(err)
		}
		sessionID, _ := (ids.Generator{}).NewID()
		if _, err := fixture.pool.Exec(ctx, `
			INSERT INTO sessions(id, user_id, token_hash, expires_at, created_at) VALUES ($1, $2, $3, $4, $5)`,
			sessionID, organization.UserID, hash, time.Now().UTC().Add(24*time.Hour), time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		cookies[organization.TenantID] = &http.Cookie{Name: identitytransport.SessionCookieName, Value: plaintext}
	}

	concurrency := envInt("LIDRADAR_LOAD_CONCURRENCY", 16)
	perEndpoint := envInt("LIDRADAR_LOAD_REQUESTS", 300)
	endpoints := []struct {
		name string
		path func(loadgen.Organization) string
	}{
		{"radar", func(loadgen.Organization) string { return "/api/v1/radar" }},
		{"risks", func(loadgen.Organization) string { return "/api/v1/risks?limit=20" }},
		{"conversations", func(loadgen.Organization) string { return "/api/v1/conversations?limit=20" }},
		{"messages", func(o loadgen.Organization) string {
			return "/api/v1/conversations/" + o.SampleConversationID + "/messages"
		}},
		{"opportunity", func(o loadgen.Organization) string { return "/api/v1/opportunities/" + o.SampleOpportunityID }},
		{"analytics", func(loadgen.Organization) string { return "/api/v1/analytics/summary" }},
	}
	testsupport.LoadTrace.Reset()
	apiReport := map[string]latencySummary{}
	apiStarted := time.Now()
	var apiRequests int
	for _, endpoint := range endpoints {
		durations := make([]time.Duration, perEndpoint)
		var failures atomic.Int64
		var wait sync.WaitGroup
		queue := make(chan int, perEndpoint)
		for index := 0; index < perEndpoint; index++ {
			queue <- index
		}
		close(queue)
		for worker := 0; worker < concurrency; worker++ {
			wait.Add(1)
			go func() {
				defer wait.Done()
				for index := range queue {
					organization := dataset.Organizations[index%len(dataset.Organizations)]
					if endpoint.name == "opportunity" && organization.SampleOpportunityID == "" {
						continue
					}
					started := time.Now()
					response := request(t, fixture.handler, http.MethodGet, endpoint.path(organization), "", cookies[organization.TenantID], organization.TenantID)
					durations[index] = time.Since(started)
					if response.Code != http.StatusOK {
						failures.Add(1)
					}
				}
			}()
		}
		wait.Wait()
		if failures.Load() != 0 {
			t.Fatalf("%s: %d ошибочных ответов", endpoint.name, failures.Load())
		}
		summary := summarize(durations)
		apiReport[endpoint.name] = summary
		apiRequests += summary.Count
		t.Logf("API %-13s %s", endpoint.name, summary)
		if summary.P95Ms > 300 {
			t.Errorf("API %s: p95 %.1f мс превышает цель 300 мс (§72)", endpoint.name, summary.P95Ms)
		}
	}
	report["api"] = apiReport
	report["apiRequestsPerSecond"] = float64(apiRequests) / time.Since(apiStarted).Seconds()
	dbP95, dbQueries := testsupport.LoadTrace.Overall(95)
	if testsupport.TraceEnabled() {
		top := testsupport.LoadTrace.Top(12)
		rows := make([]map[string]any, 0, len(top))
		for _, item := range top {
			rows = append(rows, map[string]any{"sql": item.SQL, "count": item.Count, "p50Ms": float64(item.P50.Microseconds()) / 1000, "p95Ms": float64(item.P95.Microseconds()) / 1000, "maxMs": float64(item.Max.Microseconds()) / 1000})
			t.Logf("DB n=%-6d p50=%6.2fms p95=%7.2fms %s", item.Count, float64(item.P50.Microseconds())/1000, float64(item.P95.Microseconds())/1000, item.SQL)
		}
		report["db"] = map[string]any{"queries": dbQueries, "p95Ms": float64(dbP95.Microseconds()) / 1000, "top": rows}
		t.Logf("DB всего запросов=%d p95=%s", dbQueries, dbP95)
	}

	burst := envInt("LIDRADAR_LOAD_WEBHOOKS", 400)
	webhookRound := func(round int) latencySummary {
		durations := make([]time.Duration, burst)
		var failures atomic.Int64
		var firstFailure atomic.Pointer[string]
		queue := make(chan int, burst)
		for index := 0; index < burst; index++ {
			queue <- index
		}
		close(queue)
		var wait sync.WaitGroup
		sentAt := time.Now().UTC().Add(-61 * time.Minute).Format(time.RFC3339Nano)
		for worker := 0; worker < 32; worker++ {
			wait.Add(1)
			go func() {
				defer wait.Done()
				for index := range queue {
					organization := dataset.Organizations[index%len(dataset.Organizations)]
					path := "/api/v1/webhooks/GENERIC_WEBHOOK/" + organization.TenantID + "/" + organization.ConnectionID
					label := fmt.Sprintf("cap-burst-%d-%d", round, index)
					payload := canonicalWebhook(label+"-event", "message.received.v1", label+"-dialog", label+"-message", label+"-contact",
						"INCOMING", "TEXT", "Нужна полировка, подскажите цену", sentAt, "")
					started := time.Now()
					response := webhookRequest(t, fixture.handler, path, payload, "X-LidRadar-Webhook-Secret", "load-webhook-secret-1234567890")
					durations[index] = time.Since(started)
					if response.Code != http.StatusAccepted {
						failures.Add(1)
						detail := fmt.Sprintf("HTTP %d %s", response.Code, response.Body.String())
						firstFailure.CompareAndSwap(nil, &detail)
					}
				}
			}()
		}
		wait.Wait()
		if failures.Load() != 0 {
			t.Fatalf("вебхуки, раунд %d: %d ошибочных ответов, первый: %s", round, failures.Load(), *firstFailure.Load())
		}
		return summarize(durations)
	}
	webhookSummary := webhookRound(1)
	t.Logf("webhook burst %d параллельно 32: %s", burst, webhookSummary)
	if webhookSummary.P95Ms > 200 {
		t.Errorf("webhook persist p95 %.1f мс превышает цель 200 мс (§72)", webhookSummary.P95Ms)
	}
	report["webhook"] = webhookSummary
	processedSingle, elapsedSingle, err := drain(ctx, fixture, 1)
	if err != nil {
		t.Fatal(err)
	}
	singleRate := float64(processedSingle) / elapsedSingle.Seconds()
	t.Logf("worker ×1: заданий=%d за %s (%.1f заданий/с)", processedSingle, elapsedSingle.Round(time.Millisecond), singleRate)
	secondBurst := webhookRound(2)
	processedParallel, elapsedParallel, err := drain(ctx, fixture, 4)
	if err != nil {
		t.Fatal(err)
	}
	parallelRate := float64(processedParallel) / elapsedParallel.Seconds()
	t.Logf("worker ×4: заданий=%d за %s (%.1f заданий/с); второй burst %s", processedParallel, elapsedParallel.Round(time.Millisecond), parallelRate, secondBurst)
	report["worker"] = map[string]any{
		"singleJobs": processedSingle, "singleJobsPerSecond": singleRate,
		"parallelJobs": processedParallel, "parallelJobsPerSecond": parallelRate, "parallelWorkers": 4,
	}

	// Лаг планировщика: проверки со сроком «сейчас» по сгенерированным сделкам.
	jobStore := jobsinfrastructure.NewPostgresStore(fixture.pool)
	scheduled := 0
	dueAt := time.Now().UTC()
	for _, organization := range dataset.Organizations {
		for _, opportunityID := range organization.OpportunityIDs {
			if scheduled >= 200 {
				break
			}
			checkID, _ := (ids.Generator{}).NewID()
			payload, _ := json.Marshal(map[string]string{"opportunityId": opportunityID})
			check, err := jobsdomain.NewScheduledCheck(checkID, organization.TenantID, "NO_RESPONSE_DUE", "opportunity", opportunityID,
				"risk.evaluate-no-response.v1", "cap-lag:"+opportunityID, payload, dueAt, dueAt)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := jobStore.Schedule(ctx, check); err != nil {
				t.Fatal(err)
			}
			scheduled++
		}
	}
	promotionStarted := time.Now()
	for {
		promoted, err := fixture.scheduler.RunOnce(ctx, 100)
		if err != nil {
			t.Fatal(err)
		}
		if promoted == 0 {
			break
		}
	}
	promotionElapsed := time.Since(promotionStarted)
	if _, _, err := drain(ctx, fixture, 2); err != nil {
		t.Fatal(err)
	}
	rows, err := fixture.pool.Query(ctx, `
		SELECT EXTRACT(EPOCH FROM (job.created_at - check_row.due_at)), EXTRACT(EPOCH FROM (job.completed_at - check_row.due_at))
		FROM scheduled_checks AS check_row
		JOIN jobs AS job ON job.tenant_id = check_row.tenant_id AND job.id = check_row.job_id
		WHERE check_row.dedup_key LIKE 'cap-lag:%' AND job.completed_at IS NOT NULL`)
	if err != nil {
		t.Fatal(err)
	}
	var promotionLags, completionLags []time.Duration
	for rows.Next() {
		var promotion, completion float64
		if err := rows.Scan(&promotion, &completion); err != nil {
			t.Fatal(err)
		}
		promotionLags = append(promotionLags, time.Duration(promotion*float64(time.Second)))
		completionLags = append(completionLags, time.Duration(completion*float64(time.Second)))
	}
	rows.Close()
	promotionSummary, completionSummary := summarize(promotionLags), summarize(completionLags)
	t.Logf("scheduler: проверок=%d перенос за %s; лаг переноса %s; лаг выполнения %s", scheduled, promotionElapsed.Round(time.Millisecond), promotionSummary, completionSummary)
	if completionSummary.Count != scheduled {
		t.Errorf("выполнено проверок %d из %d", completionSummary.Count, scheduled)
	}
	if completionSummary.P95Ms > 10000 {
		t.Errorf("правило создаёт риск через %.0f мс после срока, цель 10 с (§72)", completionSummary.P95Ms)
	}
	report["scheduler"] = map[string]any{"checks": scheduled, "promotionMs": promotionElapsed.Milliseconds(), "promotionLag": promotionSummary, "completionLag": completionSummary}

	// AI-очередь: имитация узла забирает и завершает задания без вывода фактов.
	aiService := aiapplication.NewService(aiinfrastructure.NewPostgresStore(fixture.pool), ids.Generator{}, time.Now, aiapplication.DefaultLease)
	nodeSecret := "load-node-secret-with-at-least-32-characters"
	node, err := aiService.RegisterNode(ctx, dataset.Organizations[0].TenantID, "LOAD-NODE", nodeSecret)
	if err != nil {
		t.Fatal(err)
	}
	for _, organization := range dataset.Organizations[1:] {
		if err := aiService.AllowNodeTenant(ctx, node.ID, organization.TenantID); err != nil {
			t.Fatal(err)
		}
	}
	ready := aiapplication.HeartbeatCommand{Status: aidomain.NodeReady, ModelVersion: aiapplication.DefaultModelVersion, AvailableSlots: 1}
	var queuedBefore int
	if err := fixture.pool.QueryRow(ctx, `SELECT count(*) FROM ai_jobs WHERE status IN ('PENDING', 'RETRY')`).Scan(&queuedBefore); err != nil {
		t.Fatal(err)
	}
	aiStarted := time.Now()
	completed := 0
	for {
		if err := aiService.Heartbeat(ctx, node.ID, nodeSecret, ready); err != nil {
			t.Fatal(err)
		}
		job, found, err := aiService.Claim(ctx, node.ID, nodeSecret)
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			break
		}
		run, err := aiService.Started(ctx, node.ID, nodeSecret, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		output := `{"schemaVersion":"analyze-conversation.v1","analysisThroughMessageId":"` + job.AnalysisThroughMessageID +
			`","summary":"Клиент интересуется полировкой и ждёт предложение.","facts":[]}`
		if _, err := aiService.Complete(ctx, node.ID, nodeSecret, job.ID, run.ID, output); err != nil {
			t.Fatal(err)
		}
		completed++
	}
	aiElapsed := time.Since(aiStarted)
	if _, _, err := drain(ctx, fixture, 2); err != nil {
		t.Fatal(err)
	}
	rows, err = fixture.pool.Query(ctx, `
		SELECT EXTRACT(EPOCH FROM (run.started_at - job.created_at)), EXTRACT(EPOCH FROM (run.completed_at - run.started_at))
		FROM ai_jobs AS job JOIN ai_runs AS run ON run.tenant_id = job.tenant_id AND run.job_id = job.id
		WHERE job.status = 'SUCCEEDED' AND run.completed_at IS NOT NULL`)
	if err != nil {
		t.Fatal(err)
	}
	var waits, inferences []time.Duration
	for rows.Next() {
		var wait, inference float64
		if err := rows.Scan(&wait, &inference); err != nil {
			t.Fatal(err)
		}
		waits = append(waits, time.Duration(wait*float64(time.Second)))
		inferences = append(inferences, time.Duration(inference*float64(time.Second)))
	}
	rows.Close()
	waitSummary, inferenceSummary := summarize(waits), summarize(inferences)
	t.Logf("AI: в очереди было %d, завершено %d за %s (%.1f/с); ожидание %s; имитация вывода %s", queuedBefore, completed, aiElapsed.Round(time.Millisecond), float64(completed)/aiElapsed.Seconds(), waitSummary, inferenceSummary)
	if waitSummary.P95Ms > 60000 {
		t.Errorf("AI queue p95 wait %.0f мс превышает триггер 60 с (§73)", waitSummary.P95Ms)
	}
	report["ai"] = map[string]any{"queued": queuedBefore, "completed": completed, "simulatedJobsPerSecond": float64(completed) / aiElapsed.Seconds(),
		"queueWait": waitSummary, "simulatedInference": inferenceSummary, "note": "узел имитирован; реальная задержка вывода — этап 15 (RTX 4060)"}

	// Профили ключевых запросов: индексные обращения к сообщениям и рискам.
	sample := dataset.Organizations[0]
	plans := map[string]string{
		"lastMessage":   `EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT) SELECT m.id, m.sent_at FROM messages AS m WHERE m.tenant_id = '` + sample.TenantID + `' AND m.conversation_id = '` + sample.SampleConversationID + `' AND m.provider_deleted_at IS NULL AND m.direction IN ('INCOMING','OUTGOING') ORDER BY m.sent_at DESC, m.id DESC LIMIT 1`,
		"radarRisks":    `EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT) SELECT id FROM risk_signals WHERE tenant_id = '` + sample.TenantID + `' AND status IN ('OPEN','ACKNOWLEDGED','ACTED') ORDER BY detected_at DESC LIMIT 20`,
		"messagesCount": `EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT) SELECT count(*) FROM messages WHERE tenant_id = '` + sample.TenantID + `' AND sent_at >= now() - interval '30 days' AND sent_at < now()`,
	}
	explains := map[string]string{}
	for name, statement := range plans {
		planRows, err := fixture.pool.Query(ctx, statement)
		if err != nil {
			t.Fatal(err)
		}
		var lines []string
		for planRows.Next() {
			var line string
			if err := planRows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			lines = append(lines, line)
		}
		planRows.Close()
		plan := strings.Join(lines, "\n")
		explains[name] = plan
		t.Logf("EXPLAIN %s:\n%s", name, plan)
		if name == "lastMessage" && strings.Contains(plan, "Seq Scan on messages") {
			t.Errorf("последнее сообщение читается полным сканом: %s", plan)
		}
	}
	report["explain"] = explains

	if path := os.Getenv("LIDRADAR_LOAD_REPORT"); path != "" {
		encoded, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, encoded, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("отчёт записан: %s", path)
	}
}
