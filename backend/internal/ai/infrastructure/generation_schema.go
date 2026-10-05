package infrastructure

import (
	"encoding/json"
	"errors"
	"fmt"

	"lidradar/backend/internal/ai/application"
)

// Generation is a subset of the unchanged public result contract. Candidate
// enums prevent inventing amounts or IDs; they do not replace server validation.
// Preserve property order: summary precedes facts in constrained generation.
func analysisGenerationSchemaV6(prompt string) (json.RawMessage, error) {
	var request application.AnalyzeConversationRequestV1
	if err := json.Unmarshal([]byte(prompt), &request); err != nil || len(request.Messages) == 0 || request.AnalysisThroughMessageID == "" {
		return nil, errors.New("price-grounded generation requires message context")
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
	other := fmt.Sprintf(`{"type":"object","additionalProperties":false,
"required":["type","value","confidence","evidenceMessageIds"],"properties":{
"type":{"enum":["BOOKING_INTENT","BUSINESS_COMMITMENT","FOLLOW_UP_CANDIDATE"]},
"value":{"type":"boolean"},"confidence":{"type":"number","minimum":0,"maximum":1},
"evidenceMessageIds":%s}}`, evidence)
	negativePrice := fmt.Sprintf(`{"type":"object","additionalProperties":false,
"required":["type","value","confidence","evidenceMessageIds"],"properties":{
"type":{"const":"PRICE_MENTIONED"},"value":{"const":false},
"confidence":{"type":"number","minimum":0,"maximum":1},"evidenceMessageIds":%s}}`, evidence)
	variants := negativePrice + "," + other
	if amounts := application.PriceEvidenceAmounts(request.Messages); len(amounts) > 0 {
		amountJSON, _ := json.Marshal(amounts)
		price := fmt.Sprintf(`{"type":"object","additionalProperties":false,
"required":["type","value","confidence","evidenceMessageIds","amount","currency"],"properties":{
"type":{"const":"PRICE_MENTIONED"},"value":{"const":true},
"confidence":{"type":"number","minimum":0,"maximum":1},"evidenceMessageIds":%s,
"amount":{"type":"string","enum":%s},"currency":{"type":"string","pattern":"^[A-Z]{3}$"}}}`, evidence, amountJSON)
		variants = price + "," + variants
	}
	return json.RawMessage(fmt.Sprintf(`{"type":"object","additionalProperties":false,
"required":["schemaVersion","analysisThroughMessageId","summary","facts"],"properties":{
"schemaVersion":{"const":"analyze-conversation.v1"},"analysisThroughMessageId":{"const":%s},
"summary":{"type":"string","minLength":1},"facts":{"type":"array","items":{"oneOf":[%s]}}}}`, throughJSON, variants)), nil
}
