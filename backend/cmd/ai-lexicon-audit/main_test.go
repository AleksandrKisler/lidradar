package main

import (
	"os"
	"testing"

	"lidradar/backend/internal/ai/application"
	"lidradar/backend/internal/ai/benchmark"
	"lidradar/backend/internal/ai/domain"
)

func testCase(id string, promise bool, messages ...application.ContextMessage) benchmark.Case {
	input := application.AnalyzeConversationRequestV1{Messages: messages}
	expected := []domain.SemanticFact{}
	if promise {
		expected = append(expected, domain.SemanticFact{Type: domain.FactBusinessCommitment, Value: true, Confidence: 1, EvidenceMessageIDs: []string{messages[len(messages)-1].ID}})
	}
	return benchmark.Case{ID: id, Input: input, Expected: expected}
}

func message(id, direction, body string) application.ContextMessage {
	return application.ContextMessage{ID: id, Direction: direction, Body: body}
}

func TestAuditCountsCoverageAndFalseAcceptsPerCategory(t *testing.T) {
	cases := []benchmark.Case{
		testCase("kept-01", true, message("a1", "INCOMING", "Когда ответите?"), message("a2", "OUTGOING", "Проверю и отвечу вам.")),
		testCase("kept-02", true, message("b1", "OUTGOING", "Ретушёр загрузит исходники в облако.")),
		testCase("trap-01", false, message("c1", "OUTGOING", "Проверю и отвечу вам, если нужно.")),
		testCase("trap-02", false, message("d1", "OUTGOING", "Мы работаем до восьми.")),
		testCase("trap-03", false, message("e1", "INCOMING", "Я пришлю документы вечером.")),
	}
	result := audit(cases, true)
	if result.Positives != 2 || result.Covered != 1 || result.Negatives != 2 || result.FalselyAccepted != 1 {
		t.Fatalf("unexpected totals: %+v", result)
	}
	if result.Coverage != 0.5 || result.FalseAcceptRate != 0.5 {
		t.Fatalf("unexpected rates: coverage %v, false accept %v", result.Coverage, result.FalseAcceptRate)
	}
	if got := result.ByCategory["kept"]; got.Positives != 2 || got.Covered != 1 {
		t.Fatalf("unexpected group kept: %+v", got)
	}
	if got := result.ByCategory["trap"]; got.Negatives != 2 || got.FalselyAccepted != 1 {
		t.Fatalf("unexpected group trap: %+v", got)
	}
	if len(result.Missed) != 1 || result.Missed[0].CaseID != "kept-02" || result.Missed[0].Text == "" {
		t.Fatalf("the missed promise must be listed with its text: %+v", result.Missed)
	}
	if len(result.AcceptedWrongly) != 1 || result.AcceptedWrongly[0].CaseID != "trap-01" {
		t.Fatalf("the wrongly accepted message must be listed: %+v", result.AcceptedWrongly)
	}
}

func TestAuditHidesTextsUnlessAskedAndIgnoresIncomingMessages(t *testing.T) {
	cases := []benchmark.Case{testCase("kept-01", true, message("a1", "OUTGOING", "Сегодня ничего не обещаю."))}
	result := audit(cases, false)
	for _, found := range append(append([]finding{}, result.Missed...), result.AcceptedWrongly...) {
		if found.Text != "" {
			t.Fatalf("texts must stay hidden by default: %+v", found)
		}
	}
	incoming := audit([]benchmark.Case{testCase("trap-01", false, message("x1", "INCOMING", "Я отвечу позже."))}, false)
	if incoming.Negatives != 0 || incoming.FalselyAccepted != 0 {
		t.Fatalf("incoming messages are not company messages: %+v", incoming)
	}
}

func TestCategoryStripsTheCaseNumber(t *testing.T) {
	for id, want := range map[string]string{"cm-dev-offer-03": "cm-dev-offer", "plain": "plain", "x-12-07": "x-12"} {
		if got := category(id); got != want {
			t.Fatalf("category(%q) = %q, want %q", id, got, want)
		}
	}
}

// Набор настройки словаря (DEV, написан автором настройки): правки не должны ни терять покрытие обещаний, ни пропускать больше
// необещаний, чем сейчас. Тест страхует слова от случайных потерь; обобщение он не доказывает — его проверяет слепая выборка (ADR 0054).
func TestCommitmentDevSetKeepsItsCoverageAndItsFalseAcceptRate(t *testing.T) {
	file, err := os.Open("../../../models/datasets/commitment_dev_v1.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	cases, _, err := benchmark.Load(file)
	if err != nil {
		t.Fatal(err)
	}
	result := audit(cases, true)
	if result.Coverage < 0.95 {
		t.Fatalf("the lexicon covers %.3f of the promises of the tuning set, want at least 0.95; missed: %+v", result.Coverage, result.Missed)
	}
	if result.FalseAcceptRate > 0.05 {
		t.Fatalf("the lexicon accepts %.3f of the non-promises of the tuning set, want at most 0.05; accepted: %+v", result.FalseAcceptRate, result.AcceptedWrongly)
	}
	// Прежний словарь ничего не терял в группе «legacy»: все обещания из неё обязаны проходить.
	if group := result.ByCategory["cm-dev-legacy"]; group.Positives == 0 || group.Covered != group.Positives {
		t.Fatalf("the promises of the previous lexicon must stay allowed: %+v", group)
	}
	// Описание услуги и просьбы клиенту — самые похожие на обещание слова; просьбы и прошедшее время не должны допускаться никогда.
	for _, name := range []string{"cm-dev-request-to-client", "cm-dev-past", "cm-dev-negation", "cm-dev-question"} {
		if group := result.ByCategory[name]; group.Negatives == 0 || group.FalselyAccepted != 0 {
			t.Fatalf("%s must never be allowed: %+v", name, group)
		}
	}
}

func TestSupportByMessageNamesTheLexiconStateOfEveryOutgoingMessageAndSkipsTheCustomer(t *testing.T) {
	cases := []benchmark.Case{
		testCase("s-01", true, message("a1", "INCOMING", "Когда ответите?"), message("a2", "OUTGOING", "Проверю и отвечу вам."), message("a3", "OUTGOING", "Хотите, пришлю прайс?"), message("a4", "OUTGOING", "Будет сделано.")),
	}
	got := supportByMessage(cases)
	want := map[string]string{"a2": "ADMITTED", "a3": "RULED_OUT", "a4": "OPEN"}
	if len(got) != len(want) {
		t.Fatalf("only outgoing messages are listed: %v", got)
	}
	for id, state := range want {
		if got[id] != state {
			t.Errorf("%s: got %q, want %q", id, got[id], state)
		}
	}
}
