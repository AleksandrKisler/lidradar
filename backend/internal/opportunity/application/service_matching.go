package application

import (
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	catalogdomain "lidradar/backend/internal/catalog/domain"
)

func matchingServices(text string, locationID *string, items []catalogdomain.ServiceCatalogItem) []catalogdomain.ServiceCatalogItem {
	words := serviceWords(text)
	if len(words) == 0 {
		return nil
	}
	matches := make([]catalogdomain.ServiceCatalogItem, 0, 1)
	for _, item := range items {
		if !item.Active || (item.LocationID != nil && (locationID == nil || *locationID != *item.LocationID)) {
			continue
		}
		name := serviceWords(item.NormalizedName)
		if len(name) == 0 || len(name) > len(words) {
			continue
		}
		forms := make([][]string, len(name))
		for i, word := range name {
			forms[i] = serviceWordForms(word)
		}
		if containsServiceName(words, forms) {
			// Collect every service, including overlapping exact/inflected names.
			// Evaluate alone decides whether there is exactly one match.
			matches = append(matches, item)
		}
	}
	return matches
}

func containsServiceName(words []string, forms [][]string) bool {
	for start := 0; start+len(forms) <= len(words); start++ {
		matched := true
		for i, alternatives := range forms {
			if !slices.Contains(alternatives, words[start+i]) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func serviceWords(value string) []string {
	return strings.FieldsFunc(strings.ToLower(value), func(character rune) bool {
		return !unicode.IsLetter(character) && !unicode.IsDigit(character)
	})
}

// serviceWordForms expands a catalog word, never stems arbitrary message text.
// Only regular singular Russian case endings are supported. Stems, token order
// and word boundaries stay intact: полировку matches полировка, полировщик does
// not. This is deliberately not a lemmatizer, synonym or typo matcher.
func serviceWordForms(word string) []string {
	forms := []string{word}
	if utf8.RuneCountInString(word) < 4 {
		return forms
	}
	for _, character := range word {
		if (character < 'а' || character > 'я') && character != 'ё' {
			return forms
		}
	}
	var ending string
	var cases []string
	switch {
	case strings.HasSuffix(word, "ая"):
		ending, cases = "ая", []string{"ую", "ой", "ою"}
	case strings.HasSuffix(word, "яя"):
		ending, cases = "яя", []string{"юю", "ей", "ею"}
	case strings.HasSuffix(word, "ый"), strings.HasSuffix(word, "ой"), strings.HasSuffix(word, "ое"):
		ending, cases = word[len(word)-len("ый"):], []string{"ого", "ому", "ым", "ом"}
	case strings.HasSuffix(word, "ий"):
		ending, cases = "ий", []string{"его", "ему", "им", "ем"}
		stem, _ := strings.CutSuffix(word, ending)
		last, _ := utf8.DecodeLastRuneInString(stem)
		if strings.ContainsRune("гкх", last) { // химический → химического, not химическего.
			cases = []string{"ого", "ому", "им", "ом"}
		}
	case strings.HasSuffix(word, "ее"):
		ending, cases = "ее", []string{"его", "ему", "им", "ем"}
	case strings.HasSuffix(word, "ия"):
		ending, cases = "ия", []string{"ии", "ию", "ией", "иею"}
	case strings.HasSuffix(word, "ие"):
		ending, cases = "ие", []string{"ия", "ию", "ием", "ии"}
	case strings.HasSuffix(word, "а"):
		ending, cases = "а", []string{"у", "е", "ой", "ою", "ы"}
		stem, _ := strings.CutSuffix(word, ending)
		last, _ := utf8.DecodeLastRuneInString(stem)
		if strings.ContainsRune("гкхжчшщ", last) {
			cases[len(cases)-1] = "и"
		}
	case strings.HasSuffix(word, "я") && !strings.HasSuffix(word, "мя"):
		ending, cases = "я", []string{"и", "е", "ю", "ей", "ею"}
	default:
		last, _ := utf8.DecodeLastRuneInString(word)
		if strings.ContainsRune("бвгджзклмнпрстфхцчшщ", last) {
			cases = []string{"а", "у", "ом", "е"}
		}
	}
	stem := strings.TrimSuffix(word, ending)
	// Short words/abbreviations and unhandled paradigms retain exact matching.
	if utf8.RuneCountInString(stem) < 3 {
		return forms
	}
	for _, suffix := range cases {
		forms = append(forms, stem+suffix)
	}
	return forms
}
