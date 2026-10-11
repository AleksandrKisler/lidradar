// Команда ai-lexicon-audit измеряет словарь обещаний компании без обращения к модели: какая доля сообщений-обещаний
// набора проходит проверку PossibleBusinessCommitmentEvidence и сколько сообщений без обещания она принимает.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"

	"lidradar/backend/internal/ai/application"
	"lidradar/backend/internal/ai/benchmark"
	"lidradar/backend/internal/ai/domain"
)

type categoryReport struct {
	Positives       int `json:"positives"`
	Covered         int `json:"covered"`
	Negatives       int `json:"negatives"`
	FalselyAccepted int `json:"falselyAccepted"`
}

type report struct {
	Dataset         string                    `json:"dataset"`
	SHA256          string                    `json:"sha256"`
	Cases           int                       `json:"cases"`
	Positives       int                       `json:"positives"`
	Covered         int                       `json:"covered"`
	Coverage        float64                   `json:"coverage"`
	Negatives       int                       `json:"negatives"`
	FalselyAccepted int                       `json:"falselyAccepted"`
	FalseAcceptRate float64                   `json:"falseAcceptRate"`
	ByCategory      map[string]categoryReport `json:"byCategory"`
	Missed          []finding                 `json:"missed,omitempty"`
	AcceptedWrongly []finding                 `json:"acceptedWrongly,omitempty"`
}

type finding struct {
	CaseID string `json:"caseId"`
	Text   string `json:"text,omitempty"`
}

var trailingNumber = regexp.MustCompile(`-\d+$`)

// category берёт название группы из идентификатора случая: «cm-dev-offer-03» → «cm-dev-offer».
func category(caseID string) string {
	return trailingNumber.ReplaceAllString(caseID, "")
}

// audit сверяет решение словаря с разметкой: положительным считается сообщение, на которое ссылается ожидаемый
// факт BUSINESS_COMMITMENT, отрицательным — любое другое исходящее сообщение набора.
func audit(cases []benchmark.Case, withTexts bool) report {
	result := report{Cases: len(cases), ByCategory: map[string]categoryReport{}}
	for _, c := range cases {
		promised := map[string]bool{}
		for _, fact := range c.Expected {
			if fact.Type == domain.FactBusinessCommitment && fact.Value {
				for _, id := range fact.EvidenceMessageIDs {
					promised[id] = true
				}
			}
		}
		group := category(c.ID)
		entry := result.ByCategory[group]
		for _, message := range c.Input.Messages {
			if message.Direction != "OUTGOING" {
				continue
			}
			accepted := application.PossibleBusinessCommitmentEvidence(application.ContextMessage{ID: message.ID, Direction: message.Direction, Body: message.Body})
			note := finding{CaseID: c.ID}
			if withTexts {
				note.Text = message.Body
			}
			switch {
			case promised[message.ID]:
				result.Positives++
				entry.Positives++
				if accepted {
					result.Covered++
					entry.Covered++
				} else {
					result.Missed = append(result.Missed, note)
				}
			default:
				result.Negatives++
				entry.Negatives++
				if accepted {
					result.FalselyAccepted++
					entry.FalselyAccepted++
					result.AcceptedWrongly = append(result.AcceptedWrongly, note)
				}
			}
		}
		result.ByCategory[group] = entry
	}
	result.Coverage = ratio(result.Covered, result.Positives)
	result.FalseAcceptRate = ratio(result.FalselyAccepted, result.Negatives)
	sort.Slice(result.Missed, func(i, j int) bool { return result.Missed[i].CaseID < result.Missed[j].CaseID })
	sort.Slice(result.AcceptedWrongly, func(i, j int) bool { return result.AcceptedWrongly[i].CaseID < result.AcceptedWrongly[j].CaseID })
	return result
}

func ratio(part, whole int) float64 {
	if whole == 0 {
		return 0
	}
	return float64(part) / float64(whole)
}

func main() {
	dataset := flag.String("dataset", "models/datasets/commitment_dev_v1.jsonl", "набор в формате lidradar-ai-benchmark.v1")
	texts := flag.Bool("texts", false, "печатать тексты пропусков и ложных срабатываний; только для наборов DEV, контрольные выборки так не читают")
	summary := flag.Bool("summary", false, "печатать только итоги и группы без списков случаев")
	flag.Parse()
	file, err := os.Open(*dataset)
	fatal(err)
	defer file.Close()
	cases, digest, err := benchmark.Load(file)
	fatal(err)
	result := audit(cases, *texts)
	result.Dataset, result.SHA256 = *dataset, digest
	if *summary {
		result.Missed, result.AcceptedWrongly = nil, nil
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	fatal(encoder.Encode(result))
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
