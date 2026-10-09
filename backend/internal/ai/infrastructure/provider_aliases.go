package infrastructure

import (
	"encoding/json"
	"fmt"
	"strconv"

	"lidradar/backend/internal/ai/application"
)

// V8 uses short, request-local message IDs to reserve context for the dialogue
// and its answer. Canonical evidence IDs are restored before leaving the
// provider; the persisted job, messages and server validation stay unchanged.
type analysisAliases struct {
	canonical map[string]string
	through   string
}

func aliasAnalysisRequest(prompt string) (string, analysisAliases, error) {
	var request application.AnalyzeConversationRequestV1
	if err := json.Unmarshal([]byte(prompt), &request); err != nil {
		return "", analysisAliases{}, err
	}
	aliases := analysisAliases{canonical: make(map[string]string, len(request.Messages))}
	seen := map[string]bool{}
	for i := range request.Messages {
		id := request.Messages[i].ID
		if id == "" || seen[id] {
			return "", analysisAliases{}, fmt.Errorf("invalid message identity in generation context")
		}
		seen[id] = true
		alias := "m" + strconv.Itoa(i+1)
		aliases.canonical[alias] = id
		request.Messages[i].ID = alias
		if id == request.AnalysisThroughMessageID {
			aliases.through = alias
		}
	}
	if aliases.through == "" {
		return "", analysisAliases{}, fmt.Errorf("analysis boundary is outside generation context")
	}
	request.AnalysisThroughMessageID = aliases.through
	request.ConversationID = "current-conversation"
	encoded, err := application.EncodeAnalysisRequest(request)
	return encoded, aliases, err
}

func (aliases analysisAliases) restore(raw string) (string, error) {
	result, err := application.ValidateAnalysisResultV2(raw, aliases.through)
	if err != nil {
		return "", err
	}
	resolve := func(id *string) error {
		canonical, ok := aliases.canonical[*id]
		if !ok {
			return fmt.Errorf("model evidence is outside generation context")
		}
		*id = canonical
		return nil
	}
	if err := resolve(&result.AnalysisThroughMessageID); err != nil {
		return "", err
	}
	for i := range result.Facts {
		for j := range result.Facts[i].EvidenceMessageIDs {
			if err := resolve(&result.Facts[i].EvidenceMessageIDs[j]); err != nil {
				return "", err
			}
		}
	}
	for i := range result.Agreements {
		if err := resolve(&result.Agreements[i].TriggerMessageID); err != nil {
			return "", err
		}
		for j := range result.Agreements[i].EvidenceMessageIDs {
			if err := resolve(&result.Agreements[i].EvidenceMessageIDs[j]); err != nil {
				return "", err
			}
		}
	}
	encoded, err := json.Marshal(result)
	return string(encoded), err
}
