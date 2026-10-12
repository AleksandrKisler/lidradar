package infrastructure

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"strings"

	"lidradar/backend/internal/ai/application"
)

// Проход по обещаниям (ADR 0055). Основной вызов v9 решает многое сразу (резюме, факты, договорённости), а допуск к
// обещанию в нём ограничен словарём: на независимой выборке словарь узнавал 69% обещаний, и из узнанных модель
// отмечала 84% (полнота 0,58). Отдельный короткий вызов просит у модели по каждому исходящему сообщению переписки одну
// категорию (promise, done, status, request, offer, other, см. promise_scan_examples.go) и расширяет допуск: сообщение без слов
// словаря, которое проход назвал обещанием, становится допустимым основанием факта и договорённости наравне со словарным.
//
// Решает по-прежнему основной вызов: он видит весь разговор, его инструкция требует молчать при сомнении, а
// ответ ограничен грамматикой допущенных сообщений. Проход сам ничего не записывает, кроме спасения (promise_rescue.go): договорённость
// на обещание, которое основной вызов пропустил, добавляется только при очень высокой вероятности promise (для словарных обещаний не ниже
// 0,9, для остальных не ниже 0,99), потому что ложная договорённость в режиме v2 не исчезает без явного выполнения и держит риск
// PROMISE_NOT_FULFILLED открытым.
//
// Границы доверия:
//   - ответ прохода ограничен схемой: по одной категории из шести на каждое проверяемое сообщение, ничего другого;
//   - проверяются только исходящие сообщения, про которые структура текста не говорит «обещания нет»
//     (application.CommitmentRuledOut: предложение, условие, отрицание, смягчение, вопрос);
//   - выполненное действие («Готово, запись перенесена») проход называет done, а не promise; грубого фильтра по подстрокам
//     («отправил», «уже») больше нет: он закрывал и настоящие обещания («Заявку передал инженерам, с вами свяжется техник»);
//   - любая неудача прохода (сеть, формат, контекст) оставляет прежний допуск по словарю: проход только повышает полноту;
//   - ответ основного вызова проходит ту же серверную проверку, что и раньше (ValidateAnalysisResultV2,
//     ValidateAgreementEvidence); для инструкции v9 она принимает основанием исходящее сообщение, которое не исключено структурой.
const (
	// promiseScanEligibleThreshold — вероятность категории promise, с которой сообщение без слов словаря становится допустимым.
	// Вероятность считается по токену значения среди шести категорий; оценки модели не откалиброваны и держатся у краёв: на наборах
	// настройки настоящие обещания вне словаря получали от 0,84, а ближайшие не обещания не выше 0,5. Порог оставляет запас.
	promiseScanEligibleThreshold = 0.95
	// promiseScanTopLogprobs — сколько альтернатив токена просить у сервера, чтобы получить вероятность каждой категории.
	promiseScanTopLogprobs = 10
)

// scanEligibility — сообщения без слов словаря, которые проход допустил к решению модели; ключ — идентификатор сообщения в
// запросе (у основного вызова это псевдоним m1…).
type scanEligibility map[string]bool

// allows: сообщение допущено словарём либо проходом.
func (e scanEligibility) allows(m application.ContextMessage) bool {
	return application.PossibleBusinessCommitmentEvidence(m) || (m.Direction == "OUTGOING" && e[m.ID])
}

// canonical переводит допуск из псевдонимов основного вызова в идентификаторы сообщений запроса.
func (e scanEligibility) canonical(aliases analysisAliases) scanEligibility {
	if len(e) == 0 {
		return nil
	}
	result := make(scanEligibility, len(e))
	for alias, allowed := range e {
		if id, ok := aliases.canonical[alias]; ok && allowed {
			result[id] = true
		}
	}
	return result
}

// admitsBeyondLexicon: среди сообщений есть исходящее без слов словаря, которое допустил проход.
func (e scanEligibility) admitsBeyondLexicon(messages []application.ContextMessage) bool {
	for _, m := range messages {
		if m.Direction == "OUTGOING" && e[m.ID] && !application.PossibleBusinessCommitmentEvidence(m) {
			return true
		}
	}
	return false
}

type scanEligibilityKey struct{}

