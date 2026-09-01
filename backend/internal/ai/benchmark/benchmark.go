// Package benchmark проверяет поставщиков анализа переписки на версионированном
// размеченном JSONL-наборе. Это автономный инженерный инструмент, который не
// изменяет состояние приложения и предметной области.
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
	"strconv"
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
	MinimumPrecision         float64       `json:"minimumPrecision"`
	MinimumFactPrecision     float64       `json:"minimumFactPrecision"`
	MinimumRecall            float64       `json:"minimumRecall"`
	MinimumF1                float64       `json:"minimumF1"`
	MinimumExactRate         float64       `json:"minimumExactRate"`
	MinimumValidRate         float64       `json:"minimumValidRate"`
	MinimumEvidenceExactRate float64       `json:"minimumEvidenceExactRate"`
	MaximumP95               time.Duration `json:"-"`
	MaximumP95MS             int64         `json:"maximumP95Ms"`
}

type Report struct {
	DatasetSHA256            string                 `json:"datasetSha256"`
	Cases                    int                    `json:"cases"`
	TruePositive             int                    `json:"truePositive"`
	FalsePositive            int                    `json:"falsePositive"`
	FalseNegative            int                    `json:"falseNegative"`
	Invalid                  int                    `json:"invalid"`
	Exact                    int                    `json:"exact"`
	Precision                float64                `json:"precision"`
	Recall                   float64                `json:"recall"`
	F1                       float64                `json:"f1"`
	ExactRate                float64                `json:"exactRate"`
	ValidRate                float64                `json:"validRate"`
	EvidenceExactRate        float64                `json:"evidenceExactRate"`
	P50MS                    int64                  `json:"p50Ms"`
	P95MS                    int64                  `json:"p95Ms"`
	P99MS                    int64                  `json:"p99Ms"`
	ThroughputCasesPerSecond float64                `json:"throughputCasesPerSecond"`
	ByFactType               map[string]FactMetrics `json:"byFactType"`
	Failures                 []Failure              `json:"failures,omitempty"`
	Passed                   bool                   `json:"passed"`
}

type FactMetrics struct {
	TruePositive  int     `json:"truePositive"`
	FalsePositive int     `json:"falsePositive"`
	FalseNegative int     `json:"falseNegative"`
	Precision     float64 `json:"precision"`
	Recall        float64 `json:"recall"`
	F1            float64 `json:"f1"`
}

type Failure struct {
	CaseID   string                `json:"caseId"`
	Reason   string                `json:"reason"`
	Expected []domain.SemanticFact `json:"expectedFacts,omitempty"`
	Actual   []domain.SemanticFact `json:"actualFacts,omitempty"`
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
			return nil, "", fmt.Errorf("строка набора %d: %w", line, err)
		}
		if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			return nil, "", fmt.Errorf("строка набора %d: лишние данные после JSON", line)
		}
		if c.Version != DatasetVersion || c.ID == "" || (c.Split != SplitTrain && c.Split != SplitValidation && c.Split != SplitGolden) || c.Expected == nil {
			return nil, "", fmt.Errorf("строка набора %d: неверные метаданные или отсутствует разметка", line)
		}
		if _, ok := seen[c.ID]; ok {
			return nil, "", fmt.Errorf("строка набора %d: повтор идентификатора %q", line, c.ID)
		}
		if err := validateCase(c); err != nil {
			return nil, "", fmt.Errorf("строка набора %d: %w", line, err)
		}
		seen[c.ID] = struct{}{}
		cases = append(cases, c)
	}
	if err := s.Err(); err != nil {
		return nil, "", err
	}
	if len(cases) == 0 {
		return nil, "", errors.New("набор пуст")
	}
	return cases, hex.EncodeToString(h.Sum(nil)), nil
}

// VerifyGolden защищает контрольную выборку от случайного изменения. Новая
// контрольная сумма появляется только после явного ревью, а не как побочный
// эффект запуска проверки.
func VerifyGolden(datasetSHA256, expectedSHA256 string) error {
	if expectedSHA256 == "" || !strings.EqualFold(datasetSHA256, expectedSHA256) {
		return fmt.Errorf("контрольная сумма golden-набора не совпала: получено %s", datasetSHA256)
	}
	return nil
}

