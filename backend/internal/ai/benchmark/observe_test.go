package benchmark

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lidradar/backend/internal/ai/application"
	"lidradar/backend/internal/ai/infrastructure"
)

const llamaAnswer = `{"schemaVersion":"analyze-conversation.v1","analysisThroughMessageId":"message-1","summary":"Клиент просит запись.","facts":[{"type":"BOOKING_INTENT","value":true,"confidence":0.99,"evidenceMessageIds":["message-1"]}]}`

func chatCompletion(content string, promptTokens, completionTokens int, perSecond float64, finish string) string {
	encoded, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"finish_reason": finish, "message": map[string]any{"role": "assistant", "content": content}}},
		"usage":   map[string]any{"prompt_tokens": promptTokens, "completion_tokens": completionTokens, "total_tokens": promptTokens + completionTokens},
		"timings": map[string]any{"prompt_n": promptTokens, "predicted_n": completionTokens, "prompt_per_second": 900.5, "predicted_per_second": perSecond},
	})
	return string(encoded)
}

func TestWithVersionsSubstitutesAfterValidationAndRejectsIncompatiblePairs(t *testing.T) {
	cases, _, err := Load(strings.NewReader(dataset))
	if err != nil {
		t.Fatal(err)
	}
	original := cases[0].Input
	for _, tc := range []struct {
		name, schema, prompt string
		ok                   bool
	}{
		{"historical prompt on the same envelope", "", application.AnalysisPromptV6, true},
		{"current pair", application.AnalysisSchemaV2, application.AnalysisPromptV9, true},
		{"schema only keeps a v1 prompt, so the pair is rejected", application.AnalysisSchemaV2, "", false},
		{"v2 prompt on a v1 envelope", "", application.AnalysisPromptV9, false},
		{"v1 prompt on a v2 envelope", application.AnalysisSchemaV2, application.AnalysisPromptV6, false},
		{"unknown schema", "analyze-conversation.v9", application.AnalysisPromptV9, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := WithVersions(cases, tc.schema, tc.prompt)
			if tc.ok != (err == nil) {
				t.Fatalf("ok=%v, error=%v", tc.ok, err)
			}
			if tc.ok && (got[0].Input.PromptVersion == "" || got[0].Input.SchemaVersion == "") {
				t.Fatalf("versions lost: %+v", got[0].Input)
			}
			if cases[0].Input.PromptVersion != original.PromptVersion || cases[0].Input.SchemaVersion != original.SchemaVersion {
				t.Fatal("the loaded case was modified in place")
			}
		})
	}
}

func TestRunRecordsSchemaVersionsFailureDetailsAndCaseIDs(t *testing.T) {
	cases, digest, err := Load(strings.NewReader(dataset))
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	failing := providerFunc(func(ctx context.Context, _ string) (string, error) {
		seen = append(seen, CaseID(ctx))
		return "", errors.New("llama.cpp вернул состояние 400")
	})
	report, err := Run(context.Background(), failing, cases, digest, Thresholds{})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] != "booking-001" {
		t.Fatalf("case id did not reach the provider: %v", seen)
	}
	if len(report.SchemaVersions) != 1 || report.SchemaVersions[0] != application.AnalysisSchemaV1 {
		t.Fatalf("schema versions: %v", report.SchemaVersions)
	}
	if len(report.Failures) != 1 || report.Failures[0].Detail != "llama.cpp вернул состояние 400" {
		t.Fatalf("failures: %+v", report.Failures)
	}
	invalid := providerFunc(func(context.Context, string) (string, error) {
		return `{"schemaVersion":"analyze-conversation.v1"}`, nil
	})
	report, err = Run(context.Background(), invalid, cases, digest, Thresholds{})
	if err != nil || len(report.Failures) != 1 || report.Failures[0].Reason != "ответ не прошёл производственную проверку" || report.Failures[0].Detail == "" {
		t.Fatalf("validation failure must say why: %+v, error: %v", report.Failures, err)
	}
}

type providerFunc func(context.Context, string) (string, error)

func (f providerFunc) Infer(ctx context.Context, prompt string) (string, error) {
	return f(ctx, prompt)
}

