package application_test

import (
	"testing"

	"lidradar/backend/internal/ai/application"
	"lidradar/backend/internal/ai/domain"
)

func promptWithVersion(t *testing.T, version string, messages ...application.ContextMessage) string {
	t.Helper()
	p, err := application.EncodeAnalysisRequest(application.AnalyzeConversationRequestV1{
		Task: application.JobTypeAnalyze, SchemaVersion: application.AnalysisSchemaV2, PromptVersion: version,
		ConversationID: "conversation", BaseConversationRevision: 2, AnalysisThroughMessageID: messages[len(messages)-1].ID, Messages: messages,
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func commitmentFactOn(id string) domain.AnalysisResultV2 {
	return domain.AnalysisResultV2{AnalysisThroughMessageID: id, Facts: []domain.SemanticFact{
		{Type: domain.FactBusinessCommitment, Value: true, Confidence: .95, EvidenceMessageIDs: []string{id}}}}
}

func TestScannedCommitmentEvidenceNeedsAnOutgoingMessageThatIsNotRuledOut(t *testing.T) {
	for _, tc := range []struct {
		name string
		m    application.ContextMessage
		want bool
	}{
		{"promise without a lexicon word", application.ContextMessage{Direction: "OUTGOING", Body: "Будет сделано."}, true},
		{"statement without a promise word", application.ContextMessage{Direction: "OUTGOING", Body: "Подготовили документы, можете забирать."}, true},
		{"admitted promise", application.ContextMessage{Direction: "OUTGOING", Body: "Пришлю завтра."}, true},
		{"offer", application.ContextMessage{Direction: "OUTGOING", Body: "Хотите, пришлю прайс?"}, false},
		{"question", application.ContextMessage{Direction: "OUTGOING", Body: "Прислать вам прайс?"}, false},
		{"hedge", application.ContextMessage{Direction: "OUTGOING", Body: "Возможно, ответим завтра."}, false},
		{"empty", application.ContextMessage{Direction: "OUTGOING", Body: "  "}, false},
		{"the customer", application.ContextMessage{Direction: "INCOMING", Body: "Будет сделано."}, false},
	} {
		if got := application.ScannedCommitmentEvidence(tc.m); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestOnlyPromptV9AcceptsACommitmentFactOnAMessageWithoutALexiconWord(t *testing.T) {
	open := application.ContextMessage{ID: "m2", Direction: "OUTGOING", Body: "Будет сделано."}
	question := application.ContextMessage{ID: "m1", Direction: "INCOMING", Body: "Когда исправите?"}
	result := commitmentFactOn("m2")
	if err := application.ValidateAgreementEvidence(result, promptWithVersion(t, application.AnalysisPromptV9, question, open)); err != nil {
		t.Fatalf("v9 accepts the scanned evidence: %v", err)
	}
	if err := application.ValidateAgreementEvidence(result, promptWithVersion(t, application.AnalysisPromptV8, question, open)); err == nil {
		t.Fatal("v8 keeps the lexicon check")
	}
	// Структурно исключённое сообщение и сообщение клиента не становятся основанием ни в одной версии.
	for _, body := range []string{"Хотите, пришлю прайс?", "Прислать вам прайс?", "Возможно, ответим завтра."} {
		ruledOut := application.ContextMessage{ID: "m2", Direction: "OUTGOING", Body: body}
		if err := application.ValidateAgreementEvidence(result, promptWithVersion(t, application.AnalysisPromptV9, question, ruledOut)); err == nil {
			t.Errorf("%q is ruled out and cannot be the evidence", body)
		}
	}
	incoming := application.ContextMessage{ID: "m2", Direction: "INCOMING", Body: "Будет сделано."}
	if err := application.ValidateAgreementEvidence(result, promptWithVersion(t, application.AnalysisPromptV9, question, incoming)); err == nil {
		t.Fatal("the promise must come from the company")
	}
	// Отрицательный факт проверки основания не требует, как и прежде.
	negative := commitmentFactOn("m2")
	negative.Facts[0].Value = false
	if err := application.ValidateAgreementEvidence(negative, promptWithVersion(t, application.AnalysisPromptV9, question, incoming)); err != nil {
		t.Fatalf("a negative fact has no evidence rule: %v", err)
	}
}

func pendingCommitmentOn(id string) domain.AgreementObservation {
	return domain.AgreementObservation{Kind: domain.AgreementCommitment, WaitingFor: domain.AgreementBusiness, Status: domain.AgreementPending,
		TriggerMessageID: id, EvidenceMessageIDs: []string{id}, Confidence: .95}
}

func TestOnlyPromptV9AcceptsACommitmentAgreementOnAMessageWithoutALexiconWord(t *testing.T) {
	question := application.ContextMessage{ID: "m1", Direction: "INCOMING", Body: "Когда исправите?"}
	open := application.ContextMessage{ID: "m2", Direction: "OUTGOING", Body: "Будет сделано."}
	result := domain.AnalysisResultV2{AnalysisThroughMessageID: "m2", Agreements: []domain.AgreementObservation{pendingCommitmentOn("m2")}}
	if err := application.ValidateAgreementEvidence(result, promptWithVersion(t, application.AnalysisPromptV9, question, open)); err != nil {
		t.Fatalf("v9 accepts the scanned trigger: %v", err)
	}
	if err := application.ValidateAgreementEvidence(result, promptWithVersion(t, application.AnalysisPromptV8, question, open)); err == nil {
		t.Fatal("v8 keeps the lexicon check")
	}
	for _, body := range []string{"Хотите, пришлю прайс?", "Прислать вам прайс?", "Возможно, ответим завтра."} {
		ruledOut := application.ContextMessage{ID: "m2", Direction: "OUTGOING", Body: body}
		if err := application.ValidateAgreementEvidence(result, promptWithVersion(t, application.AnalysisPromptV9, question, ruledOut)); err == nil {
			t.Errorf("%q is ruled out and cannot trigger an agreement", body)
		}
	}
	// Остальные виды договорённостей этим не затронуты: запись без слов записи по-прежнему недопустима.
	booking := domain.AgreementObservation{Kind: domain.AgreementBookingConfirmation, WaitingFor: domain.AgreementCustomer, Status: domain.AgreementPending,
		TriggerMessageID: "m2", EvidenceMessageIDs: []string{"m2"}, Confidence: .95}
	bookingResult := domain.AnalysisResultV2{AnalysisThroughMessageID: "m2", Agreements: []domain.AgreementObservation{booking}}
	if err := application.ValidateAgreementEvidence(bookingResult, promptWithVersion(t, application.AnalysisPromptV9, question, open)); err == nil {
		t.Fatal("only COMMITMENT agreements use the scanned trigger")
	}
	// Договорённость обещает компания: сообщение клиента основанием не служит.
	incoming := application.ContextMessage{ID: "m2", Direction: "INCOMING", Body: "Будет сделано."}
	if err := application.ValidateAgreementEvidence(result, promptWithVersion(t, application.AnalysisPromptV9, question, incoming)); err == nil {
		t.Fatal("the promise must come from the company")
	}
}