func Run(ctx context.Context, provider Provider, cases []Case, datasetSHA string, thresholds Thresholds) (Report, error) {
	if provider == nil || len(cases) == 0 {
		return Report{}, errors.New("поставщик модели и случаи обязательны")
	}
	started := time.Now()
	latencies := make([]time.Duration, 0, len(cases))
	report := Report{DatasetSHA256: datasetSHA, Cases: len(cases), ByFactType: map[string]FactMetrics{}}
	matchedFacts := 0
	exactEvidence := 0
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
			addFalseNegatives(report.ByFactType, c.Expected)
			report.Failures = append(report.Failures, Failure{CaseID: c.ID, Reason: "ошибка вызова модели", Expected: c.Expected})
			continue
		}
		result, err := application.ValidateAnalysisResultV1(raw, c.Input.AnalysisThroughMessageID)
		if err != nil {
			report.Invalid++
			report.FalseNegative += len(c.Expected)
			addFalseNegatives(report.ByFactType, c.Expected)
			report.Failures = append(report.Failures, Failure{CaseID: c.ID, Reason: "ответ не прошёл производственную проверку", Expected: c.Expected})
			continue
		}
		actual := application.TrustedFacts(result)
		tp, fp, fn, evidenceMatches := compareDetailed(actual, c.Expected, report.ByFactType)
		matchedFacts += tp
		exactEvidence += evidenceMatches
		report.TruePositive += tp
		report.FalsePositive += fp
		report.FalseNegative += fn
		if fp == 0 && fn == 0 {
			report.Exact++
		} else {
			report.Failures = append(report.Failures, Failure{CaseID: c.ID, Reason: "факты не совпали с разметкой", Expected: c.Expected, Actual: actual})
		}
	}
	report.Precision = ratio(report.TruePositive, report.TruePositive+report.FalsePositive)
	report.Recall = ratio(report.TruePositive, report.TruePositive+report.FalseNegative)
	report.F1 = ratio(2*report.TruePositive, 2*report.TruePositive+report.FalsePositive+report.FalseNegative)
	report.ExactRate = ratio(report.Exact, report.Cases)
	report.ValidRate = ratio(report.Cases-report.Invalid, report.Cases)
	report.EvidenceExactRate = ratio(exactEvidence, matchedFacts)
	for factType, counts := range report.ByFactType {
		counts.Precision = ratio(counts.TruePositive, counts.TruePositive+counts.FalsePositive)
		counts.Recall = ratio(counts.TruePositive, counts.TruePositive+counts.FalseNegative)
		counts.F1 = ratio(2*counts.TruePositive, 2*counts.TruePositive+counts.FalsePositive+counts.FalseNegative)
		report.ByFactType[factType] = counts
	}
	factPrecisionPassed := true
	for _, factType := range []domain.FactType{domain.FactBookingIntent, domain.FactBusinessCommitment, domain.FactPriceMentioned, domain.FactFollowUpCandidate} {
		if report.ByFactType[string(factType)].Precision < thresholds.MinimumFactPrecision {
			factPrecisionPassed = false
		}
	}
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
	report.Passed = report.Precision >= thresholds.MinimumPrecision && factPrecisionPassed && report.Recall >= thresholds.MinimumRecall && report.F1 >= thresholds.MinimumF1 && report.ExactRate >= thresholds.MinimumExactRate && report.ValidRate >= thresholds.MinimumValidRate && report.EvidenceExactRate >= thresholds.MinimumEvidenceExactRate && (maxP95 == 0 || p95 <= maxP95)
	return report, nil
}

// Audit проверяет баланс выборок и отсутствие дословно совпадающих переписок.
// Это не заменяет ручную проверку разметки, но не даёт незаметно смешать
// обучающие примеры с независимой контрольной выборкой.
type Audit struct {
	Cases       int            `json:"cases"`
	SplitCounts map[Split]int  `json:"splitCounts"`
	FactCounts  map[string]int `json:"factCounts"`
	EmptyCases  int            `json:"emptyCases"`
}

func AuditCases(cases []Case) (Audit, error) {
	report := Audit{
		Cases:       len(cases),
		SplitCounts: map[Split]int{},
		FactCounts:  map[string]int{},
	}
	seenIDs := make(map[string]struct{}, len(cases))
	seenConversations := make(map[string]string, len(cases))
	for _, c := range cases {
		if _, exists := seenIDs[c.ID]; exists {
			return Audit{}, fmt.Errorf("повтор идентификатора случая %q", c.ID)
		}
		seenIDs[c.ID] = struct{}{}
		fingerprint := conversationFingerprint(c)
		if previousID, exists := seenConversations[fingerprint]; exists {
			return Audit{}, fmt.Errorf("случаи %q и %q содержат одинаковую переписку", previousID, c.ID)
		}
		seenConversations[fingerprint] = c.ID
		report.SplitCounts[c.Split]++
		if len(c.Expected) == 0 {
			report.EmptyCases++
		}
		for _, fact := range c.Expected {
			report.FactCounts[string(fact.Type)]++
		}
	}
	return report, nil
}

