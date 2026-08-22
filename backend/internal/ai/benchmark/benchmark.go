// Package benchmark evaluates conversation-analysis providers against a
// versioned, labelled JSONL dataset. It is an offline engineering tool and
// never changes application or domain state.
package benchmark

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"lidradar/backend/internal/ai/application"
	"lidradar/backend/internal/ai/domain"
)

const DatasetVersion = "lidradar-ai-benchmark.v1"

type Split string

const (
	SplitTrain      Split = "TRAIN"
	SplitValidation Split = "VALIDATION"
	SplitGolden     Split = "GOLDEN"
)

type Case struct {
	Version  string                                   `json:"version"`
	ID       string                                   `json:"id"`
	Split    Split                                    `json:"split"`
	Input    application.AnalyzeConversationRequestV1 `json:"input"`
	Expected []domain.SemanticFact                    `json:"expectedFacts"`
}

type Provider interface {
	Infer(context.Context, string) (string, error)
}

type Thresholds struct {
	MinimumPrecision float64       `json:"minimumPrecision"`
	MinimumRecall    float64       `json:"minimumRecall"`
	MinimumF1        float64       `json:"minimumF1"`
	MinimumExactRate float64       `json:"minimumExactRate"`
	MaximumP95       time.Duration `json:"-"`
	MaximumP95MS     int64         `json:"maximumP95Ms"`
}

type Report struct {
	DatasetSHA256            string  `json:"datasetSha256"`
	Cases                    int     `json:"cases"`
	TruePositive             int     `json:"truePositive"`
	FalsePositive            int     `json:"falsePositive"`
	FalseNegative            int     `json:"falseNegative"`
	Invalid                  int     `json:"invalid"`
	Exact                    int     `json:"exact"`
	Precision                float64 `json:"precision"`
	Recall                   float64 `json:"recall"`
	F1                       float64 `json:"f1"`
	ExactRate                float64 `json:"exactRate"`
	P50MS                    int64   `json:"p50Ms"`
	P95MS                    int64   `json:"p95Ms"`
	P99MS                    int64   `json:"p99Ms"`
	ThroughputCasesPerSecond float64 `json:"throughputCasesPerSecond"`
	Passed                   bool    `json:"passed"`
}

func Load(r io.Reader) ([]Case, string, error) {
	h := sha256.New()
	s := bufio.NewScanner(io.TeeReader(r, h))
	s.Buffer(make([]byte, 64*1024), 2*1024*1024)
	seen := map[string]struct{}{}
	var cases []Case
	for line := 1; s.Scan(); line++ {
		if strings.TrimSpace(s.Text()) == "" {
			continue
		}
		var c Case
		dec := json.NewDecoder(strings.NewReader(s.Text()))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return nil, "", fmt.Errorf("dataset line %d: %w", line, err)
		}
		if c.Version != DatasetVersion || c.ID == "" || (c.Split != SplitTrain && c.Split != SplitValidation && c.Split != SplitGolden) || c.Expected == nil {
			return nil, "", fmt.Errorf("dataset line %d: invalid metadata or missing labels", line)
		}
		if _, ok := seen[c.ID]; ok {
			return nil, "", fmt.Errorf("dataset line %d: duplicate id %q", line, c.ID)
		}
		if c.Input.SchemaVersion != application.AnalysisSchemaV1 || c.Input.PromptVersion != application.AnalysisPromptV1 || c.Input.AnalysisThroughMessageID == "" {
			return nil, "", fmt.Errorf("dataset line %d: incompatible input contract", line)
		}
		seen[c.ID] = struct{}{}
		cases = append(cases, c)
	}
	if err := s.Err(); err != nil {
		return nil, "", err
	}
	if len(cases) == 0 {
		return nil, "", errors.New("dataset is empty")
	}
	return cases, hex.EncodeToString(h.Sum(nil)), nil
}

// VerifyGolden prevents an accidental golden-set change. Updating the expected
// digest is an explicit review action, never an automatic benchmark side effect.
func VerifyGolden(datasetSHA256, expectedSHA256 string) error {
	if expectedSHA256 == "" || !strings.EqualFold(datasetSHA256, expectedSHA256) {
		return fmt.Errorf("golden dataset checksum mismatch: got %s", datasetSHA256)
	}
	return nil
}

