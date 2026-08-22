package application

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"lidradar/backend/internal/ai/domain"
)

const (
	AnalysisSchemaV1    = "analyze-conversation.v1"
	AnalysisPromptV1    = "analyze-conversation.prompt.v1"
	DefaultModelVersion = "local-4b-8b-q4"
	MaxContextMessages  = 20
	MaxContextRunes     = 12000 // conservative 3,000-token target
)

var ErrInvalidAIOutput = errors.New("invalid AI output")

type ContextMessage struct {
	ID        string `json:"id"`
	Direction string `json:"direction"`
	Body      string `json:"body"`
}

type ConversationContext struct {
	TenantID, ConversationID, CompanyContext, ExistingSummary string
	Revision                                                  int64
	Messages                                                  []ContextMessage
}

type AnalyzeConversationRequestV1 struct {
	Task                     string           `json:"task"`
	SchemaVersion            string           `json:"schemaVersion"`
	PromptVersion            string           `json:"promptVersion"`
	ConversationID           string           `json:"conversationId"`
	BaseConversationRevision int64            `json:"baseConversationRevision"`
	AnalysisThroughMessageID string           `json:"analysisThroughMessageId"`
	CompanyContext           string           `json:"companyContext"`
	ConversationSummary      string           `json:"conversationSummary,omitempty"`
	Messages                 []ContextMessage `json:"messages"`
}

// BuildAnalysisContext creates a bounded, versioned request from the latest
// messages. Tenant identity is used for validation but is intentionally not
// sent to the model.
func BuildAnalysisContext(c ConversationContext) (AnalyzeConversationRequestV1, error) {
	if c.TenantID == "" || c.ConversationID == "" || c.Revision < 1 || len(c.Messages) == 0 {
		return AnalyzeConversationRequestV1{}, ErrInvalid
	}
	start := len(c.Messages) - MaxContextMessages
	if start < 0 {
		start = 0
	}
	messages := append([]ContextMessage(nil), c.Messages[start:]...)
	for len(messages) > 0 && contextRunes(c.CompanyContext, c.ExistingSummary, messages) > MaxContextRunes {
		messages = messages[1:]
	}
	if len(messages) == 0 {
		return AnalyzeConversationRequestV1{}, ErrInvalid
	}
	for _, m := range messages {
		if m.ID == "" || (m.Direction != "INCOMING" && m.Direction != "OUTGOING") || strings.TrimSpace(m.Body) == "" {
			return AnalyzeConversationRequestV1{}, ErrInvalid
		}
	}
	return AnalyzeConversationRequestV1{
		Task: "ANALYZE_CONVERSATION", SchemaVersion: AnalysisSchemaV1, PromptVersion: AnalysisPromptV1,
		ConversationID: c.ConversationID, BaseConversationRevision: c.Revision,
		AnalysisThroughMessageID: messages[len(messages)-1].ID, CompanyContext: c.CompanyContext,
		ConversationSummary: c.ExistingSummary, Messages: messages,
	}, nil
}

func contextRunes(company, summary string, messages []ContextMessage) int {
	n := utf8.RuneCountInString(company) + utf8.RuneCountInString(summary)
	for _, m := range messages {
		n += utf8.RuneCountInString(m.Body)
	}
	return n
}

func EncodeAnalysisRequest(r AnalyzeConversationRequestV1) (string, error) {
	b, err := json.Marshal(r)
	return string(b), err
}

func confidenceBand(v float64) domain.ConfidenceBand {
	if v >= .85 {
		return domain.ConfidenceStrong
	}
	if v >= .65 {
		return domain.ConfidenceWeak
	}
	return domain.ConfidenceUntrusted
}

// ValidateAnalysisResultV1 performs strict JSON/schema, enum/range and
// semantic-consistency validation. Unknown fields are rejected so schema
// changes cannot silently alter application behavior.
func ValidateAnalysisResultV1(raw string, throughMessageID string) (domain.AnalysisResultV1, error) {
	var result domain.AnalysisResultV1
	dec := json.NewDecoder(bytes.NewBufferString(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&result); err != nil {
		return result, fmt.Errorf("%w: JSON: %v", ErrInvalidAIOutput, err)
	}
	if dec.Decode(&struct{}{}) == nil {
		return result, fmt.Errorf("%w: multiple JSON values", ErrInvalidAIOutput)
	}
	if result.SchemaVersion != AnalysisSchemaV1 || result.AnalysisThroughMessageID == "" || (throughMessageID != "" && result.AnalysisThroughMessageID != throughMessageID) || strings.TrimSpace(result.Summary) == "" || result.Facts == nil {
		return result, fmt.Errorf("%w: missing or mismatched required field", ErrInvalidAIOutput)
	}
	if utf8.RuneCountInString(result.Summary) > 2000 {
		return result, fmt.Errorf("%w: summary too long", ErrInvalidAIOutput)
	}
	for i, fact := range result.Facts {
		switch fact.Type {
		case domain.FactBookingIntent, domain.FactBusinessCommitment, domain.FactPriceMentioned, domain.FactFollowUpCandidate:
		default:
			return result, fmt.Errorf("%w: fact %d has unknown type", ErrInvalidAIOutput, i)
		}
		if fact.Confidence < 0 || fact.Confidence > 1 {
			return result, fmt.Errorf("%w: fact %d confidence out of range", ErrInvalidAIOutput, i)
		}
		if len(fact.EvidenceMessageIDs) == 0 {
			return result, fmt.Errorf("%w: fact %d has no evidence", ErrInvalidAIOutput, i)
		}
		for _, id := range fact.EvidenceMessageIDs {
			if id == "" {
				return result, fmt.Errorf("%w: empty evidence id", ErrInvalidAIOutput)
			}
		}
		if fact.Type == domain.FactPriceMentioned {
			if fact.Value && (fact.Amount == nil || *fact.Amount == "" || fact.Currency == "") {
				return result, fmt.Errorf("%w: mentioned price lacks amount/currency", ErrInvalidAIOutput)
			}
			if !fact.Value && (fact.Amount != nil || fact.Currency != "") {
				return result, fmt.Errorf("%w: unmentioned price has amount/currency", ErrInvalidAIOutput)
			}
		} else if fact.Amount != nil || fact.Currency != "" {
			return result, fmt.Errorf("%w: price fields on non-price fact", ErrInvalidAIOutput)
		}
	}
	return result, nil
}

func TrustedFacts(result domain.AnalysisResultV1) []domain.SemanticFact {
	trusted := make([]domain.SemanticFact, 0, len(result.Facts))
	for _, fact := range result.Facts {
		if confidenceBand(fact.Confidence) != domain.ConfidenceUntrusted {
			trusted = append(trusted, fact)
		}
	}
	return trusted
}
