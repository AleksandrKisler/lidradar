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
		request := application.AnalyzeConversationRequestV1{AnalysisThroughMessageID: "m1", CompanyContext: "Цена 9999 RUB", ConversationSummary: "Ранее 8888 рублей",
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
