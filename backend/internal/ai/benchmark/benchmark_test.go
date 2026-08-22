package benchmark

import (
	"context"
	"strings"
	"testing"

	"lidradar/backend/internal/ai/infrastructure"
)

const dataset = `{"version":"lidradar-ai-benchmark.v1","id":"booking-001","split":"GOLDEN","input":{"task":"ANALYZE_CONVERSATION","schemaVersion":"analyze-conversation.v1","promptVersion":"analyze-conversation.prompt.v1","conversationId":"conversation-1","baseConversationRevision":1,"analysisThroughMessageId":"message-1","companyContext":"Детейлинг","messages":[{"id":"message-1","direction":"INCOMING","body":"Можно завтра?"}]},"expectedFacts":[{"type":"BOOKING_INTENT","value":true,"confidence":1,"evidenceMessageIds":["message-1"]}]}
`

func TestLoadRunAndGoldenProtection(t *testing.T) {
	cases, digest, err := Load(strings.NewReader(dataset))
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyGolden(digest, digest); err != nil {
		t.Fatal(err)
	}
	if err := VerifyGolden(digest, "bad"); err == nil {
		t.Fatal("expected checksum mismatch")
	}
	provider := infrastructure.FakeProvider{Output: `{"schemaVersion":"analyze-conversation.v1","analysisThroughMessageId":"message-1","summary":"Есть намерение записаться.","facts":[{"type":"BOOKING_INTENT","value":true,"confidence":0.95,"evidenceMessageIds":["message-1"]}]}`}
	report, err := Run(context.Background(), provider, cases, digest, Thresholds{MinimumPrecision: .9, MinimumRecall: .9, MinimumF1: .9, MinimumExactRate: .9})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed || report.Exact != 1 || report.F1 != 1 {
		t.Fatalf("unexpected report: %+v", report)
	}
}

func TestLoadRejectsDuplicateAndRunCountsInvalid(t *testing.T) {
	if _, _, err := Load(strings.NewReader(dataset + dataset)); err == nil {
		t.Fatal("expected duplicate rejection")
	}
	cases, digest, _ := Load(strings.NewReader(dataset))
	report, err := Run(context.Background(), infrastructure.FakeProvider{Output: `not json`}, cases, digest, Thresholds{MinimumRecall: 1})
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || report.Invalid != 1 || report.FalseNegative != 1 {
		t.Fatalf("unexpected report: %+v", report)
	}
}
