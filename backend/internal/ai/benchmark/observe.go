package benchmark

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	neturl "net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// maxObservedBody ограничивает чтение ответа модели: структурированный ответ
// анализа занимает единицы килобайт, больший ответ — уже аномалия.
const maxObservedBody = 4 << 20

// Observation — что известно об одном обращении к модели из HTTP-ответа
// llama.cpp. Измерения нужны для отчёта об оборудовании (длина контекста,
// скорость генерации) и для разбора несовпавших случаев; на оценку качества они
// не влияют.
type Observation struct {
	CaseID             string  `json:"caseId"`
	Status             int     `json:"status"`
	LatencyMS          int64   `json:"latencyMs"`
	PromptTokens       int     `json:"promptTokens,omitempty"`
	CompletionTokens   int     `json:"completionTokens,omitempty"`
	PromptPerSecond    float64 `json:"promptPerSecond,omitempty"`
	PredictedPerSecond float64 `json:"predictedPerSecond,omitempty"`
	FinishReason       string  `json:"finishReason,omitempty"`
	// Error — текст ошибки сервера или транспорта; ProviderError — итоговая ошибка
	// поставщика, например отказ разбирать структурированный ответ.
	Error         string `json:"error,omitempty"`
	ProviderError string `json:"providerError,omitempty"`
	// Output — ответ модели до восстановления псевдонимов сообщений.
	Output string `json:"output,omitempty"`
}

// Recorder собирает измерения обращений к модели. Он не меняет ни запросы, ни
// ответы: транспорт возвращает вызывающему байт в байт то, что прочитал.
type Recorder struct {
	mu           sync.Mutex
	observations []Observation
}

func NewRecorder() *Recorder { return &Recorder{} }

// Observations возвращает копию записанных измерений в порядке обращений.
func (r *Recorder) Observations() []Observation {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Observation(nil), r.observations...)
}

// Transport оборачивает транспорт HTTP-клиента поставщика. Идентификатор случая
// берётся из контекста запроса (WithCaseID).
func (r *Recorder) Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return recordingTransport{recorder: r, base: base}
}

// Wrap дополняет последнее измерение случая итоговой ошибкой поставщика.
func (r *Recorder) Wrap(provider Provider) Provider {
	return recordingProvider{recorder: r, inner: provider}
}

type recordingProvider struct {
	recorder *Recorder
	inner    Provider
}

func (p recordingProvider) Infer(ctx context.Context, prompt string) (string, error) {
	output, err := p.inner.Infer(ctx, prompt)
	if err != nil {
		p.recorder.annotateLast(CaseID(ctx), Describe(err))
	}
	return output, err
}

func (r *Recorder) annotateLast(caseID, providerError string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.observations) - 1; i >= 0; i-- {
		if r.observations[i].CaseID == caseID {
			r.observations[i].ProviderError = providerError
			return
		}
	}
	// Ошибка возникла до обращения к модели (например, не собрана схема генерации).
	r.observations = append(r.observations, Observation{CaseID: caseID, ProviderError: providerError})
}

type recordingTransport struct {
	recorder *Recorder
	base     http.RoundTripper
}

func (t recordingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	started := time.Now()
	observation := Observation{CaseID: CaseID(request.Context())}
	response, err := t.base.RoundTrip(request)
	observation.LatencyMS = time.Since(started).Milliseconds()
	if err != nil {
		observation.Error = Describe(err)
		t.recorder.add(observation)
		return nil, err
	}
	observation.Status = response.StatusCode
	data, readErr := io.ReadAll(io.LimitReader(response.Body, maxObservedBody))
	_ = response.Body.Close()
	if readErr != nil {
		observation.Error = Describe(readErr)
	}
	parseResponse(data, &observation)
	observation.LatencyMS = time.Since(started).Milliseconds()
	t.recorder.add(observation)
	if readErr != nil {
		return nil, readErr
	}
	response.Body = io.NopCloser(bytes.NewReader(data))
	response.ContentLength = int64(len(data))
	return response, nil
}

func (r *Recorder) add(observation Observation) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.observations = append(r.observations, observation)
}

func parseResponse(data []byte, observation *Observation) {
	var payload struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
		Timings struct {
			PromptN            int     `json:"prompt_n"`
			PredictedN         int     `json:"predicted_n"`
			PromptPerSecond    float64 `json:"prompt_per_second"`
			PredictedPerSecond float64 `json:"predicted_per_second"`
		} `json:"timings"`
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(data, &payload) != nil {
		if observation.Error == "" && observation.Status/100 != 2 {
			observation.Error = truncate(strings.TrimSpace(string(data)), 300)
		}
		return
	}
	if len(payload.Error) > 0 && string(payload.Error) != "null" {
		var detail struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(payload.Error, &detail) == nil && detail.Message != "" {
			observation.Error = truncate(detail.Message, 300)
		} else {
			observation.Error = truncate(string(payload.Error), 300)
		}
	}
	observation.PromptTokens = payload.Usage.PromptTokens
	observation.CompletionTokens = payload.Usage.CompletionTokens
	observation.PromptPerSecond = payload.Timings.PromptPerSecond
	if payload.Timings.PredictedN > 0 {
		observation.PredictedPerSecond = payload.Timings.PredictedPerSecond
	}
	if len(payload.Choices) > 0 {
		observation.FinishReason = payload.Choices[0].FinishReason
		observation.Output = payload.Choices[0].Message.Content
	}
}

