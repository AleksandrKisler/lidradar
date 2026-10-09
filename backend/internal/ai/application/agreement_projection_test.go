package application_test

import (
	"strings"
	"testing"

	"lidradar/backend/internal/ai/application"
	"lidradar/backend/internal/ai/domain"
)

func TestAgreementProjectionRetainsIndependentAnchorsAndProvenance(t *testing.T) {
	booking := domain.Agreement{Kind: domain.AgreementBookingConfirmation, WaitingFor: domain.AgreementCustomer, Status: domain.AgreementPending, TriggerMessageID: "offer", EvidenceMessageIDs: []string{"offer"}, Confidence: .96, Trusted: true}
	blocker := domain.Agreement{Kind: domain.AgreementPurchaseBlocker, WaitingFor: domain.AgreementBusiness, Status: domain.AgreementPending, TriggerMessageID: "blocker", EvidenceMessageIDs: []string{"blocker"}, Confidence: .95, Trusted: true}
	first := application.MergeAgreementProjection(nil, []domain.Agreement{booking, blocker}, nil, "", "run-1", "")
	if len(first) != 2 || first[0].SourceRunID != "run-1" || first[1].SourceRunID != "run-1" {
		t.Fatalf("first projection: %#v", first)
	}
	omitted := application.MergeAgreementProjection(first, nil, []bool{true, true}, "run-1", "run-2", "")
	if len(omitted) != 2 || omitted[0].Status != domain.AgreementPending || omitted[1].SourceRunID != "run-1" {
		t.Fatalf("omission erased pending: %#v", omitted)
	}
	weak := blocker
	weak.Trusted, weak.Confidence, weak.Status = false, .7, domain.AgreementResolved
	merged := application.MergeAgreementProjection(omitted, []domain.Agreement{weak}, []bool{true, true}, "run-2", "run-3", "")
	if merged[1].Status != domain.AgreementPending || !merged[1].Trusted || merged[1].SourceRunID != "run-1" {
		t.Fatalf("weak closed pending: %#v", merged)
	}
	merged = application.MergeAgreementProjection(merged, []domain.Agreement{blocker}, []bool{true, true}, "run-3", "run-3b", "")
	if merged[1].SourceRunID != "run-1" {
		t.Fatalf("repeat changed original provenance: %#v", merged)
	}
	resolved := blocker
	resolved.Status = domain.AgreementResolved
	resolved.EvidenceMessageIDs = []string{"blocker", "fixed"}
	merged = application.MergeAgreementProjection(merged, []domain.Agreement{resolved}, []bool{true, true}, "run-3", "run-4", "")
	if len(merged) != 2 || merged[0].Status != domain.AgreementPending || merged[1].Status != domain.AgreementResolved || merged[1].SourceRunID != "run-4" {
		t.Fatalf("resolution changed unrelated anchor: %#v", merged)
	}
	merged = application.MergeAgreementProjection(merged, []domain.Agreement{blocker}, []bool{true, true}, "run-4", "run-5", "")
	if merged[1].Status != domain.AgreementResolved || merged[1].SourceRunID != "run-4" {
		t.Fatalf("terminal reopened: %#v", merged)
	}
}

func TestAgreementSourceRunIDRemainsServerOwned(t *testing.T) {
	raw := `{"schemaVersion":"analyze-conversation.v2","analysisThroughMessageId":"m","summary":"Test","facts":[],"agreements":[{"kind":"COMMITMENT","waitingFor":"BUSINESS","status":"PENDING","triggerMessageId":"m","evidenceMessageIds":["m"],"confidence":0.96,"sourceRunId":"forged"}]}`
	for _, field := range []string{"sourceRunId", "supersededByTriggerMessageId"} {
		_, err := application.ValidateAnalysisResultV2(strings.ReplaceAll(raw, "sourceRunId", field), "m")
		if err == nil || !strings.Contains(err.Error(), field) {
			t.Fatalf("model supplied %s accepted: %v", field, err)
		}
	}
}

func TestAgreementProjectionKeepsMultiplePromises(t *testing.T) {
	first := domain.Agreement{Kind: domain.AgreementCommitment, WaitingFor: domain.AgreementBusiness, Status: domain.AgreementPending, TriggerMessageID: "promise-1", EvidenceMessageIDs: []string{"promise-1"}, Confidence: .96, Trusted: true}
	second := first
	second.TriggerMessageID, second.EvidenceMessageIDs = "promise-2", []string{"promise-2"}
	got := application.MergeAgreementProjection([]domain.Agreement{first}, []domain.Agreement{second}, []bool{true}, "origin", "new", "")
	if len(got) != 2 || got[0].SourceRunID != "origin" || got[1].SourceRunID != "new" {
		t.Fatalf("independent promises: %#v", got)
	}
}

