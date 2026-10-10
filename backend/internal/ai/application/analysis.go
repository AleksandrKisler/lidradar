package application

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"lidradar/backend/internal/ai/domain"
)

const (
	AnalysisSchemaV1      = "analyze-conversation.v1"
	AnalysisSchemaV2      = "analyze-conversation.v2"
	AnalysisPromptV1      = "analyze-conversation.prompt.v1"
	AnalysisPromptV2      = "analyze-conversation.prompt.v2"
	AnalysisPromptV3      = "analyze-conversation.prompt.v3"
	AnalysisPromptV4      = "analyze-conversation.prompt.v4"
	AnalysisPromptV5      = "analyze-conversation.prompt.v5"
	AnalysisPromptV6      = "analyze-conversation.prompt.v6"
	AnalysisPromptV7      = "analyze-conversation.prompt.v7"
	AnalysisPromptV8      = "analyze-conversation.prompt.v8"
	AnalysisPromptV9      = "analyze-conversation.prompt.v9"
	CurrentAnalysisPrompt = AnalysisPromptV9
	DefaultModelVersion   = "lidradar-main-v1"
	MaxContextMessages    = 20
	MaxContextRunes       = 12000 // консервативная оценка для цели в 3000 токенов
)

var ErrInvalidAIOutput = errors.New("invalid AI output")

func SupportedAnalysisPrompt(version string) bool {
	switch version {
	case AnalysisPromptV1, AnalysisPromptV2, AnalysisPromptV3, AnalysisPromptV4, AnalysisPromptV5, AnalysisPromptV6, AnalysisPromptV7, AnalysisPromptV8, AnalysisPromptV9:
		return true
	default:
		return false
	}
}

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

// AnalyzeConversationRequestV2 uses the same bounded context envelope; its
// schema and prompt versions select the new result contract.
type AnalyzeConversationRequestV2 = AnalyzeConversationRequestV1

// BuildAnalysisContext строит ограниченный версионированный запрос из последних
// сообщений. Идентификатор организации нужен для проверки, но намеренно не
// передаётся модели.
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
		Task: "ANALYZE_CONVERSATION", SchemaVersion: AnalysisSchemaV2, PromptVersion: CurrentAnalysisPrompt,
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

