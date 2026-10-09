package infrastructure

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"lidradar/backend/internal/ai/application"
)

// Generation is a subset of the unchanged public result contract. Candidate
// enums prevent inventing amounts or IDs; they do not replace server validation.
// Preserve property order: summary precedes facts in constrained generation.
func analysisGenerationSchemaV6(prompt string) (json.RawMessage, error) {
	return analysisGenerationSchema(prompt, false)
}

func analysisGenerationSchemaV7(prompt string) (json.RawMessage, error) {
	return analysisGenerationSchema(prompt, true)
}

func analysisGenerationSchema(prompt string, v2 bool) (json.RawMessage, error) {
	var request application.AnalyzeConversationRequestV1
	if err := json.Unmarshal([]byte(prompt), &request); err != nil || len(request.Messages) == 0 || request.AnalysisThroughMessageID == "" {
		return nil, errors.New("price-grounded generation requires message context")
	}
	expectedVersion := application.AnalysisSchemaV1
	if v2 {
		expectedVersion = application.AnalysisSchemaV2
	}
	if request.SchemaVersion != expectedVersion && (v2 || request.SchemaVersion != "") {
		return nil, errors.New("generation schema version mismatch")
	}
	ids := make([]string, 0, len(request.Messages))
	throughFound := false
	for _, message := range request.Messages {
		if message.ID == "" {
			return nil, errors.New("generation context has no message ID")
		}
		ids = append(ids, message.ID)
		throughFound = throughFound || message.ID == request.AnalysisThroughMessageID
	}
	if !throughFound {
		return nil, errors.New("generation boundary is outside message context")
	}
	idJSON, _ := json.Marshal(ids)
	throughJSON, _ := json.Marshal(request.AnalysisThroughMessageID)
	evidence := fmt.Sprintf(`{"type":"array","minItems":1,"items":{"type":"string","enum":%s}}`, idJSON)
	types := []string{"BOOKING_INTENT", "BUSINESS_COMMITMENT", "FOLLOW_UP_CANDIDATE"}
	if v2 {
		possiblePromise := false
		possibleFollowUp := false
		for _, m := range request.Messages {
			possibleFollowUp = possibleFollowUp || (m.Direction == "INCOMING" && application.HasFollowUpDeferral(m.Body))
			if m.Direction == "INCOMING" && application.HasExplicitPurchaseIntent(m.Body) {
				if types[len(types)-1] != "PURCHASE_INTENT" {
					types = append(types, "PURCHASE_INTENT")
				}
			}
			b := strings.ToLower(m.Body)
			if application.PossibleBusinessCommitmentEvidence(m) && !strings.Contains(b, "обычно") {
				possiblePromise = true
			}
		}
		if !possiblePromise || !possibleFollowUp {
			filtered := types[:0]
			for _, kind := range types {
				if (kind == "BUSINESS_COMMITMENT" && !possiblePromise) || (kind == "FOLLOW_UP_CANDIDATE" && !possibleFollowUp) {
					continue
				}
				filtered = append(filtered, kind)
			}
			types = filtered
		}
	}
	factTypes, _ := json.Marshal(types)
	other := fmt.Sprintf(`{"type":"object","additionalProperties":false,
"required":["type","value","confidence","evidenceMessageIds"],"properties":{
"type":{"enum":%s},
"value":{"type":"boolean"},"confidence":{"type":"number","minimum":0,"maximum":1},
"evidenceMessageIds":%s}}`, factTypes, evidence)
	if v2 {
		var variants []string
		for _, kind := range types {
			var eligible []string
			for _, m := range request.Messages {
				allowed := m.Direction == "INCOMING"
				switch kind {
				case "BOOKING_INTENT":
					allowed = application.PossibleBookingEvidence(m)
				case "BUSINESS_COMMITMENT":
					allowed = application.PossibleBusinessCommitmentEvidence(m)
				case "FOLLOW_UP_CANDIDATE":
					allowed = allowed && application.HasFollowUpDeferral(m.Body)
				case "PURCHASE_INTENT":
					allowed = allowed && application.HasExplicitPurchaseIntent(m.Body)
				}
				if allowed {
					eligible = append(eligible, m.ID)
				}
			}
			if kind == "BOOKING_INTENT" {
				// The contract asks for the latest direct proof, not every
				// historical repetition. Keep fallback wording model-assessed.
				for _, m := range request.Messages {
					if application.ExplicitBookingEvidence(m) && (request.PromptVersion != application.AnalysisPromptV9 || application.PossibleBookingEvidence(m)) {
						eligible = []string{m.ID}
					}
				}
			}
			if kind == "BUSINESS_COMMITMENT" && request.PromptVersion == application.AnalysisPromptV9 && len(eligible) > 1 {
				eligible = eligible[len(eligible)-1:]
			}
			if len(eligible) == 0 {
				continue
			}
			ids, _ := json.Marshal(eligible)
			typeJSON, _ := json.Marshal(kind)
			variants = append(variants, fmt.Sprintf(`{"type":"object","additionalProperties":false,"required":["type","value","confidence","evidenceMessageIds"],"properties":{"type":{"const":%s},"value":{"type":"boolean"},"confidence":{"type":"number","minimum":0,"maximum":1},"evidenceMessageIds":{"type":"array","minItems":1,"items":{"type":"string","enum":%s}}}}`, typeJSON, ids))
		}
		other = strings.Join(variants, ",")
	}
	negativePrice := fmt.Sprintf(`{"type":"object","additionalProperties":false,
"required":["type","value","confidence","evidenceMessageIds"],"properties":{
"type":{"const":"PRICE_MENTIONED"},"value":{"const":false},
"confidence":{"type":"number","minimum":0,"maximum":1},"evidenceMessageIds":%s}}`, evidence)
	variants := negativePrice
	if other != "" {
		variants += "," + other
	}
	if amounts := application.PriceEvidenceAmounts(request.Messages); len(amounts) > 0 {
		amountJSON, _ := json.Marshal(amounts)
		price := fmt.Sprintf(`{"type":"object","additionalProperties":false,
"required":["type","value","confidence","evidenceMessageIds","amount","currency"],"properties":{
"type":{"const":"PRICE_MENTIONED"},"value":{"const":true},
"confidence":{"type":"number","minimum":0,"maximum":1},"evidenceMessageIds":%s,
"amount":{"type":"string","enum":%s},"currency":{"type":"string","pattern":"^[A-Z]{3}$"}}}`, evidence, amountJSON)
		variants = price + "," + variants
	}
	if v2 {
		candidates := agreementGenerationCandidates(request.Messages)
		agreements := map[string]any{"const": []any{}}
		if len(candidates) > 0 {
			agreements = map[string]any{"type": "array", "maxItems": len(candidates), "items": map[string]any{"oneOf": candidates}}
		}
		agreement, err := json.Marshal(agreements)
		if err != nil {
			return nil, err
		}
		factsSchema := fmt.Sprintf(`{"type":"array","items":{"oneOf":[%s]}}`, variants)
		if request.PromptVersion == application.AnalysisPromptV9 {
			// The prompt permits one fact per type. Bound malformed repetitive
			// generation while still allowing abstention and negative values.
			factsSchema = fmt.Sprintf(`{"type":"array","maxItems":5,"items":{"oneOf":[%s]}}`, variants)
		}
		if request.PromptVersion == application.AnalysisPromptV9 && other == "" && len(application.PriceEvidenceAmounts(request.Messages)) == 0 {
			// No positive fact is eligible. Do not force the model to express a
			// refusal as an irrelevant negative price observation.
			factsSchema = `{"const":[]}`
		}
		return json.RawMessage(fmt.Sprintf(`{"type":"object","additionalProperties":false,
"required":["schemaVersion","analysisThroughMessageId","summary","facts","agreements"],"properties":{
"schemaVersion":{"const":"analyze-conversation.v2"},"analysisThroughMessageId":{"const":%s},
"summary":{"type":"string","minLength":1},"facts":%s,"agreements":%s}}`, throughJSON, factsSchema, agreement)), nil
	}
	return json.RawMessage(fmt.Sprintf(`{"type":"object","additionalProperties":false,
"required":["schemaVersion","analysisThroughMessageId","summary","facts"],"properties":{
"schemaVersion":{"const":"analyze-conversation.v1"},"analysisThroughMessageId":{"const":%s},
"summary":{"type":"string","minLength":1},"facts":{"type":"array","items":{"oneOf":[%s]}}}}`, throughJSON, variants)), nil
}
