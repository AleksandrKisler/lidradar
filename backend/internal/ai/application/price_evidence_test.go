package application_test

import (
	"context"
	"encoding/json"
	"testing"

	"lidradar/backend/internal/ai/application"
	"lidradar/backend/internal/ai/domain"
)

func TestPriceFactRequiresAmountInCitedMessage(t *testing.T) {
	cases := []struct {
		name, body, amount string
		accept             bool
	}{
		{"QA-06 invented zero", "Здравствуйте, подскажите стоимость полировки.", "0", false},
		{"invented catalog price", "Сколько стоит полировка?", "5000", false},
		{"price list request", "Пришлите прайс", "0", false},
		{"invented amount", "Стоимость 5000 рублей", "7000", false},
		{"substring", "Стоимость 15000 рублей", "5000", false},
		{"time", "Можно узнать цену к 16:00?", "16", false},
		{"fraction of decimal", "Цена 3500,50 ₽", "3500", false},
		{"integer", "Полировка стоит 5000 рублей.", "5000.00", true},
		{"comma decimal", "Стоимость 3500,50 ₽", "3500.50", true},
		{"comma model output", "Стоимость 3500,50 ₽", "3500,50", true},
		{"dot decimal", "Стоимость 3500.50 RUB", "3500.5", true},
		{"grouped", "Цена 3 500 руб.", "3500", true},
		{"nonbreaking space", "Цена 3\u00a0500,50 руб.", "3500.50", true},
		{"narrow nonbreaking space", "Цена 3\u202f500 руб.", "3500", true},
		{"explicit zero", "Стоимость 0 рублей по акции", "0", true},
		{"incoming budget", "Мой бюджет 7000 рублей", "7000", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checkPriceCompletion(t, []application.ContextMessage{{ID: "message", Direction: "INCOMING", Body: tc.body}}, []string{"message"}, tc.amount, tc.accept)
		})
	}
	t.Run("unknown evidence", func(t *testing.T) {
		checkPriceCompletion(t, []application.ContextMessage{{ID: "message", Direction: "OUTGOING", Body: "Цена 5000 рублей"}}, []string{"unknown"}, "5000", false)
	})
	t.Run("price in uncited message cannot prove question", func(t *testing.T) {
		checkPriceCompletion(t, []application.ContextMessage{
			{ID: "quote", Direction: "OUTGOING", Body: "Цена 5000 рублей"},
			{ID: "message", Direction: "INCOMING", Body: "Подскажите стоимость полировки"},
		}, []string{"message"}, "5000", false)
	})
	t.Run("question cannot be added to valid evidence", func(t *testing.T) {
		checkPriceCompletion(t, []application.ContextMessage{
			{ID: "question", Direction: "INCOMING", Body: "Сколько стоит полировка?"},
			{ID: "message", Direction: "OUTGOING", Body: "Цена 5000 рублей"},
		}, []string{"question", "message"}, "5000", false)
	})
}

func checkPriceCompletion(t *testing.T, messages []application.ContextMessage, evidence []string, amount string, accept bool) {
	t.Helper()
	svc, store, _ := setup(t)
	ctx := context.Background()
	prompt, err := application.EncodeAnalysisRequest(application.AnalyzeConversationRequestV1{
		Task: application.JobTypeAnalyze, SchemaVersion: application.AnalysisSchemaV1,
		PromptVersion: application.CurrentAnalysisPrompt, ConversationID: "conversation",
		BaseConversationRevision: 2, AnalysisThroughMessageID: "message",
		CompanyContext: "Полировка: 5000 RUB", ConversationSummary: "Ранее упоминали 7000 рублей", Messages: messages,
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err := svc.Enqueue(ctx, application.EnqueueCommand{TenantID: "tenant", ConversationID: "conversation", Prompt: prompt,
		BaseConversationRevision: 2, AnalysisThroughMessageID: "message", ModelVersion: "test-model"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Heartbeat(ctx, "1", nodeSecret, application.HeartbeatCommand{Status: domain.NodeReady, ModelVersion: "test-model", AvailableSlots: 1}); err != nil {
		t.Fatal(err)
	}
	if _, found, err := svc.Claim(ctx, "1", nodeSecret); err != nil || !found {
		t.Fatalf("claim: %v %v", found, err)
	}
	run, err := svc.Started(ctx, "1", nodeSecret, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(domain.AnalysisResultV1{SchemaVersion: application.AnalysisSchemaV1, AnalysisThroughMessageID: "message",
		Summary: "Результат QA-06", Facts: []domain.SemanticFact{{Type: domain.FactPriceMentioned, Value: true, Confidence: .99,
			Amount: &amount, Currency: "RUB", EvidenceMessageIDs: evidence}}})
	if err != nil {
		t.Fatal(err)
	}
	done, err := svc.Complete(ctx, "1", nodeSecret, job.ID, run.ID, string(raw))
	if err != nil {
		t.Fatal(err)
	}
	want := domain.ApplicationRejected
	if accept {
		want = domain.ApplicationApplied
	}
	if done.ApplicationStatus != want {
		t.Fatalf("application status = %s, want %s", done.ApplicationStatus, want)
	}
	summary, exists := store.Summary("tenant", "conversation")
	if exists != accept {
		t.Fatalf("summary exists = %v, want %v", exists, accept)
	}
	if accept && (len(summary.Facts) != 1 || !summary.Facts[0].Trusted) {
		t.Fatalf("lost positive price: %#v", summary.Facts)
	}
	if done.Output != string(raw) {
		t.Fatal("original model response must be preserved for audit")
	}
	if !accept && done.ValidationError == "" {
		t.Fatal("rejection must be auditable")
	}
	// Повтор завершения не создаёт доверенный факт и не меняет решение.
	repeated, err := svc.Complete(ctx, "1", nodeSecret, job.ID, run.ID, string(raw))
	if err != nil || repeated.ApplicationStatus != want {
		t.Fatalf("repeat: %#v %v", repeated, err)
	}
}