// withScanEligibility передаёт допуск прохода основному вызову через контекст: сигнатуры публичных методов поставщика не меняются.
func withScanEligibility(ctx context.Context, e scanEligibility) context.Context {
	if len(e) == 0 {
		return ctx
	}
	return context.WithValue(ctx, scanEligibilityKey{}, e)
}

func scanEligibilityFrom(ctx context.Context) scanEligibility {
	e, _ := ctx.Value(scanEligibilityKey{}).(scanEligibility)
	return e
}

// scanMessage — сообщение в запросе прохода; поля идут в этом порядке, чтобы запрос не зависел от порядка ключей карты.
type scanMessage struct {
	ID        string `json:"id"`
	Direction string `json:"direction"`
	Body      string `json:"body"`
}

type scanConversation struct {
	Messages []scanMessage `json:"messages"`
}

// promiseScanSchema — объект с обязательной категорией на каждое проверяемое сообщение, в порядке переписки.
func promiseScanSchema(ids []string) json.RawMessage {
	var out strings.Builder
	required, _ := json.Marshal(ids)
	enum, _ := json.Marshal(scanClasses)
	fmt.Fprintf(&out, `{"type":"object","additionalProperties":false,"required":%s,"properties":{`, required)
	for i, id := range ids {
		if i > 0 {
			out.WriteByte(',')
		}
		key, _ := json.Marshal(id)
		fmt.Fprintf(&out, `%s:{"type":"string","enum":%s}`, key, enum)
	}
	out.WriteString("}}")
	return json.RawMessage(out.String())
}

// promiseEligibility запрашивает проход по псевдонимизированному окну основного вызова и возвращает допуск: сообщения без слов
// словаря, которые модель сочла вероятными обещаниями. Любая неудача возвращает пустой допуск.
func (p LlamaProvider) promiseEligibility(ctx context.Context, aliasedPrompt string) scanEligibility {
	return eligibilityFrom(aliasedPrompt, p.promiseProbabilities(ctx, aliasedPrompt))
}

// eligibilityFrom строит допуск по вероятностям прохода: сообщения без слов словаря, которые модель сочла вероятными обещаниями.
func eligibilityFrom(aliasedPrompt string, probabilities map[string]float64) scanEligibility {
	var request application.AnalyzeConversationRequestV1
	if json.Unmarshal([]byte(aliasedPrompt), &request) != nil {
		return nil
	}
	eligible := scanEligibility{}
	for _, m := range request.Messages {
		// Про сообщение, которое не спрашивали, вероятность нулевая и порог его не пропустит.
		if application.CommitmentSupport(m.Body) != application.LexicalOpen || probabilities[m.ID] < promiseScanEligibleThreshold {
			continue
		}
		eligible[m.ID] = true
	}
	return eligible
}

// PromiseProbabilities запускает только проход по обещаниям для запроса v9 и возвращает вероятность «обещание» по идентификатору
// сообщения запроса, ту самую, по которой поставщик допускает и спасает сообщения (с подтверждением вторым вариантом). Нужен инструментам
// настройки (cmd/ai-scan-probe): рабочий путь вызывает проход изнутри Infer.
func (p LlamaProvider) PromiseProbabilities(ctx context.Context, prompt string) (map[string]float64, error) {
	aliased, aliases, err := aliasAnalysisRequest(prompt)
	if err != nil {
		return nil, err
	}
	return aliases.canonicalProbabilities(p.agreedProbabilities(ctx, aliased, p.promiseProbabilities(ctx, aliased))), nil
}

// promiseProbabilities спрашивает модель про каждое исходящее сообщение-кандидат (не исключённое структурой) и возвращает
// вероятность ответа «обещание» по псевдониму сообщения. Если запрос не умещается в контекст, он повторяется по более короткому
// окну, как у основного вызова. Нет кандидатов или любая неудача — nil.
func (p LlamaProvider) promiseProbabilities(ctx context.Context, aliasedPrompt string) map[string]float64 {
	return p.scanWithRetry(ctx, aliasedPrompt, promiseScanExampleTurns, nil)
}

