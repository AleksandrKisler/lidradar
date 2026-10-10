package infrastructure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"lidradar/backend/internal/ai/application"
)

// Бюджет продукта задан в рунах (20 сообщений, 12 000 рун), а контекст сервера — в токенах, и
// число токенов на руну зависит от текста: эмодзи, цифры и латиница занимают заметно больше
// кириллической прозы. Если запрос всё же не умещается, сервер отвечает 400
// exceed_context_size_error. Вместо мёртвого задания поставщик v9 повторяет запрос по более
// короткому окну: самые старые сообщения отбрасываются, последнее (граница анализа) остаётся, а
// доказательства ответа остаются внутри исходного снимка, поэтому серверная проверка не меняется.
const (
	// maxContextRetries ограничивает число повторов: каждый отказ приходит до генерации и дёшев,
	// но бесконечного сужения быть не должно.
	maxContextRetries = 4
	// answerReserveTokens — запас под ответ (до 250–500 токенов по замерам) при расчёте, сколько вырезать.
	answerReserveTokens = 600
	// tokensPerRune — осторожная оценка веса руны переписки (по замерам 0,28–0,40): лучше вырезать
	// чуть больше, чем получить второй отказ.
	tokensPerRune = 0.45
	// tokensPerMessage — служебные символы JSON сообщения (по замерам около 13 токенов).
	tokensPerMessage = 14
)

// contextExceededError — отказ сервера «запрос не умещается в контекст» с числами из его ответа.
type contextExceededError struct{ promptTokens, contextTokens int }

func (e contextExceededError) Error() string {
	return fmt.Sprintf("llama.cpp вернул состояние 400: запрос (%d токенов) не умещается в контекст %d", e.promptTokens, e.contextTokens)
}

// parseContextExceeded разбирает тело ответа 400; для любой другой ошибки возвращает false.
func parseContextExceeded(body []byte) (contextExceededError, bool) {
	var payload struct {
		Error struct {
			Type         string `json:"type"`
			PromptTokens int    `json:"n_prompt_tokens"`
			ContextSize  int    `json:"n_ctx"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &payload) != nil || payload.Error.Type != "exceed_context_size_error" || payload.Error.PromptTokens <= 0 || payload.Error.ContextSize <= 0 {
		return contextExceededError{}, false
	}
	return contextExceededError{promptTokens: payload.Error.PromptTokens, contextTokens: payload.Error.ContextSize}, true
}

// shrinkToContext отбрасывает самые старые сообщения так, чтобы по оценке убрать превышение
// вместе с запасом под ответ. Последнее сообщение никогда не отбрасывается; false — сужать нечего.
func shrinkToContext(prompt string, exceeded contextExceededError) (string, bool) {
	var request application.AnalyzeConversationRequestV1
	if json.Unmarshal([]byte(prompt), &request) != nil || len(request.Messages) < 2 {
		return "", false
	}
	excess := float64(exceeded.promptTokens - (exceeded.contextTokens - answerReserveTokens))
	drop, removed := 0, 0.0
	for drop < len(request.Messages)-1 && (removed < excess || drop == 0) {
		removed += float64(utf8.RuneCountInString(request.Messages[drop].Body))*tokensPerRune + tokensPerMessage
		drop++
	}
	request.Messages = request.Messages[drop:]
	encoded, err := application.EncodeAnalysisRequest(request)
	if err != nil {
		return "", false
	}
	return encoded, true
}

// inferWithinContext выполняет запрос и при отказе по размеру контекста повторяет его по более
// короткому окну. Другие ошибки возвращаются как есть.
func (p LlamaProvider) inferWithinContext(ctx context.Context, prompt string) (string, error) {
	for attempt := 0; ; attempt++ {
		raw, err := p.infer(ctx, prompt)
		var exceeded contextExceededError
		if err == nil || !errors.As(err, &exceeded) || attempt == maxContextRetries {
			return raw, err
		}
		shrunk, ok := shrinkToContext(prompt, exceeded)
		if !ok {
			return "", err
		}
		prompt = shrunk
	}
}
