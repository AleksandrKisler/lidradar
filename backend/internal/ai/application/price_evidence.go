package application

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"lidradar/backend/internal/ai/domain"
)

var evidenceNumber = regexp.MustCompile(`[0-9]+(?:[ \x{00a0}\x{202f}][0-9]{3})*(?:[.,][0-9]+)?`)

func hasPositivePrice(result domain.AnalysisResultV1) bool {
	for _, fact := range result.Facts {
		if fact.Type == domain.FactPriceMentioned && fact.Value {
			return true
		}
	}
	return false
}

// ValidatePriceEvidence grounds positive prices in the exact job context, not
// the catalog, summary or a later conversation revision. This is a necessary
// numeric check, not a replacement for the model's monetary interpretation.
// Every cited message must contain the amount: a question cannot borrow the
// amount from another message and then drive a direction-dependent rule.
func ValidatePriceEvidence(result domain.AnalysisResultV1, prompt string) error {
	if !hasPositivePrice(result) {
		return nil
	}
	var request AnalyzeConversationRequestV1
	if err := json.Unmarshal([]byte(prompt), &request); err != nil || len(request.Messages) == 0 || request.AnalysisThroughMessageID != result.AnalysisThroughMessageID {
		return fmt.Errorf("%w: price evidence context unavailable", ErrInvalidAIOutput)
	}
	messages := make(map[string]string, len(request.Messages))
	for _, message := range request.Messages {
		messages[message.ID] = message.Body
	}
	for _, fact := range result.Facts {
		if fact.Type != domain.FactPriceMentioned || !fact.Value {
			continue
		}
		if fact.Amount == nil || !validDecimalAmount(*fact.Amount) || len(fact.EvidenceMessageIDs) == 0 {
			return fmt.Errorf("%w: price evidence lacks amount or references", ErrInvalidAIOutput)
		}
		for _, id := range fact.EvidenceMessageIDs {
			body, found := messages[id]
			if !found || !containsEvidenceAmount(body, *fact.Amount) {
				// Never include message bodies, IDs or monetary values in diagnostic logs.
				return fmt.Errorf("%w: price amount absent from cited message", ErrInvalidAIOutput)
			}
		}
	}
	return nil
}

func containsEvidenceAmount(body, amount string) bool {
	for _, span := range evidenceNumber.FindAllStringIndex(body, -1) {
		if span[0] > 0 {
			previous, _ := utf8.DecodeLastRuneInString(body[:span[0]])
			if unicode.IsLetter(previous) || unicode.IsDigit(previous) || strings.ContainsRune(":/+-_", previous) {
				continue
			}
		}
		if span[1] < len(body) {
			next, _ := utf8.DecodeRuneInString(body[span[1]:])
			if unicode.IsDigit(next) || strings.ContainsRune(":/+-_", next) {
				continue
			}
		}
		if normalizedEvidenceAmount(body[span[0]:span[1]]) == normalizedEvidenceAmount(amount) {
			return true
		}
	}
	return false
}

// Decimal string normalization avoids floating-point rounding and preserves
// arbitrary precision. Only conventional digit grouping and decimal separators
// are accepted; words and multipliers ("5 тыс.") are not guessed.
func normalizedEvidenceAmount(value string) string {
	value = strings.NewReplacer(" ", "", "\u00a0", "", "\u202f", "", ",", ".").Replace(value)
	integer, fraction, _ := strings.Cut(value, ".")
	integer = strings.TrimLeft(integer, "0")
	if integer == "" {
		integer = "0"
	}
	fraction = strings.TrimRight(fraction, "0")
	if fraction != "" {
		return integer + "." + fraction
	}
	return integer
}