// agreedProbabilities требует согласия двух вариантов прохода. Ответ прохода на пограничные сообщения неустойчив: небольшая правка или
// другой порядок примеров переворачивает его, и ложная отметка с вероятностью 0,99 встречается у любого одного варианта (на наборах
// настройки у отдельного варианта 3–8 ложных отметок на 466 сообщений вне словаря). Настоящее обещание отмечают оба варианта, а ложную
// отметку чаще всего только один. Поэтому сообщение вне словаря, которое первый вариант назвал обещанием, спрашивают у второго (те же
// примеры в обратном порядке); берётся меньшая вероятность. Второй запрос идёт только когда есть такие сообщения и спрашивает только о них;
// при его неудаче они остаются без подтверждения (вероятность 0), то есть допуск возвращается к словарю. Сообщения со словами словаря
// остаются с вероятностью первого варианта.
func (p LlamaProvider) agreedProbabilities(ctx context.Context, aliasedPrompt string, first map[string]float64) map[string]float64 {
	var request application.AnalyzeConversationRequestV1
	if len(first) == 0 || json.Unmarshal([]byte(aliasedPrompt), &request) != nil {
		return first
	}
	flagged := map[string]bool{}
	for _, m := range request.Messages {
		if application.CommitmentSupport(m.Body) == application.LexicalOpen && first[m.ID] >= promiseScanEligibleThreshold {
			flagged[m.ID] = true
		}
	}
	if len(flagged) == 0 {
		return first
	}
	second := p.scanWithRetry(ctx, aliasedPrompt, promiseScanConfirmTurns, flagged)
	result := make(map[string]float64, len(first))
	for id, probability := range first {
		result[id] = probability
	}
	for id := range flagged {
		result[id] = math.Min(first[id], second[id])
	}
	return result
}

// scanWithRetry выполняет проход с заданными примерами; only (если не nil) оставляет среди кандидатов только перечисленные сообщения.
func (p LlamaProvider) scanWithRetry(ctx context.Context, aliasedPrompt string, examples []map[string]string, only map[string]bool) map[string]float64 {
	for attempt := 0; ; attempt++ {
		var candidates []string
		for _, id := range scanCandidates(aliasedPrompt) {
			if only == nil || only[id] {
				candidates = append(candidates, id)
			}
		}
		if len(candidates) == 0 {
			return nil
		}
		probabilities, err := p.scanRequest(ctx, aliasedPrompt, candidates, examples)
		var exceeded contextExceededError
		if err == nil {
			return probabilities
		}
		if !errors.As(err, &exceeded) || attempt == maxContextRetries {
			return nil
		}
		shrunk, ok := shrinkToContext(aliasedPrompt, exceeded)
		if !ok {
			return nil
		}
		aliasedPrompt = shrunk
	}
}

// scanCandidates — исходящие сообщения окна до границы анализа, про которые структура текста не говорит «обещания нет».
func scanCandidates(aliasedPrompt string) []string {
	var request application.AnalyzeConversationRequestV1
	if json.Unmarshal([]byte(aliasedPrompt), &request) != nil {
		return nil
	}
	var candidates []string
	for _, m := range request.Messages {
		if application.ScannedCommitmentEvidence(m) {
			candidates = append(candidates, m.ID)
		}
		if m.ID == request.AnalysisThroughMessageID {
			break
		}
	}
	return candidates
}

// scanRequest выполняет один запрос прохода по уже псевдонимизированному окну (m1…mN) и разбирает вероятности по кандидатам.
func (p LlamaProvider) scanRequest(ctx context.Context, aliasedPrompt string, candidates []string, examples []map[string]string) (map[string]float64, error) {
	if p.URL == "" {
		return nil, errors.New("адрес llama.cpp обязателен")
	}
	var request application.AnalyzeConversationRequestV1
	if err := json.Unmarshal([]byte(aliasedPrompt), &request); err != nil {
		return nil, err
	}
	var conversation scanConversation
	for _, m := range request.Messages {
		body := m.Body
		if application.ScannedCommitmentEvidence(m) {
			// Модель видит только предложения, в которых обещание возможно: вопрос «Прислать вам фото?» или «могу прислать» рядом
			// с обычным ответом обещанием не бывает и вводил бы в заблуждение.
			body = application.CommitmentOpenText(body)
		}
		conversation.Messages = append(conversation.Messages, scanMessage{ID: m.ID, Direction: m.Direction, Body: body})
		if m.ID == request.AnalysisThroughMessageID {
			break
		}
	}
	userContent, err := json.Marshal(conversation)
	if err != nil {
		return nil, err
	}
	messages := []map[string]string{{"role": "system", "content": promiseScanSystemPrompt}}
	messages = append(messages, examples...)
	messages = append(messages, map[string]string{"role": "user", "content": string(userContent)})
	body, err := json.Marshal(map[string]any{
		"model":                p.Model,
		"messages":             messages,
		"temperature":          0,
		"seed":                 42,
		"reasoning_effort":     "none",
		"chat_template_kwargs": map[string]bool{"enable_thinking": false},
		"response_format":      map[string]any{"type": "json_object", "schema": promiseScanSchema(candidates)},
		"logprobs":             true,
		"top_logprobs":         promiseScanTopLogprobs,
		"max_tokens":           16 + 12*len(candidates),
	})
	if err != nil {
		return nil, err
	}
	client := p.Client
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_, _ = io.Copy(io.Discard, resp.Body)
		if exceeded, ok := parseContextExceeded(data); ok && resp.StatusCode == http.StatusBadRequest {
			return nil, exceeded
		}
		return nil, fmt.Errorf("llama.cpp вернул состояние %d", resp.StatusCode)
	}
	var payload scanResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return nil, err
	}
	if len(payload.Choices) != 1 || payload.Choices[0].FinishReason == "length" {
		return nil, errors.New("проход по обещаниям вернул неполный ответ")
	}
	return scanProbabilities(payload.Choices[0], candidates)
}