func TestRecorderObservesTheRealProviderWithoutChangingTheAnswer(t *testing.T) {
	status := http.StatusOK
	body := chatCompletion(llamaAnswer, 1830, 74, 51.25, "stop")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()
	cases, digest, err := Load(strings.NewReader(dataset))
	if err != nil {
		t.Fatal(err)
	}
	recorder := NewRecorder()
	provider := recorder.Wrap(infrastructure.LlamaProvider{URL: server.URL + "/v1/chat/completions", Client: &http.Client{Transport: recorder.Transport(nil)}})
	report, err := Run(context.Background(), provider, cases, digest, Thresholds{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Invalid != 0 || report.TruePositive != 1 || report.Exact != 1 {
		t.Fatalf("the recorder must not change what the provider returns: %+v", report)
	}
	observations := recorder.Observations()
	if len(observations) != 1 {
		t.Fatalf("observations: %+v", observations)
	}
	got := observations[0]
	if got.CaseID != "booking-001" || got.Status != 200 || got.PromptTokens != 1830 || got.CompletionTokens != 74 || got.PredictedPerSecond != 51.25 || got.FinishReason != "stop" || got.Output != llamaAnswer {
		t.Fatalf("observation: %+v", got)
	}
	summary := Summarize(observations)
	if summary.Requests != 1 || summary.HTTPStatuses["200"] != 1 || summary.MaxPromptTokens != 1830 || summary.MaxTotalTokens != 1904 || summary.MinTokensPerSecond != 51.25 || summary.LengthFinishes != 0 {
		t.Fatalf("summary: %+v", summary)
	}

	// Ошибка сервера: причина записывается, а поставщик по-прежнему возвращает свою ошибку.
	status, body = http.StatusBadRequest, `{"error":{"code":400,"message":"the request exceeds the available context size","type":"exceed_context_size_error"}}`
	failed := NewRecorder()
	provider = failed.Wrap(infrastructure.LlamaProvider{URL: server.URL + "/v1/chat/completions", Client: &http.Client{Transport: failed.Transport(nil)}})
	report, err = Run(context.Background(), provider, cases, digest, Thresholds{})
	if err != nil || report.Invalid != 1 {
		t.Fatalf("report: %+v, error: %v", report, err)
	}
	ExplainFailures(report.Failures, failed.Observations())
	if len(report.Failures) != 1 || !strings.Contains(report.Failures[0].Detail, "состояние 400") || !strings.Contains(report.Failures[0].Detail, "the request exceeds the available context size") {
		t.Fatalf("the server's reason must reach the failure: %+v", report.Failures)
	}
	got = failed.Observations()[0]
	if got.Status != 400 || got.Error != "the request exceeds the available context size" || got.ProviderError == "" {
		t.Fatalf("failed observation: %+v", got)
	}
	if Summarize(failed.Observations()).HTTPStatuses["400"] != 1 {
		t.Fatal("status must be counted")
	}
}

func TestRecorderCountsTransportErrorsAndBeforeCallFailures(t *testing.T) {
	recorder := NewRecorder()
	client := &http.Client{Transport: recorder.Transport(roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	}))}
	request, _ := http.NewRequestWithContext(WithCaseID(context.Background(), "case-1"), http.MethodPost, "http://node.invalid/v1/chat/completions", strings.NewReader("{}"))
	if _, err := client.Do(request); err == nil {
		t.Fatal("transport error must reach the caller")
	}
	wrapped := recorder.Wrap(providerFunc(func(context.Context, string) (string, error) {
		return "", errors.New("схема генерации не собрана")
	}))
	if _, err := wrapped.Infer(WithCaseID(context.Background(), "case-2"), "{}"); err == nil {
		t.Fatal("provider error must reach the caller")
	}
	summary := Summarize(recorder.Observations())
	if summary.Requests != 2 || summary.TransportErrors != 2 || len(summary.HTTPStatuses) != 0 {
		t.Fatalf("summary: %+v", summary)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSummarizeFindsTheLongestPromptAndTheSlowestGeneration(t *testing.T) {
	summary := Summarize([]Observation{
		{CaseID: "a", Status: 200, PromptTokens: 900, CompletionTokens: 60, PredictedPerSecond: 48, FinishReason: "stop"},
		{CaseID: "b", Status: 200, PromptTokens: 3453, CompletionTokens: 640, PredictedPerSecond: 31.5, FinishReason: "length"},
		{CaseID: "c", Status: 200, PromptTokens: 1200, CompletionTokens: 80, PredictedPerSecond: 52, FinishReason: "stop"},
		{CaseID: "d", Status: 400},
	})
	if summary.Requests != 4 || summary.ResponsesWithTimings != 3 || summary.HTTPStatuses["200"] != 3 || summary.HTTPStatuses["400"] != 1 {
		t.Fatalf("counts: %+v", summary)
	}
	if summary.MaxPromptTokens != 3453 || summary.MaxPromptTokensCase != "b" || summary.MaxTotalTokens != 4093 || summary.MaxCompletionTokens != 640 {
		t.Fatalf("tokens: %+v", summary)
	}
	if summary.MinTokensPerSecond != 31.5 || summary.MinTokensPerSecondCase != "b" || summary.MedianTokensPerSecond != 48 || summary.LengthFinishes != 1 {
		t.Fatalf("speed: %+v", summary)
	}
}

func TestCaptureServerReadsBuildModelAndObservedSampling(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/props", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"default_generation_settings":{"n_ctx":4096},"total_slots":1,"model_path":"/models/Qwen3-8B-Q4_K_M.gguf","model_ftype":"Q4_K - Medium","build_info":"b10666-4e97ac86e","chat_template":"..."}`)
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"id":"/models/Qwen3-8B-Q4_K_M.gguf","meta":{"n_params":8190735360,"size":5021827072,"n_ctx":4096}}]}`)
	})
	mux.HandleFunc("/slots", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[{"id":0,"params":{"seed":42,"temperature":0.20000000298023224,"top_k":20,"top_p":0.800000011920929,"min_p":0.0,"presence_penalty":0.0,"generation_prompt":"<|im_start|>assistant\n<think>\n\n</think>\n\n"}}]`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	endpoint := server.URL + "/v1/chat/completions"
	info := CaptureServer(context.Background(), server.Client(), endpoint)
	info.ObserveSampling(context.Background(), server.Client(), endpoint)
	if info.BuildInfo != "b10666-4e97ac86e" || info.ModelPath != "/models/Qwen3-8B-Q4_K_M.gguf" || info.ModelFtype != "Q4_K - Medium" || info.ContextSize != 4096 || info.TotalSlots != 1 || info.ModelSizeBytes != 5021827072 || info.ModelParameters != 8190735360 {
		t.Fatalf("server: %+v", info)
	}
	want := Sampling{Seed: 42, Temperature: .2, TopP: .8, TopK: 20, MinP: 0, PresencePenalty: 0, ThinkingDisabled: true}
	if info.ObservedSampling == nil || *info.ObservedSampling != want || len(info.Errors) != 0 {
		t.Fatalf("sampling: %+v, errors: %v", info.ObservedSampling, info.Errors)
	}
}