// Describe возвращает текст ошибки без адреса узла: отчёты проверки попадают в
// репозиторий, а адрес туннеля или узла в них не нужен.
func Describe(err error) string {
	var urlError *neturl.Error
	if errors.As(err, &urlError) && urlError.Err != nil {
		err = urlError.Err
	}
	var opError *net.OpError
	if errors.As(err, &opError) && opError.Err != nil {
		return opError.Op + ": " + opError.Err.Error()
	}
	var dnsError *net.DNSError
	if errors.As(err, &dnsError) {
		return "lookup: " + dnsError.Err
	}
	return err.Error()
}

func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}

// ExplainFailures дополняет отказ вызова модели причиной, которую назвал сервер
// (например, «the request exceeds the available context size»): поставщик сообщает
// только код состояния.
func ExplainFailures(failures []Failure, observations []Observation) {
	reasons := map[string]string{}
	for _, o := range observations {
		if o.Error != "" && o.Status/100 != 2 {
			reasons[o.CaseID] = o.Error
		}
	}
	for i, f := range failures {
		if reason, ok := reasons[f.CaseID]; ok && f.Reason == "ошибка вызова модели" && !strings.Contains(f.Detail, reason) {
			failures[i].Detail = strings.TrimSpace(f.Detail + ": " + reason)
		}
	}
}

// Performance — сводка измерений для отчёта об оборудовании. Видеопамять и
// аварийные остановки по HTTP не видны: их снимают на узле.
type Performance struct {
	// StartedAt и FinishedAt (UTC, секунды) нужны, чтобы убедиться: снимки видеопамяти
	// на узле покрывают именно этот прогон.
	StartedAt              string         `json:"startedAt,omitempty"`
	FinishedAt             string         `json:"finishedAt,omitempty"`
	Requests               int            `json:"requests"`
	ResponsesWithTimings   int            `json:"responsesWithTimings"`
	HTTPStatuses           map[string]int `json:"httpStatuses"`
	TransportErrors        int            `json:"transportErrors"`
	LengthFinishes         int            `json:"lengthFinishes"`
	MaxPromptTokens        int            `json:"maxPromptTokens"`
	MaxPromptTokensCase    string         `json:"maxPromptTokensCase,omitempty"`
	MaxCompletionTokens    int            `json:"maxCompletionTokens"`
	MaxTotalTokens         int            `json:"maxTotalTokens"`
	MinTokensPerSecond     float64        `json:"minTokensPerSecond"`
	MinTokensPerSecondCase string         `json:"minTokensPerSecondCase,omitempty"`
	MedianTokensPerSecond  float64        `json:"medianTokensPerSecond"`
}

// Summarize сводит измерения: самый длинный запрос, самая медленная генерация,
// ответы, оборванные по длине, и коды состояния.
func Summarize(observations []Observation) Performance {
	summary := Performance{Requests: len(observations), HTTPStatuses: map[string]int{}}
	var rates []float64
	for _, o := range observations {
		if o.Status == 0 {
			summary.TransportErrors++
		} else {
			summary.HTTPStatuses[strconv.Itoa(o.Status)]++
		}
		if o.FinishReason == "length" {
			summary.LengthFinishes++
		}
		if o.PromptTokens > summary.MaxPromptTokens {
			summary.MaxPromptTokens, summary.MaxPromptTokensCase = o.PromptTokens, o.CaseID
		}
		if o.CompletionTokens > summary.MaxCompletionTokens {
			summary.MaxCompletionTokens = o.CompletionTokens
		}
		if total := o.PromptTokens + o.CompletionTokens; total > summary.MaxTotalTokens {
			summary.MaxTotalTokens = total
		}
		if o.PredictedPerSecond > 0 {
			summary.ResponsesWithTimings++
			rates = append(rates, o.PredictedPerSecond)
			if summary.MinTokensPerSecond == 0 || o.PredictedPerSecond < summary.MinTokensPerSecond {
				summary.MinTokensPerSecond, summary.MinTokensPerSecondCase = o.PredictedPerSecond, o.CaseID
			}
		}
	}
	if len(rates) > 0 {
		sort.Float64s(rates)
		summary.MedianTokensPerSecond = rates[len(rates)/2]
	}
	return summary
}