type scanResponse struct {
	Choices []scanChoice `json:"choices"`
}

type scanChoice struct {
	FinishReason string `json:"finish_reason"`
	Message      struct {
		Content string `json:"content"`
	} `json:"message"`
	Logprobs *struct {
		Content []scanToken `json:"content"`
	} `json:"logprobs"`
}

type scanToken struct {
	Token       string `json:"token"`
	Bytes       []int  `json:"bytes"`
	TopLogprobs []struct {
		Token   string  `json:"token"`
		Logprob float64 `json:"logprob"`
	} `json:"top_logprobs"`
}

// scanProbabilities разбирает ответ: категорию по каждому ключу и, если сервер вернул вероятности токенов, вероятность
// «promise» как долю между категориями на месте значения. Без вероятностей решением служит сам ответ (1 или 0).
func scanProbabilities(choice scanChoice, ids []string) (map[string]float64, error) {
	var decided map[string]string
	if err := json.NewDecoder(strings.NewReader(choice.Message.Content)).Decode(&decided); err != nil || len(decided) != len(ids) {
		return nil, errors.New("проход по обещаниям вернул неверный ответ")
	}
	result := make(map[string]float64, len(ids))
	for _, id := range ids {
		value, ok := decided[id]
		if !ok {
			return nil, errors.New("проход по обещаниям пропустил сообщение")
		}
		if !slices.Contains(scanClasses, value) {
			return nil, errors.New("проход по обещаниям назвал неизвестную категорию")
		}
		if value == scanPromise {
			result[id] = 1
		} else {
			result[id] = 0
		}
	}
	if choice.Logprobs == nil {
		return result, nil
	}
	content := choice.Message.Content
	starts := make([]int, len(choice.Logprobs.Content))
	offset := 0
	for i, token := range choice.Logprobs.Content {
		starts[i] = offset
		if token.Bytes != nil {
			offset += len(token.Bytes)
		} else {
			offset += len(token.Token)
		}
	}
	if offset != len(content) {
		return result, nil
	}
	for _, id := range ids {
		key, _ := json.Marshal(id)
		at := strings.Index(content, string(key)+":")
		if at < 0 {
			continue
		}
		valueAt := at + len(key) + 1
		for valueAt < len(content) && (content[valueAt] == ' ' || content[valueAt] == '"') {
			valueAt++
		}
		for i, token := range choice.Logprobs.Content {
			end := len(content)
			if i+1 < len(starts) {
				end = starts[i+1]
			}
			if valueAt < starts[i] || valueAt >= end {
				continue
			}
			var promise, total float64
			for _, alternative := range token.TopLogprobs {
				text := strings.TrimSpace(alternative.Token)
				if text == "" {
					continue
				}
				matches := 0
				for _, class := range scanClasses {
					if strings.HasPrefix(class, text) {
						matches++
					}
				}
				if matches != 1 {
					continue
				}
				weight := math.Exp(alternative.Logprob)
				total += weight
				if strings.HasPrefix(scanPromise, text) {
					promise += weight
				}
			}
			if total > 0 {
				result[id] = promise / total
			}
			break
		}
	}
	return result, nil
}