func Run(ctx context.Context, provider Provider, cases []Case, datasetSHA string, thresholds Thresholds) (Report, error) {
	if provider == nil || len(cases) == 0 {
		return Report{}, errors.New("provider and cases are required")
	}
	started := time.Now()
	latencies := make([]time.Duration, 0, len(cases))
	report := Report{DatasetSHA256: datasetSHA, Cases: len(cases)}
	for _, c := range cases {
		prompt, err := application.EncodeAnalysisRequest(c.Input)
		if err != nil {
			return report, err
		}
		begin := time.Now()
		raw, err := provider.Infer(ctx, prompt)
		latencies = append(latencies, time.Since(begin))
		if err != nil {
			if ctx.Err() != nil {
				return report, ctx.Err()
			}
			report.Invalid++
			report.FalseNegative += len(c.Expected)
			continue
		}
		result, err := application.ValidateAnalysisResultV1(raw, c.Input.AnalysisThroughMessageID)
		if err != nil {
			report.Invalid++
			report.FalseNegative += len(c.Expected)
			continue
		}
		tp, fp, fn := compare(application.TrustedFacts(result), c.Expected)
		report.TruePositive += tp
		report.FalsePositive += fp
		report.FalseNegative += fn
		if fp == 0 && fn == 0 {
			report.Exact++
		}
	}
	report.Precision = ratio(report.TruePositive, report.TruePositive+report.FalsePositive)
	report.Recall = ratio(report.TruePositive, report.TruePositive+report.FalseNegative)
	report.F1 = ratio(2*report.TruePositive, 2*report.TruePositive+report.FalsePositive+report.FalseNegative)
	report.ExactRate = ratio(report.Exact, report.Cases)
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	report.P50MS = percentile(latencies, .50).Milliseconds()
	p95 := percentile(latencies, .95)
	report.P95MS = p95.Milliseconds()
	report.P99MS = percentile(latencies, .99).Milliseconds()
	elapsed := time.Since(started).Seconds()
	if elapsed > 0 {
		report.ThroughputCasesPerSecond = float64(report.Cases) / elapsed
	}
	maxP95 := thresholds.MaximumP95
	if maxP95 == 0 && thresholds.MaximumP95MS > 0 {
		maxP95 = time.Duration(thresholds.MaximumP95MS) * time.Millisecond
	}
	report.Passed = report.Precision >= thresholds.MinimumPrecision && report.Recall >= thresholds.MinimumRecall && report.F1 >= thresholds.MinimumF1 && report.ExactRate >= thresholds.MinimumExactRate && (maxP95 == 0 || p95 <= maxP95)
	return report, nil
}

func compare(actual, expected []domain.SemanticFact) (tp, fp, fn int) {
	remaining := append([]domain.SemanticFact(nil), expected...)
	for _, a := range actual {
		match := -1
		for i, e := range remaining {
			if sameFact(a, e) {
				match = i
				break
			}
		}
		if match < 0 {
			fp++
			continue
		}
		tp++
		remaining = append(remaining[:match], remaining[match+1:]...)
	}
	return tp, fp, len(remaining)
}

func sameFact(a, b domain.SemanticFact) bool {
	if a.Type != b.Type || a.Value != b.Value || a.Currency != b.Currency || (a.Amount == nil) != (b.Amount == nil) || !sameStrings(a.EvidenceMessageIDs, b.EvidenceMessageIDs) {
		return false
	}
	return a.Amount == nil || *a.Amount == *b.Amount
}
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	aa, bb := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(aa)
	sort.Strings(bb)
	for i := range aa {
		if aa[i] != bb[i] {
			return false
		}
	}
	return true
}
func ratio(a, b int) float64 {
	if b == 0 {
		return 1
	}
	return float64(a) / float64(b)
}
func percentile(v []time.Duration, p float64) time.Duration {
	if len(v) == 0 {
		return 0
	}
	i := int(float64(len(v)-1)*p + .5)
	return v[i]
}
