package application_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"lidradar/backend/internal/ai/application"
	"lidradar/backend/internal/ai/domain"
)

func TestAgreementGenerationTransitionsRequireEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, body, direction string
		kind                  domain.AgreementKind
		actor                 domain.AgreementWaitingFor
		want                  []domain.AgreementStatus
	}{
		{"booking accepted", "Да, запишите меня", "INCOMING", domain.AgreementBookingConfirmation, domain.AgreementCustomer, []domain.AgreementStatus{domain.AgreementResolved}},
		{"home visit accepted", "Будем ждать вас дома", "INCOMING", domain.AgreementBookingConfirmation, domain.AgreementCustomer, []domain.AgreementStatus{domain.AgreementResolved}},
		{"waiting for answer is not acceptance", "Буду ждать вашего ответа", "INCOMING", domain.AgreementBookingConfirmation, domain.AgreementCustomer, []domain.AgreementStatus{domain.AgreementPending}},
		{"visit not accepted", "Не будем ждать вас", "INCOMING", domain.AgreementBookingConfirmation, domain.AgreementCustomer, []domain.AgreementStatus{domain.AgreementPending}},
		{"booking cancelled", "Отмените, мне не нужно", "INCOMING", domain.AgreementBookingConfirmation, domain.AgreementCustomer, []domain.AgreementStatus{domain.AgreementCancelled}},
		{"greeting is not delivery", "Здравствуйте!", "OUTGOING", domain.AgreementCommitment, domain.AgreementBusiness, []domain.AgreementStatus{domain.AgreementPending}},
		{"explicit delivery", "Проверил: товар есть", "OUTGOING", domain.AgreementCommitment, domain.AgreementBusiness, []domain.AgreementStatus{domain.AgreementResolved}},
		{"wrong party", "Я проверил", "INCOMING", domain.AgreementCommitment, domain.AgreementBusiness, []domain.AgreementStatus{domain.AgreementPending}},
		{"negative delivery", "Ещё не отправил", "OUTGOING", domain.AgreementCommitment, domain.AgreementBusiness, []domain.AgreementStatus{domain.AgreementPending}},
		{"keep booking", "Не отменяйте запись", "INCOMING", domain.AgreementBookingConfirmation, domain.AgreementCustomer, []domain.AgreementStatus{domain.AgreementPending}},
		{"payment does not confirm slot", "Я оплатил", "INCOMING", domain.AgreementBookingConfirmation, domain.AgreementCustomer, []domain.AgreementStatus{domain.AgreementPending}},
		{"price acceptance does not fix payment", "Цена подходит", "INCOMING", domain.AgreementPurchaseBlocker, domain.AgreementCustomer, []domain.AgreementStatus{domain.AgreementPending}},
		{"payment problem fixed", "Исправили ошибку, теперь получилось", "OUTGOING", domain.AgreementPurchaseBlocker, domain.AgreementBusiness, []domain.AgreementStatus{domain.AgreementResolved}},
		{"reschedule completed", "Перенесли на завтра", "OUTGOING", domain.AgreementReschedule, domain.AgreementBusiness, []domain.AgreementStatus{domain.AgreementResolved}},
		{"unrelated delivery", "Отправил счёт", "OUTGOING", domain.AgreementCommitment, domain.AgreementBusiness, []domain.AgreementStatus{domain.AgreementPending}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trigger := "Проверю наличие"
			if tc.kind == domain.AgreementReschedule {
				trigger = "Перенесите запись на завтра"
			}
			got := application.AgreementTransitionCandidates(tc.kind, tc.actor, trigger, []application.ContextMessage{{ID: "reply", Direction: tc.direction, Body: tc.body}})
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("statuses = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFollowUpEvidenceRequiresDeferral(t *testing.T) {
	for _, tc := range []struct {
		body string
		ok   bool
	}{
		{"Что с моей заявкой?", false}, {"Не ставьте меня в расписание", false},
		{"Мне нужно время до завтра, потом дам ответ", true},
		{"Подумаю и вернусь с решением", true}, {"Напомните в понедельник", true},
		{"Больше не звоните мне позже", false},
		{"Возьму паузу до воскресенья", true}, {"Снова напишу после разговора", true}, {"Свяжусь, когда смогу решить", true},
	} {
		prompt := agreementPrompt(t, application.ContextMessage{ID: "m", Direction: "INCOMING", Body: tc.body})
		result := domain.AnalysisResultV2{AnalysisThroughMessageID: "m", Facts: []domain.SemanticFact{{Type: domain.FactFollowUpCandidate, Value: true, Confidence: .99, EvidenceMessageIDs: []string{"m"}}}}
		if err := application.ValidateAgreementEvidence(result, prompt); (err == nil) != tc.ok {
			t.Fatalf("%q: %v", tc.body, err)
		}
	}
}

func agreementPrompt(t *testing.T, messages ...application.ContextMessage) string {
	t.Helper()
	p, err := application.EncodeAnalysisRequest(application.AnalyzeConversationRequestV1{
		Task: application.JobTypeAnalyze, SchemaVersion: application.AnalysisSchemaV2, PromptVersion: application.AnalysisPromptV7,
		ConversationID: "conversation", BaseConversationRevision: 2, AnalysisThroughMessageID: messages[len(messages)-1].ID,
		Messages: messages,
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func agreementResult(t *testing.T, through string, facts []domain.SemanticFact, agreements ...domain.AgreementObservation) string {
	t.Helper()
	if facts == nil {
		facts = []domain.SemanticFact{}
	}
	if agreements == nil {
		agreements = []domain.AgreementObservation{}
	}
	raw, err := json.Marshal(domain.AnalysisResultV2{SchemaVersion: application.AnalysisSchemaV2,
		AnalysisThroughMessageID: through, Summary: "Состояние беседы.", Facts: facts, Agreements: agreements})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestAgreementEvidenceLiveConversationAndTransitions(t *testing.T) {
	booking := domain.AgreementObservation{Kind: domain.AgreementBookingConfirmation, WaitingFor: domain.AgreementCustomer,
		Status: domain.AgreementPending, TriggerMessageID: "slot", EvidenceMessageIDs: []string{"slot"}, Confidence: .96}
	base := []application.ContextMessage{{ID: "intent", Direction: "INCOMING", Body: "Хочу записаться на услугу."},
		{ID: "slot", Direction: "OUTGOING", Body: "Свободно завтра в 15:00, вам подходит?"}}
	for _, tc := range []struct {
		name     string
		messages []application.ContextMessage
		a        domain.AgreementObservation
		ok       bool
	}{
		{"live pending customer", base, booking, true},
		{"slot offer supersedes business wait", base,
			domain.AgreementObservation{Kind: booking.Kind, WaitingFor: domain.AgreementBusiness, Status: domain.AgreementPending, TriggerMessageID: "intent", EvidenceMessageIDs: []string{"intent"}, Confidence: .96}, false},
		{"confirmation resolves", append(append([]application.ContextMessage{}, base...), application.ContextMessage{ID: "confirm", Direction: "INCOMING", Body: "Да, мне подходит, запишите."}),
			domain.AgreementObservation{Kind: booking.Kind, WaitingFor: booking.WaitingFor, Status: domain.AgreementResolved, TriggerMessageID: "slot", EvidenceMessageIDs: []string{"slot", "confirm"}, Confidence: .98}, true},
		{"confirmation is not a new business wait", append(append([]application.ContextMessage{}, base...), application.ContextMessage{ID: "confirm", Direction: "INCOMING", Body: "Да, мне подходит, запишите."}),
			domain.AgreementObservation{Kind: booking.Kind, WaitingFor: domain.AgreementBusiness, Status: domain.AgreementPending, TriggerMessageID: "confirm", EvidenceMessageIDs: []string{"confirm"}, Confidence: .98}, false},
		{"greeting is not resolution", append(append([]application.ContextMessage{}, base...), application.ContextMessage{ID: "hello", Direction: "INCOMING", Body: "Здравствуйте 👋"}),
			domain.AgreementObservation{Kind: booking.Kind, WaitingFor: booking.WaitingFor, Status: domain.AgreementResolved, TriggerMessageID: "slot", EvidenceMessageIDs: []string{"slot", "hello"}, Confidence: .98}, false},
		{"missing evidence", base, domain.AgreementObservation{Kind: booking.Kind, WaitingFor: booking.WaitingFor, Status: booking.Status, TriggerMessageID: "slot", EvidenceMessageIDs: []string{"slot", "foreign"}, Confidence: .98}, false},
		{"wrong actor", base, domain.AgreementObservation{Kind: booking.Kind, WaitingFor: domain.AgreementBusiness, Status: booking.Status, TriggerMessageID: "slot", EvidenceMessageIDs: []string{"slot"}, Confidence: .98}, false},
		{"superseded slot", append(append([]application.ContextMessage{}, base...), application.ContextMessage{ID: "new-slot", Direction: "OUTGOING", Body: "Свободно также в 16:00"}), booking, false},
		{"postponement resets trigger", append(append([]application.ContextMessage{}, base...), application.ContextMessage{ID: "later", Direction: "INCOMING", Body: "Давайте позже, я подумаю до завтра."}),
			domain.AgreementObservation{Kind: booking.Kind, WaitingFor: domain.AgreementCustomer, Status: domain.AgreementPending, TriggerMessageID: "later", EvidenceMessageIDs: []string{"later"}, Confidence: .96}, true},
		{"old trigger after postponement", append(append([]application.ContextMessage{}, base...), application.ContextMessage{ID: "later", Direction: "INCOMING", Body: "Давайте позже, я подумаю до завтра."}), booking, false},
		{"cancelled", append(append([]application.ContextMessage{}, base...), application.ContextMessage{ID: "cancel", Direction: "INCOMING", Body: "Отмените, запись не нужна."}),
			domain.AgreementObservation{Kind: booking.Kind, WaitingFor: booking.WaitingFor, Status: domain.AgreementCancelled, TriggerMessageID: "slot", EvidenceMessageIDs: []string{"slot", "cancel"}, Confidence: .98}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			through := tc.messages[len(tc.messages)-1].ID
			result, err := application.ValidateAnalysisResultV2(agreementResult(t, through, nil, tc.a), through)
			if err == nil {
				err = application.ValidateAgreementEvidence(result, agreementPrompt(t, tc.messages...))
			}
			if (err == nil) != tc.ok {
				t.Fatalf("validation error = %v", err)
			}
		})
	}
}

func TestPurchaseIntentNeedsExplicitIncomingOrder(t *testing.T) {
	for _, tc := range []struct {
		body, direction string
		ok              bool
	}{
		{"Хочу купить, оформите заказ.", "INCOMING", true},
		{"Можно заказать?", "INCOMING", false},
		{"Интересует цена услуги", "INCOMING", false},
		{"Хочу купить, оформите заказ.", "OUTGOING", false},
	} {
		prompt := agreementPrompt(t, application.ContextMessage{ID: "m", Direction: tc.direction, Body: tc.body})
		fact := domain.SemanticFact{Type: domain.FactPurchaseIntent, Value: true, Confidence: .95, EvidenceMessageIDs: []string{"m"}}
		result, err := application.ValidateAnalysisResultV2(agreementResult(t, "m", []domain.SemanticFact{fact}), "m")
		if err == nil {
			err = application.ValidateAgreementEvidence(result, prompt)
		}
		if (err == nil) != tc.ok {
			t.Fatalf("%q: validation error = %v", tc.body, err)
		}
	}
}

func TestLiveSlotOfferIsNotBusinessCommitment(t *testing.T) {
	messages := []application.ContextMessage{{ID: "intent", Direction: "INCOMING", Body: "Хочу записаться на услугу."}, {ID: "slot", Direction: "OUTGOING", Body: "Свободно завтра в 15:00, вам подходит?"}}
	prompt := agreementPrompt(t, messages...)
	booking := domain.SemanticFact{Type: domain.FactBookingIntent, Value: true, Confidence: .96, EvidenceMessageIDs: []string{"intent"}}
	for _, tc := range []struct {
		name  string
		facts []domain.SemanticFact
		ok    bool
	}{
		{"booking only", []domain.SemanticFact{booking}, true},
		{"invented commitment", []domain.SemanticFact{booking, {Type: domain.FactBusinessCommitment, Value: true, Confidence: .96, EvidenceMessageIDs: []string{"slot"}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := application.ValidateAnalysisResultV2(agreementResult(t, "slot", tc.facts), "slot")
			if err == nil {
				err = application.ValidateAgreementEvidence(result, prompt)
			}
			if (err == nil) != tc.ok {
				t.Fatalf("validation error = %v", err)
			}
		})
	}
}

func TestAgreementKindsAndExplicitFulfillment(t *testing.T) {
	for _, tc := range []struct {
		name     string
		messages []application.ContextMessage
		a        domain.AgreementObservation
		ok       bool
	}{
		{"reschedule", []application.ContextMessage{{ID: "new", Direction: "INCOMING", Body: "Можно перенести запись на завтра?"}}, domain.AgreementObservation{Kind: domain.AgreementReschedule, WaitingFor: domain.AgreementBusiness, Status: domain.AgreementPending, TriggerMessageID: "new", EvidenceMessageIDs: []string{"new"}, Confidence: .95}, true},
		{"deferred trigger", []application.ContextMessage{{ID: "offer", Direction: "OUTGOING", Body: "Свободно завтра в 15:00"}, {ID: "later", Direction: "INCOMING", Body: "Подумаю, решим позже"}}, domain.AgreementObservation{Kind: domain.AgreementReschedule, WaitingFor: domain.AgreementCustomer, Status: domain.AgreementPending, TriggerMessageID: "later", EvidenceMessageIDs: []string{"later"}, Confidence: .95}, true},
		{"blocker", []application.ContextMessage{{ID: "block", Direction: "INCOMING", Body: "Не могу оплатить: проблема с картой."}}, domain.AgreementObservation{Kind: domain.AgreementPurchaseBlocker, WaitingFor: domain.AgreementBusiness, Status: domain.AgreementPending, TriggerMessageID: "block", EvidenceMessageIDs: []string{"block"}, Confidence: .95}, true},
		{"promise greeting", []application.ContextMessage{{ID: "promise", Direction: "OUTGOING", Body: "Проверю и отвечу завтра."}, {ID: "hello", Direction: "OUTGOING", Body: "Здравствуйте!"}}, domain.AgreementObservation{Kind: domain.AgreementCommitment, WaitingFor: domain.AgreementBusiness, Status: domain.AgreementResolved, TriggerMessageID: "promise", EvidenceMessageIDs: []string{"promise", "hello"}, Confidence: .95}, false},
		{"promise fulfilled", []application.ContextMessage{{ID: "promise", Direction: "OUTGOING", Body: "Проверю и отвечу завтра."}, {ID: "done", Direction: "OUTGOING", Body: "Проверил: свободно в 15:00."}}, domain.AgreementObservation{Kind: domain.AgreementCommitment, WaitingFor: domain.AgreementBusiness, Status: domain.AgreementResolved, TriggerMessageID: "promise", EvidenceMessageIDs: []string{"promise", "done"}, Confidence: .95}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			through := tc.messages[len(tc.messages)-1].ID
			result, err := application.ValidateAnalysisResultV2(agreementResult(t, through, nil, tc.a), through)
			if err == nil {
				err = application.ValidateAgreementEvidence(result, agreementPrompt(t, tc.messages...))
			}
			if (err == nil) != tc.ok {
				t.Fatalf("validation error = %v", err)
			}
		})
	}
}

func TestV2RejectsModelAuthorityAndInvalidObservations(t *testing.T) {
	base := agreementResult(t, "m", nil, domain.AgreementObservation{Kind: domain.AgreementCommitment, WaitingFor: domain.AgreementBusiness, Status: domain.AgreementPending, TriggerMessageID: "m", EvidenceMessageIDs: []string{"m"}, Confidence: .9})
	for _, raw := range []string{
		strings.Replace(base, `"confidence":0.9}`, `"confidence":0.9,"trusted":true}`, 1),
		strings.Replace(base, `"waitingFor":"BUSINESS"`, `"waitingFor":"ALIEN"`, 1),
		strings.Replace(base, `"agreements":[`, `"agreements":null,"extra":[`, 1),
	} {
		_, err := application.ValidateAnalysisResultV2(raw, "m")
		if !errors.Is(err, application.ErrInvalidAIOutput) {
			t.Fatalf("accepted invalid observation: %s", raw)
		}
	}
	if _, err := application.ValidateAnalysisResultV1(base, "m"); !errors.Is(err, application.ErrInvalidAIOutput) {
		t.Fatal("v1 reader accepted v2 authority")
	}
	if _, err := application.ValidateAnalysisResultV1(`{"schemaVersion":"analyze-conversation.v1","analysisThroughMessageId":"m","summary":"Order","facts":[{"type":"PURCHASE_INTENT","value":true,"confidence":0.9,"evidenceMessageIds":["m"]}]}`, "m"); !errors.Is(err, application.ErrInvalidAIOutput) {
		t.Fatal("v1 reader accepted v2 purchase intent")
	}
}

func TestV2WeakAndStaleDoNotApplyAgreements(t *testing.T) {
	for _, tc := range []struct {
		name    string
		stale   bool
		invalid bool
		want    domain.ApplicationStatus
	}{
		{"weak", false, false, domain.ApplicationApplied}, {"stale", true, false, domain.ApplicationStale}, {"invalid", false, true, domain.ApplicationRejected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, store, _ := setup(t)
			ctx := context.Background()
			prompt := agreementPrompt(t, application.ContextMessage{ID: "m", Direction: "OUTGOING", Body: "Проверю и отвечу завтра."})
			job, err := svc.Enqueue(ctx, application.EnqueueCommand{TenantID: "tenant", ConversationID: "conversation", Prompt: prompt, BaseConversationRevision: 2, AnalysisThroughMessageID: "m", SchemaVersion: application.AnalysisSchemaV2, PromptVersion: application.AnalysisPromptV7})
			if err != nil {
				t.Fatal(err)
			}
			if err := svc.Heartbeat(ctx, "1", nodeSecret, application.HeartbeatCommand{Status: domain.NodeReady, ModelVersion: application.DefaultModelVersion, AvailableSlots: 1}); err != nil {
				t.Fatal(err)
			}
			if _, ok, err := svc.Claim(ctx, "1", nodeSecret); err != nil || !ok {
				t.Fatalf("claim %v %v", ok, err)
			}
			run, err := svc.Started(ctx, "1", nodeSecret, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.stale {
				store.SetConversationSnapshot("tenant", "conversation", domain.ConversationSnapshot{Revision: 3, LastMessageID: "later"})
			} else {
				store.SetConversationSnapshot("tenant", "conversation", domain.ConversationSnapshot{Revision: 2, LastMessageID: "m"})
			}
			out := agreementResult(t, "m", nil, domain.AgreementObservation{Kind: domain.AgreementCommitment, WaitingFor: domain.AgreementBusiness, Status: domain.AgreementPending, TriggerMessageID: "m", EvidenceMessageIDs: []string{"m"}, Confidence: .7})
			if tc.invalid {
				out = strings.Replace(out, `"confidence":0.7}`, `"confidence":0.7,"trusted":true}`, 1)
			}
			done, err := svc.Complete(ctx, "1", nodeSecret, job.ID, run.ID, out)
			if err != nil {
				t.Fatal(err)
			}
			if done.ApplicationStatus != tc.want {
				t.Fatalf("status %s", done.ApplicationStatus)
			}
			summary, ok := store.Summary("tenant", "conversation")
			if tc.stale || tc.invalid {
				if ok {
					t.Fatal("untrusted summary applied")
				}
			} else if !ok || len(summary.Agreements) != 1 || summary.Agreements[0].Trusted {
				t.Fatalf("weak projection %#v", summary)
			}
		})
	}
}

func TestAgreementNegationCannotFulfillOrCancel(t *testing.T) {
	for _, tc := range []struct {
		body   string
		status domain.AgreementStatus
	}{
		{"Я ещё не подтвердил время", domain.AgreementResolved},
		{"Пожалуйста, не отменяйте запись", domain.AgreementCancelled},
		{"Оплатить не получилось", domain.AgreementResolved},
	} {
		messages := []application.ContextMessage{{ID: "offer", Direction: "OUTGOING", Body: "Предлагаю запись завтра, подходит?"}, {ID: "reply", Direction: "INCOMING", Body: tc.body}}
		observation := domain.AgreementObservation{Kind: domain.AgreementBookingConfirmation, WaitingFor: domain.AgreementCustomer, Status: tc.status, TriggerMessageID: "offer", EvidenceMessageIDs: []string{"offer", "reply"}, Confidence: .96}
		result, err := application.ValidateAnalysisResultV2(agreementResult(t, "reply", nil, observation), "reply")
		if err == nil {
			err = application.ValidateAgreementEvidence(result, agreementPrompt(t, messages...))
		}
		if err == nil {
			t.Fatalf("negated completion accepted: %s", tc.body)
		}
	}
}

func TestPromiseEvidenceSeparatesOfferAndFulfillment(t *testing.T) {
	for _, tc := range []struct {
		body string
		want bool
	}{
		{"Свободно во вторник. Вам подходит?", false},
		{"Уточнил: деталь есть в наличии.", false},
		{"Отправил счёт.", false},
		{"Уточню: есть ли деталь.", true},
		{"Свободно во вторник. Отправлю счёт сегодня.", true},
		{"Уточнил наличие. Завтра отправлю счёт.", true},
	} {
		if got := application.PossibleBusinessCommitmentEvidence(application.ContextMessage{Direction: "OUTGOING", Body: tc.body}); got != tc.want {
			t.Errorf("%q: got %v, want %v", tc.body, got, tc.want)
		}
	}
}

func TestNegativeReplyCannotReplaceDirectBookingEvidence(t *testing.T) {
	for _, body := range []string{"Мне не подходит.", "Никакой записи не требуется.", "Не подтверждаю запись.", "Отмените запись."} {
		if application.ExplicitBookingEvidence(application.ContextMessage{Direction: "INCOMING", Body: body}) {
			t.Errorf("negative reply accepted as direct booking proof: %q", body)
		}
	}
}

func TestBookingRefusalDoesNotHideASeparateRequest(t *testing.T) {
	for _, tc := range []struct {
		body     string
		rejected bool
	}{
		{"Никакой записи не требуется. Не пишите мне.", true},
		{"Отмените запись.", true},
		{"Не подтверждаю запись.", true},
		{"Отмените понедельник, запишите на вторник.", false},
		{"Хочу записаться, но не знаю удобное время.", false},
	} {
		if got := application.RejectsBookingIntent(tc.body); got != tc.rejected {
			t.Errorf("%q: rejected=%v want=%v", tc.body, got, tc.rejected)
		}
	}
}
