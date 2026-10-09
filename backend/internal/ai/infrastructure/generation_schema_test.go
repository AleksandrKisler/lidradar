package infrastructure

import (
	"encoding/json"
	"strings"
	"testing"

	"lidradar/backend/internal/ai/application"
)

func TestGenerationSchemaUsesOnlyMessageAmountsAndIDs(t *testing.T) {
	for _, tc := range []struct {
		body    string
		amounts []string
	}{
		{"Подскажите стоимость, свободно ли в 16:00?", nil},
		{"Стоимость 3\u202f500,50 ₽, ранее 4000 RUB", []string{"3500.5", "4000"}},
		{"Стоимость 0 рублей", []string{"0"}},
	} {
		request := application.AnalyzeConversationRequestV1{SchemaVersion: application.AnalysisSchemaV1, AnalysisThroughMessageID: "m1", CompanyContext: "Цена 9999 RUB", ConversationSummary: "Ранее 8888 рублей",
			Messages: []application.ContextMessage{{ID: "m1", Direction: "INCOMING", Body: tc.body}}}
		prompt, _ := json.Marshal(request)
		raw, err := analysisGenerationSchemaV6(string(prompt))
		if err != nil {
			t.Fatal(err)
		}
		var schema struct {
			Properties struct {
				Through struct {
					Const string `json:"const"`
				} `json:"analysisThroughMessageId"`
				Facts struct {
					Items struct {
						OneOf []struct {
							Properties map[string]json.RawMessage `json:"properties"`
						} `json:"oneOf"`
					} `json:"items"`
				} `json:"facts"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatal(err)
		}
		variants := schema.Properties.Facts.Items.OneOf
		wantCount := 2
		if len(tc.amounts) > 0 {
			wantCount = 3
		}
		if len(variants) != wantCount || schema.Properties.Through.Const != "m1" {
			t.Fatalf("schema=%s", raw)
		}
		if strings.Contains(string(raw), "9999") || strings.Contains(string(raw), "8888") {
			t.Fatal("reference context leaked into allowed sums")
		}
		if len(tc.amounts) > 0 {
			var amount struct {
				Enum []string `json:"enum"`
			}
			json.Unmarshal(variants[0].Properties["amount"], &amount)
			if strings.Join(amount.Enum, ",") != strings.Join(tc.amounts, ",") {
				t.Fatalf("amounts = %#v", amount.Enum)
			}
		}
		for _, variant := range variants {
			var value struct {
				Const bool   `json:"const"`
				Type  string `json:"type"`
			}
			json.Unmarshal(variant.Properties["value"], &value)
			// Negative facts remain legal so grammar cannot force an intended
			// denial into a positive fact. Positive prices still require an amount.
			if _, hasAmount := variant.Properties["amount"]; hasAmount && !value.Const {
				t.Fatal("price amount belongs only to positive facts")
			}
			if strings.Contains(string(variant.Properties["type"]), "BOOKING_INTENT") && value.Type != "boolean" {
				t.Fatal("grammar must not force a denied fact to true")
			}
			var evidence struct {
				Items struct {
					Enum []string `json:"enum"`
				} `json:"items"`
			}
			json.Unmarshal(variant.Properties["evidenceMessageIds"], &evidence)
			if len(evidence.Items.Enum) != 1 || evidence.Items.Enum[0] != "m1" {
				t.Fatal("evidence IDs are not constrained")
			}
		}
		if strings.Index(string(raw), `"summary":{"type"`) > strings.Index(string(raw), `"facts":{"type"`) {
			t.Fatal("summary must precede facts during generation")
		}
	}
}

func TestGenerationSchemaRejectsMissingContext(t *testing.T) {
	for _, prompt := range []string{`{`, `{}`, `{"analysisThroughMessageId":"m2","messages":[{"id":"m1","body":"Цена 5 RUB"}]}`} {
		if _, err := analysisGenerationSchemaV6(prompt); err == nil {
			t.Fatal("invalid context accepted")
		}
	}
}

func TestGenerationSchemaV7ConstrainsAgreementAndPurchaseEvidence(t *testing.T) {
	request := application.AnalyzeConversationRequestV1{SchemaVersion: application.AnalysisSchemaV2,
		PromptVersion: application.AnalysisPromptV7, AnalysisThroughMessageID: "m2",
		Messages: []application.ContextMessage{{ID: "m1", Direction: "INCOMING", Body: "Хочу купить услугу"}, {ID: "m2", Direction: "OUTGOING", Body: "Свободно завтра в 15:00"}}}
	prompt, _ := json.Marshal(request)
	raw, err := analysisGenerationSchemaV7(string(prompt))
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties struct {
			Version struct {
				Const string `json:"const"`
			} `json:"schemaVersion"`
			Agreements struct {
				Items struct {
					OneOf []struct {
						Properties map[string]json.RawMessage `json:"properties"`
					} `json:"oneOf"`
				} `json:"items"`
			} `json:"agreements"`
			Facts struct {
				Items struct {
					OneOf []struct {
						Properties map[string]json.RawMessage `json:"properties"`
					} `json:"oneOf"`
				} `json:"items"`
			} `json:"facts"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	if schema.Properties.Version.Const != application.AnalysisSchemaV2 || len(schema.Properties.Facts.Items.OneOf) != 3 {
		t.Fatalf("unexpected v7 schema: %s", raw)
	}
	if strings.Contains(string(raw), `"trusted"`) {
		t.Fatal("model schema grants trusted authority")
	}
	if !strings.Contains(string(raw), `"PURCHASE_INTENT"`) {
		t.Fatal("purchase intent omitted")
	}
	if len(schema.Properties.Agreements.Items.OneOf) != 1 {
		t.Fatalf("unexpected agreement candidates: %s", raw)
	}
	properties := schema.Properties.Agreements.Items.OneOf[0].Properties
	if string(properties["triggerMessageId"]) != `{"const":"m2"}` || string(properties["waitingFor"]) != `{"const":"CUSTOMER"}` {
		t.Fatal("slot must expect customer confirmation and cite its outgoing message")
	}
	if strings.Contains(string(properties["evidenceMessageIds"]), `"m1"`) || !strings.Contains(string(properties["evidenceMessageIds"]), `"m2"`) {
		t.Fatal("unbounded evidence")
	}

	if _, err := analysisGenerationSchemaV6(string(prompt)); err == nil {
		t.Fatal("v1 grammar accepted v2 request")
	}
}

func TestGenerationSchemaV7CannotInventAgreementWithoutTrigger(t *testing.T) {
	for _, body := range []string{"Здравствуйте!", "Стоимость 4000 RUB", "Отмените запись", "Возможно, ответим позже"} {
		request := application.AnalyzeConversationRequestV1{SchemaVersion: application.AnalysisSchemaV2,
			AnalysisThroughMessageID: "m1", Messages: []application.ContextMessage{{ID: "m1", Direction: "OUTGOING", Body: body}}}
		prompt, _ := json.Marshal(request)
		raw, err := analysisGenerationSchemaV7(string(prompt))
		if err != nil {
			t.Fatal(err)
		}
		var schema struct{ Properties map[string]json.RawMessage }
		if json.Unmarshal(raw, &schema) != nil || string(schema.Properties["agreements"]) != `{"const":[]}` {
			t.Fatalf("unfounded agreement allowed for %q: %s", body, raw)
		}
	}
}

func TestGenerationSchemaV7OfferReplacesIncomingWait(t *testing.T) {
	for _, tc := range []struct {
		outgoing string
		want     int
	}{
		{"Да, свободно в 12:30. Записываю?", 1},
		{"Проверю, свободно ли в 12:30", 3},
	} {
		request := application.AnalyzeConversationRequestV1{SchemaVersion: application.AnalysisSchemaV2, AnalysisThroughMessageID: "offer",
			Messages: []application.ContextMessage{{ID: "question", Direction: "INCOMING", Body: "Есть свободное окно?"}, {ID: "offer", Direction: "OUTGOING", Body: tc.outgoing}}}
		prompt, _ := json.Marshal(request)
		raw, err := analysisGenerationSchemaV7(string(prompt))
		if err != nil {
			t.Fatal(err)
		}
		var schema struct {
			Properties struct {
				Agreements struct {
					Items struct{ OneOf []json.RawMessage }
				}
			}
		}
		if json.Unmarshal(raw, &schema) != nil || len(schema.Properties.Agreements.Items.OneOf) != tc.want {
			t.Fatalf("offer/promise distinction lost for %q: %s", tc.outgoing, raw)
		}
	}
}

func TestGenerationExcludesSupersededAndCompletedTriggers(t *testing.T) {
	for _, tc := range []struct {
		messages []application.ContextMessage
		excluded string
	}{
		{[]application.ContextMessage{{ID: "offer", Direction: "OUTGOING", Body: "Свободно во вторник. Вам подходит?"}, {ID: "deferral", Direction: "INCOMING", Body: "Подумаю до завтра, потом подтвержу."}}, "offer"},
		{[]application.ContextMessage{{ID: "request", Direction: "INCOMING", Body: "Перенесите запись на субботу."}, {ID: "done", Direction: "OUTGOING", Body: "Перенесли запись на субботу."}}, "done"},
		{[]application.ContextMessage{{ID: "info", Direction: "INCOMING", Body: "Вход со двора."}, {ID: "noted", Direction: "OUTGOING", Body: "Записал инструкцию по входу."}}, "noted"},
	} {
		candidates := agreementGenerationCandidates(tc.messages)
		for _, candidate := range candidates {
			encoded, _ := json.Marshal(candidate)
			var item struct {
				Properties struct {
					Trigger struct{ Const string } `json:"triggerMessageId"`
				}
			}
			if err := json.Unmarshal(encoded, &item); err != nil {
				t.Fatal(err)
			}
			if item.Properties.Trigger.Const == tc.excluded {
				t.Errorf("irrelevant trigger allowed: %s", encoded)
			}
		}
	}
}

func TestV9LatestPromiseFactKeepsIndependentAgreementAnchors(t *testing.T) {
	input := application.AnalyzeConversationRequestV1{SchemaVersion: application.AnalysisSchemaV2, PromptVersion: application.AnalysisPromptV9, AnalysisThroughMessageID: "m2",
		Messages: []application.ContextMessage{{ID: "m1", Direction: "OUTGOING", Body: "Отправлю счёт."}, {ID: "m2", Direction: "OUTGOING", Body: "Отправлю договор."}}}
	prompt, _ := application.EncodeAnalysisRequest(input)
	raw, err := analysisGenerationSchemaV7(prompt)
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties struct {
			Facts      json.RawMessage `json:"facts"`
			Agreements json.RawMessage `json:"agreements"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	var facts struct {
		Items struct {
			OneOf []struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"oneOf"`
		} `json:"items"`
	}
	if err := json.Unmarshal(schema.Properties.Facts, &facts); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, variant := range facts.Items.OneOf {
		if !strings.Contains(string(variant.Properties["type"]), "BUSINESS_COMMITMENT") {
			continue
		}
		found = true
		evidence := string(variant.Properties["evidenceMessageIds"])
		if strings.Contains(evidence, `"m1"`) || !strings.Contains(evidence, `"m2"`) {
			t.Fatalf("historical fact must cite latest promise: %s", evidence)
		}
	}
	if !found {
		t.Fatal("promise fact unavailable")
	}
	for _, id := range []string{`"m1"`, `"m2"`} {
		if !strings.Contains(string(schema.Properties.Agreements), id) {
			t.Fatalf("independent agreement lost: %s", schema.Properties.Agreements)
		}
	}
}

func TestV9BudgetAcceptanceIsNotBookingEvidence(t *testing.T) {
	input := application.AnalyzeConversationRequestV1{SchemaVersion: application.AnalysisSchemaV2, PromptVersion: application.AnalysisPromptV9, AnalysisThroughMessageID: "m1",
		Messages: []application.ContextMessage{{ID: "m1", Direction: "INCOMING", Body: "Указанная сумма меня устраивает: 2750 рублей."}}}
	prompt, _ := application.EncodeAnalysisRequest(input)
	raw, err := analysisGenerationSchemaV7(prompt)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"BOOKING_INTENT"`) {
		t.Fatalf("budget acceptance became a booking candidate: %s", raw)
	}
	if !strings.Contains(string(raw), `"2750"`) {
		t.Fatal("actual monetary evidence lost")
	}
}
