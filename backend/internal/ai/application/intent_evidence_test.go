package application

import (
	"testing"

	"lidradar/backend/internal/ai/domain"
)

func TestPurchaseEligibilityPreservesRequestsAndRejectsRefusals(t *testing.T) {
	for _, tc := range []struct {
		body     string
		eligible bool
	}{
		{"Хотела бы приобрести этот набор.", true},
		{"Хочу оформить заказ на комплект.", true},
		{"Хочу купить, но не могу оплатить картой.", true},
		{"Отмените старый заказ. Хочу заказать другой набор.", true},
		{"I would like to buy this package.", true},
		{"Не хочу купить этот набор.", false},
		{"Пока не готов оформить заказ.", false},
		{"Если решусь, хочу заказать набор.", false},
		{"Сколько стоит набор?", false},
		{"Интересует набор, расскажите подробнее.", false},
	} {
		t.Run(tc.body, func(t *testing.T) {
			if got := HasExplicitPurchaseIntent(tc.body); got != tc.eligible {
				t.Fatalf("eligible=%v, want %v", got, tc.eligible)
			}
		})
	}
}

func TestBookingEligibilitySeparatesAvailabilityFromInformation(t *testing.T) {
	for _, tc := range []struct {
		body     string
		eligible bool
	}{
		{"Свободен ли специалист в воскресенье в 12:00?", true},
		{"Хотел бы попасть в четверг утром, есть место?", true},
		{"Нужен визит на следующей неделе.", true},
		{"Сколько стоит? И запишите меня на завтра.", true},
		{"Не ставьте меня в расписание.", false},
		{"Визит отменяется, приезжать не буду.", false},
		{"Нужны подробности про пробный урок.", false},
		{"Какой режим работы?", false},
		{"Запись была ошибочной, отмените её.", false},
		{"Есть ли у вас вообще услуга замера?", false},
		{"Сколько стоит услуга?", false},
		{"Интересует пробный урок.", false},
		{"Интересует пробный урок, есть место в субботу?", true},
		{"Могу оплатить 900 рублей.", false},
		{"Указанная сумма меня устраивает: 2750 рублей.", false},
		{"Во сколько обойдётся работа?", false},
		{"Есть скидки?", false},
		{"Больше не рассматриваю эту услугу.", false},
		{"Предложение подходит, окончательно отвечу завтра.", false},
		{"Над второй услугой подумаю, а сейчас запишите меня на завтра.", true},
		{"Интересует консультация, можно ли приехать завтра?", true},
		{"Возможно ли записаться на завтра?", true},
		{"Возможно когда-нибудь запишусь.", false},
	} {
		t.Run(tc.body, func(t *testing.T) {
			m := ContextMessage{Direction: "INCOMING", Body: tc.body}
			if got := PossibleBookingEvidence(m); got != tc.eligible {
				t.Fatalf("eligible=%v want=%v", got, tc.eligible)
			}
			m.Direction = "OUTGOING"
			if PossibleBookingEvidence(m) {
				t.Fatal("business message cannot prove customer intent")
			}
		})
	}
}

func TestUnrelatedObjectCannotCompleteOrCancelAgreement(t *testing.T) {
	if resolvesAgreement("COMMITMENT", "Отправлю счёт", "Отправил договор") {
		t.Fatal("contract fulfilled invoice promise")
	}
	if !resolvesAgreement("COMMITMENT", "Отправлю счёт", "Счёт отправил") {
		t.Fatal("matching fulfillment rejected")
	}
	if cancelsAgreement("Отправлю счёт", "Отмените договор") {
		t.Fatal("unrelated cancellation fulfilled invoice")
	}
	if !cancelsAgreement("Отправлю счёт", "Отмените счёт") {
		t.Fatal("matching cancellation rejected")
	}
}

func TestGenericReplyDoesNotCloseEitherIndependentPromise(t *testing.T) {
	for _, reply := range []string{"Готово.", "Отправил.", "Отмените."} {
		messages := []ContextMessage{
			{ID: "invoice", Direction: "OUTGOING", Body: "Отправлю счёт."},
			{ID: "contract", Direction: "OUTGOING", Body: "Отправлю договор."},
			{ID: "reply", Direction: "OUTGOING", Body: reply},
		}
		prompt, err := EncodeAnalysisRequest(AnalyzeConversationRequestV1{SchemaVersion: AnalysisSchemaV2, AnalysisThroughMessageID: "reply", Messages: messages})
		if err != nil {
			t.Fatal(err)
		}
		status := domain.AgreementResolved
		if reply == "Отмените." {
			status = domain.AgreementCancelled
		}
		for _, trigger := range []string{"invoice", "contract"} {
			result := domain.AnalysisResultV2{AnalysisThroughMessageID: "reply", Agreements: []domain.AgreementObservation{{
				Kind: domain.AgreementCommitment, WaitingFor: domain.AgreementBusiness, Status: status,
				TriggerMessageID: trigger, EvidenceMessageIDs: []string{trigger, "reply"}, Confidence: .96,
			}}}
			if err := ValidateAgreementEvidence(result, prompt); err == nil {
				t.Fatalf("%q closed %s", reply, trigger)
			}
		}
	}
}