func TestAgreementProjectionInvalidatesEditedOriginWithoutInventingStatus(t *testing.T) {
	old := domain.Agreement{Kind: domain.AgreementCommitment, WaitingFor: domain.AgreementBusiness, Status: domain.AgreementPending, TriggerMessageID: "promise", EvidenceMessageIDs: []string{"promise"}, Confidence: .97, Trusted: true}
	got := application.MergeAgreementProjection([]domain.Agreement{old}, nil, []bool{false}, "legacy-run", "new-run", "")
	if len(got) != 1 || got[0].Trusted || got[0].Status != domain.AgreementPending || got[0].SourceRunID != "legacy-run" {
		t.Fatalf("invalid evidence: %#v", got)
	}
	old.Status = domain.AgreementResolved
	got = application.MergeAgreementProjection([]domain.Agreement{old}, []domain.Agreement{{Kind: old.Kind, WaitingFor: old.WaitingFor,
		Status: domain.AgreementPending, TriggerMessageID: old.TriggerMessageID, EvidenceMessageIDs: old.EvidenceMessageIDs,
		Confidence: .96, Trusted: true}}, []bool{false}, "legacy-run", "new-run", "")
	if len(got) != 1 || got[0].Status != domain.AgreementResolved || got[0].Trusted || got[0].SourceRunID != "legacy-run" {
		t.Fatalf("invalid terminal anchor reopened: %#v", got)
	}
}

func TestAgreementProjectionBookingContinuationRequiresMatchingFlow(t *testing.T) {
	old := domain.Agreement{Kind: domain.AgreementBookingConfirmation, WaitingFor: domain.AgreementBusiness, Status: domain.AgreementPending, TriggerMessageID: "intent", EvidenceMessageIDs: []string{"intent"}, Confidence: .96, Trusted: true, SourceRunID: "first"}
	newOffer := domain.Agreement{Kind: domain.AgreementBookingConfirmation, WaitingFor: domain.AgreementCustomer, Status: domain.AgreementPending, TriggerMessageID: "offer", EvidenceMessageIDs: []string{"offer"}, Confidence: .97, Trusted: true}
	prompt, err := application.EncodeAnalysisRequest(application.AnalyzeConversationRequestV1{Messages: []application.ContextMessage{{ID: "intent", Direction: "INCOMING", Body: "Хочу записаться."}, {ID: "offer", Direction: "OUTGOING", Body: "Свободно завтра. Вам подходит?"}}})
	if err != nil {
		t.Fatal(err)
	}
	got := application.MergeAgreementProjection([]domain.Agreement{old}, []domain.Agreement{newOffer}, []bool{true}, "first", "second", prompt)
	if len(got) != 2 || got[0].SupersededByTriggerMessageID != "offer" || got[0].SourceRunID != "first" || got[1].TriggerMessageID != "offer" {
		t.Fatalf("same booking flow: %#v", got)
	}
	got = application.MergeAgreementProjection([]domain.Agreement{old}, []domain.Agreement{newOffer}, []bool{true}, "first", "second", "")
	if len(got) != 2 {
		t.Fatalf("ambiguous booking flow was superseded: %#v", got)
	}
	intervening, err := application.EncodeAnalysisRequest(application.AnalyzeConversationRequestV1{Messages: []application.ContextMessage{
		{ID: "intent", Direction: "INCOMING", Body: "Хочу записаться."},
		{ID: "other", Direction: "INCOMING", Body: "Есть вопрос по оплате."},
		{ID: "offer", Direction: "OUTGOING", Body: "Свободно завтра. Вам подходит?"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	got = application.MergeAgreementProjection([]domain.Agreement{old}, []domain.Agreement{newOffer}, []bool{true}, "first", "second", intervening)
	if len(got) != 2 {
		t.Fatalf("nonadjacent booking flow was superseded: %#v", got)
	}
	commitment := old
	commitment.Kind = domain.AgreementCommitment
	got = application.MergeAgreementProjection([]domain.Agreement{commitment}, []domain.Agreement{newOffer}, []bool{true}, "first", "second", prompt)
	if len(got) != 2 {
		t.Fatalf("unrelated commitment was superseded: %#v", got)
	}
}