func validateCase(c Case) error {
	input := c.Input
	if input.Task != "ANALYZE_CONVERSATION" || input.SchemaVersion != application.AnalysisSchemaV1 || input.PromptVersion != application.CurrentAnalysisPrompt {
		return errors.New("несовместимый входной контракт")
	}
	if input.ConversationID == "" || input.BaseConversationRevision < 1 || len(input.Messages) == 0 || len(input.Messages) > application.MaxContextMessages {
		return errors.New("неверные метаданные переписки")
	}
	messageIDs := make(map[string]struct{}, len(input.Messages))
	for _, message := range input.Messages {
		if message.ID == "" || strings.TrimSpace(message.Body) == "" || (message.Direction != "INCOMING" && message.Direction != "OUTGOING") {
			return errors.New("неверное сообщение")
		}
		if _, exists := messageIDs[message.ID]; exists {
			return fmt.Errorf("повтор идентификатора сообщения %q", message.ID)
		}
		messageIDs[message.ID] = struct{}{}
	}
	if input.AnalysisThroughMessageID != input.Messages[len(input.Messages)-1].ID {
		return errors.New("analysisThroughMessageId не указывает на последнее сообщение")
	}
	factTypes := make(map[domain.FactType]struct{}, len(c.Expected))
	for _, fact := range c.Expected {
		switch fact.Type {
		case domain.FactBookingIntent, domain.FactBusinessCommitment, domain.FactPriceMentioned, domain.FactFollowUpCandidate:
		default:
			return fmt.Errorf("неизвестный ожидаемый тип факта %q", fact.Type)
		}
		if _, exists := factTypes[fact.Type]; exists {
			return fmt.Errorf("повтор ожидаемого типа факта %q", fact.Type)
		}
		factTypes[fact.Type] = struct{}{}
		if !fact.Value || fact.Confidence != 1 || len(fact.EvidenceMessageIDs) == 0 {
			return fmt.Errorf("ожидаемый факт %q должен иметь положительную метку, уверенность 1 и доказательство", fact.Type)
		}
		for _, id := range fact.EvidenceMessageIDs {
			if _, exists := messageIDs[id]; !exists {
				return fmt.Errorf("ожидаемый факт %q ссылается на неизвестное сообщение %q", fact.Type, id)
			}
		}
		if fact.Type == domain.FactPriceMentioned {
			if fact.Amount == nil || normalizeDecimal(*fact.Amount) == "" || len(fact.Currency) != 3 || fact.Currency != strings.ToUpper(fact.Currency) {
				return errors.New("ценовая метка должна содержать десятичную сумму и валюту в верхнем регистре")
			}
		} else if fact.Amount != nil || fact.Currency != "" {
			return fmt.Errorf("неценовой факт %q содержит поля цены", fact.Type)
		}
	}
	return nil
}

func conversationFingerprint(c Case) string {
	var b strings.Builder
	for _, message := range c.Input.Messages {
		b.WriteString(message.Direction)
		b.WriteByte(':')
		b.WriteString(strings.ToLower(strings.Join(strings.Fields(message.Body), " ")))
		b.WriteByte('\n')
	}
	return b.String()
}

func compare(actual, expected []domain.SemanticFact) (tp, fp, fn int) {
	tp, fp, fn, _ = compareDetailed(actual, expected, nil)
	return tp, fp, fn
}

func compareDetailed(actual, expected []domain.SemanticFact, byFactType map[string]FactMetrics) (tp, fp, fn, exactEvidence int) {
	remaining := append([]domain.SemanticFact(nil), expected...)
	for _, a := range actual {
		match := -1
		for i, e := range remaining {
			if sameSemanticFact(a, e) {
				match = i
				break
			}
		}
		if match < 0 {
			fp++
			incrementFact(byFactType, a.Type, 0, 1, 0)
			continue
		}
		tp++
		if sameStrings(a.EvidenceMessageIDs, remaining[match].EvidenceMessageIDs) {
			exactEvidence++
		}
		incrementFact(byFactType, a.Type, 1, 0, 0)
		remaining = append(remaining[:match], remaining[match+1:]...)
	}
	for _, missed := range remaining {
		incrementFact(byFactType, missed.Type, 0, 0, 1)
	}
	return tp, fp, len(remaining), exactEvidence
}

func addFalseNegatives(byFactType map[string]FactMetrics, expected []domain.SemanticFact) {
	for _, fact := range expected {
		incrementFact(byFactType, fact.Type, 0, 0, 1)
	}
}

func incrementFact(byFactType map[string]FactMetrics, factType domain.FactType, tp, fp, fn int) {
	if byFactType == nil {
		return
	}
	current := byFactType[string(factType)]
	current.TruePositive += tp
	current.FalsePositive += fp
	current.FalseNegative += fn
	byFactType[string(factType)] = current
}

func sameSemanticFact(a, b domain.SemanticFact) bool {
	if a.Type != b.Type || a.Value != b.Value || a.Currency != b.Currency || (a.Amount == nil) != (b.Amount == nil) {
		return false
	}
	return a.Amount == nil || normalizeDecimal(*a.Amount) == normalizeDecimal(*b.Amount)
}

func normalizeDecimal(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "+") || strings.HasPrefix(value, "-") || strings.Count(value, ".") > 1 {
		return ""
	}
	parts := strings.SplitN(value, ".", 2)
	if parts[0] == "" {
		return ""
	}
	if _, err := strconv.ParseUint(parts[0], 10, 64); err != nil {
		return ""
	}
	integer := strings.TrimLeft(parts[0], "0")
	if integer == "" {
		integer = "0"
	}
	if len(parts) == 1 {
		return integer
	}
	if parts[1] == "" {
		return ""
	}
	if _, err := strconv.ParseUint(parts[1], 10, 64); err != nil {
		return ""
	}
	fraction := strings.TrimRight(parts[1], "0")
	if fraction == "" {
		return integer
	}
	return integer + "." + fraction
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