func TestCaptureServerTreatsAnUnreachableNodeAsAnIncompleteBindingNotAFailure(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	endpoint := server.URL + "/v1/chat/completions"
	address := strings.TrimPrefix(server.URL, "http://")
	server.Close() // дальше адрес никто не слушает
	info := CaptureServer(context.Background(), http.DefaultClient, endpoint)
	info.ObserveSampling(context.Background(), http.DefaultClient, endpoint)
	if len(info.Errors) != 3 || info.BuildInfo != "" || info.ObservedSampling != nil {
		t.Fatalf("server: %+v", info)
	}
	for _, message := range info.Errors {
		if strings.Contains(message, address) || !strings.Contains(message, "connection refused") {
			t.Fatalf("the node address must not enter the report, the reason must stay: %q", message)
		}
	}
}

func TestServerBaseStripsTheChatRoute(t *testing.T) {
	for in, want := range map[string]string{
		"http://127.0.0.1:18089/v1/chat/completions":    "http://127.0.0.1:18089",
		"http://llama-server:8080/v1/chat/completions/": "http://llama-server:8080",
		"http://127.0.0.1:8080":                         "http://127.0.0.1:8080",
	} {
		if got := ServerBase(in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}

func manyCases(t *testing.T, count int) []Case {
	t.Helper()
	cases, _, err := Load(strings.NewReader(dataset))
	if err != nil {
		t.Fatal(err)
	}
	result := make([]Case, count)
	for i := range result {
		result[i] = cases[0]
		result[i].ID = "booking-" + string(rune('a'+i%26)) + string(rune('a'+i/26))
	}
	return result
}

func TestRunAbortsWithoutAReportWhenTheModelStopsAnswering(t *testing.T) {
	calls := 0
	down := providerFunc(func(context.Context, string) (string, error) {
		calls++
		return "", errors.New("connection refused")
	})
	report, err := Run(context.Background(), down, manyCases(t, 12), "digest", Thresholds{})
	if !errors.Is(err, ErrModelUnavailable) || calls != maxConsecutiveCallFailures {
		t.Fatalf("the run must stop after %d failures in a row: calls=%d, error=%v", maxConsecutiveCallFailures, calls, err)
	}
	if !strings.Contains(err.Error(), "connection refused") || report.Cases != 12 {
		t.Fatalf("the error must say why: %v", err)
	}
}

func TestRunKeepsGoingThroughIsolatedFailures(t *testing.T) {
	calls := 0
	flaky := providerFunc(func(context.Context, string) (string, error) {
		calls++
		if calls%3 != 0 { // две ошибки, затем успешный ответ: подряд ошибок меньше предела
			return "", errors.New("llama.cpp вернул состояние 503")
		}
		return llamaAnswer, nil
	})
	report, err := Run(context.Background(), flaky, manyCases(t, 12), "digest", Thresholds{})
	if err != nil || report.Invalid != 8 || calls != 12 {
		t.Fatalf("isolated failures are scored, not fatal: invalid=%d calls=%d error=%v", report.Invalid, calls, err)
	}
	// Ответ, не прошедший проверку, — тоже ответ модели: он сбрасывает счётчик отказов вызова.
	garbled := providerFunc(func(context.Context, string) (string, error) {
		return `{"schemaVersion":"analyze-conversation.v1"}`, nil
	})
	if _, err := Run(context.Background(), garbled, manyCases(t, 12), "digest", Thresholds{}); err != nil {
		t.Fatalf("invalid answers are results, not outages: %v", err)
	}
}