// ValidateAnalysisResultV1 строго проверяет JSON, схему, перечни, диапазоны и
// смысловую согласованность. Неизвестные поля отклоняются, чтобы изменение
// схемы не могло незаметно изменить поведение приложения.
func ValidateAnalysisResultV1(raw string, throughMessageID string) (domain.AnalysisResultV1, error) {
	var result domain.AnalysisResultV1
	dec := json.NewDecoder(bytes.NewBufferString(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&result); err != nil {
		return result, fmt.Errorf("%w: JSON: %v", ErrInvalidAIOutput, err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return result, fmt.Errorf("%w: trailing data after JSON", ErrInvalidAIOutput)
	}
	if result.SchemaVersion != AnalysisSchemaV1 || result.AnalysisThroughMessageID == "" || (throughMessageID != "" && result.AnalysisThroughMessageID != throughMessageID) || strings.TrimSpace(result.Summary) == "" || result.Facts == nil {
		return result, fmt.Errorf("%w: missing or mismatched required field", ErrInvalidAIOutput)
	}
	if utf8.RuneCountInString(result.Summary) > 2000 {
		return result, fmt.Errorf("%w: summary too long", ErrInvalidAIOutput)
	}
	normalizedFacts, err := validateFacts(result.Facts, false)
	if err != nil {
		return result, err
	}
	result.Facts = normalizedFacts
	return result, nil
}

// ValidateAnalysisResultV2 keeps v2 authority separate from legacy v1 runs.
// In particular, a v1 result cannot create agreements or purchase intent.
func ValidateAnalysisResultV2(raw string, throughMessageID string) (domain.AnalysisResultV2, error) {
	var result domain.AnalysisResultV2
	dec := json.NewDecoder(bytes.NewBufferString(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&result); err != nil {
		return result, fmt.Errorf("%w: JSON: %v", ErrInvalidAIOutput, err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return result, fmt.Errorf("%w: trailing data after JSON", ErrInvalidAIOutput)
	}
	if result.SchemaVersion != AnalysisSchemaV2 || result.AnalysisThroughMessageID == "" || (throughMessageID != "" && result.AnalysisThroughMessageID != throughMessageID) || strings.TrimSpace(result.Summary) == "" || result.Facts == nil || result.Agreements == nil || utf8.RuneCountInString(result.Summary) > 2000 {
		return result, fmt.Errorf("%w: missing or mismatched required field", ErrInvalidAIOutput)
	}
	var err error
	result.Facts, err = validateFacts(result.Facts, true)
	if err != nil {
		return result, err
	}
	seen := make(map[string]domain.AgreementObservation)
	for i := range result.Agreements {
		a := &result.Agreements[i]
		switch a.Kind {
		case domain.AgreementBookingConfirmation, domain.AgreementReschedule, domain.AgreementCommitment, domain.AgreementPurchaseBlocker:
		default:
			return result, fmt.Errorf("%w: unknown agreement kind", ErrInvalidAIOutput)
		}
		if a.WaitingFor != domain.AgreementCustomer && a.WaitingFor != domain.AgreementBusiness {
			return result, fmt.Errorf("%w: unknown waiting actor", ErrInvalidAIOutput)
		}
		if a.Status != domain.AgreementPending && a.Status != domain.AgreementResolved && a.Status != domain.AgreementCancelled {
			return result, fmt.Errorf("%w: unknown agreement status", ErrInvalidAIOutput)
		}
		if a.Confidence < 0 || a.Confidence > 1 || strings.TrimSpace(a.TriggerMessageID) == "" || len(a.EvidenceMessageIDs) == 0 {
			return result, fmt.Errorf("%w: incomplete agreement", ErrInvalidAIOutput)
		}
		for _, id := range a.EvidenceMessageIDs {
			if strings.TrimSpace(id) == "" {
				return result, fmt.Errorf("%w: empty agreement evidence", ErrInvalidAIOutput)
			}
		}
		a.EvidenceMessageIDs = appendUnique(nil, a.EvidenceMessageIDs...)
		key := string(a.Kind) + "\x00" + a.TriggerMessageID
		if _, duplicate := seen[key]; duplicate {
			return result, fmt.Errorf("%w: duplicate or contradictory agreement", ErrInvalidAIOutput)
		}
		seen[key] = *a
	}
	return result, nil
}

func validateFacts(facts []domain.SemanticFact, allowPurchase bool) ([]domain.SemanticFact, error) {
	normalizedFacts := make([]domain.SemanticFact, 0, len(facts))
	factIndexes := make(map[domain.FactType]int, len(facts))
	for i, fact := range facts {
		switch fact.Type {
		case domain.FactBookingIntent, domain.FactBusinessCommitment, domain.FactPriceMentioned, domain.FactFollowUpCandidate:
		case domain.FactPurchaseIntent:
			if !allowPurchase {
				return nil, fmt.Errorf("%w: fact %d has unknown type", ErrInvalidAIOutput, i)
			}
		default:
			return nil, fmt.Errorf("%w: fact %d has unknown type", ErrInvalidAIOutput, i)
		}
		if fact.Confidence < 0 || fact.Confidence > 1 {
			return nil, fmt.Errorf("%w: fact %d confidence out of range", ErrInvalidAIOutput, i)
		}
		if len(fact.EvidenceMessageIDs) == 0 {
			return nil, fmt.Errorf("%w: fact %d has no evidence", ErrInvalidAIOutput, i)
		}
		evidenceSeen := make(map[string]struct{}, len(fact.EvidenceMessageIDs))
		normalizedEvidence := make([]string, 0, len(fact.EvidenceMessageIDs))
		for _, id := range fact.EvidenceMessageIDs {
			if strings.TrimSpace(id) == "" {
				return nil, fmt.Errorf("%w: empty evidence id", ErrInvalidAIOutput)
			}
			if _, duplicated := evidenceSeen[id]; !duplicated {
				evidenceSeen[id] = struct{}{}
				normalizedEvidence = append(normalizedEvidence, id)
			}
		}
		fact.EvidenceMessageIDs = normalizedEvidence
		if fact.Type == domain.FactPriceMentioned {
			if fact.Value && fact.Amount != nil {
				amount := normalizeModelAmount(*fact.Amount)
				fact.Amount = &amount
			}
			if fact.Value && (fact.Amount == nil || !validDecimalAmount(*fact.Amount) || !validCurrency(fact.Currency)) {
				return nil, fmt.Errorf("%w: mentioned price lacks amount/currency", ErrInvalidAIOutput)
			}
			if !fact.Value && (fact.Amount != nil || fact.Currency != "") {
				return nil, fmt.Errorf("%w: unmentioned price has amount/currency", ErrInvalidAIOutput)
			}
		} else if fact.Amount != nil || fact.Currency != "" {
			return nil, fmt.Errorf("%w: price fields on non-price fact", ErrInvalidAIOutput)
		}
		if index, duplicated := factIndexes[fact.Type]; duplicated {
			existing := &normalizedFacts[index]
			if existing.Value != fact.Value || existing.Currency != fact.Currency || !sameOptionalString(existing.Amount, fact.Amount) {
				return nil, fmt.Errorf("%w: fact %d contradicts type %s", ErrInvalidAIOutput, i, fact.Type)
			}
			if fact.Confidence < existing.Confidence {
				existing.Confidence = fact.Confidence
			}
			existing.EvidenceMessageIDs = appendUnique(existing.EvidenceMessageIDs, fact.EvidenceMessageIDs...)
			continue
		}
		factIndexes[fact.Type] = len(normalizedFacts)
		normalizedFacts = append(normalizedFacts, fact)
	}
	return normalizedFacts, nil
}

// ConflictingFactTypes возвращает типы, для которых ответ содержит противоречащие друг другу факты
// (то же значение, сумма и валюта у одного типа — не противоречие). Сравнение такое же, как в
// validateFacts: сумма приводится к каноническому виду до сравнения. Серверная проверка отвергает
// такой ответ целиком; поставщик использует эту функцию, чтобы убрать только спорные факты.
func ConflictingFactTypes(facts []domain.SemanticFact) map[domain.FactType]bool {
	conflicting := map[domain.FactType]bool{}
	first := make(map[domain.FactType]domain.SemanticFact, len(facts))
	for _, fact := range facts {
		if fact.Type == domain.FactPriceMentioned && fact.Value && fact.Amount != nil {
			amount := normalizeModelAmount(*fact.Amount)
			fact.Amount = &amount
		}
		previous, seen := first[fact.Type]
		if !seen {
			first[fact.Type] = fact
			continue
		}
		if previous.Value != fact.Value || previous.Currency != fact.Currency || !sameOptionalString(previous.Amount, fact.Amount) {
			conflicting[fact.Type] = true
		}
	}
	return conflicting
}

func sameOptionalString(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func validCurrency(value string) bool {
	if len(value) != 3 {
		return false
	}
	for index := 0; index < len(value); index++ {
		if value[index] < 'A' || value[index] > 'Z' {
			return false
		}
	}
	return true
}

func validDecimalAmount(value string) bool {
	if value == "" {
		return false
	}
	dotSeen := false
	digitSeen := false
	for index := 0; index < len(value); index++ {
		switch {
		case value[index] >= '0' && value[index] <= '9':
			digitSeen = true
		case value[index] == '.' && !dotSeen && index > 0 && index < len(value)-1:
			dotSeen = true
		default:
			return false
		}
	}
	return digitSeen
}

// Accept a decimal comma only with one or two fractional digits. Multiple
// separators and ambiguous thousands notation remain invalid. The original
// Run.Output is untouched; only the parsed fact becomes canonical.
func normalizeModelAmount(value string) string {
	if strings.Count(value, ",") == 1 && !strings.Contains(value, ".") {
		_, fraction, _ := strings.Cut(value, ",")
		if len(fraction) >= 1 && len(fraction) <= 2 {
			candidate := strings.Replace(value, ",", ".", 1)
			if validDecimalAmount(candidate) {
				return candidate
			}
		}
	}
	return value
}

func appendUnique(values []string, additional ...string) []string {
	seen := make(map[string]struct{}, len(values)+len(additional))
	for _, value := range values {
		seen[value] = struct{}{}
	}
	for _, value := range additional {
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		values = append(values, value)
	}
	return values
}

// AppliedFacts применяет уровни уверенности §59 к проверенному результату.
// Сильный факт (>= 0.85) получает trusted = true и может открывать Risk;
// слабый (0.65–0.849) сохраняется с trusted = false для наблюдения и
// метрик; ненадёжный (< 0.65) в домен не попадает вовсе.
func AppliedFacts(result domain.AnalysisResultV1) []domain.AppliedFact {
	applied := make([]domain.AppliedFact, 0, len(result.Facts))
	for _, fact := range result.Facts {
		band := confidenceBand(fact.Confidence)
		if band == domain.ConfidenceUntrusted {
			continue
		}
		applied = append(applied, domain.AppliedFact{SemanticFact: fact, Trusted: band == domain.ConfidenceStrong})
	}
	return applied
}

// TrustedFacts возвращает только факты, которым разрешено менять предметную
// область. Именно их сравнивает с разметкой benchmark: слабый факт не открывает
// Risk и потому не считается обнаруженным.
func TrustedFacts(result domain.AnalysisResultV1) []domain.SemanticFact {
	trusted := make([]domain.SemanticFact, 0, len(result.Facts))
	for _, fact := range AppliedFacts(result) {
		if fact.Trusted {
			trusted = append(trusted, fact.SemanticFact)
		}
	}
	return trusted
}
