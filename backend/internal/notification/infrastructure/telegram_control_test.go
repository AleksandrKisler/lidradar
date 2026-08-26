package infrastructure

import (
	"context"
	"fmt"
	"testing"
	"time"

	connectordomain "lidradar/backend/internal/connector/domain"
	"lidradar/backend/internal/notification/application"
	riskdomain "lidradar/backend/internal/risk/domain"
)

type controlIDs struct{ next int }

func (generator *controlIDs) NewID() (string, error) {
	generator.next++
	return fmt.Sprintf("018f0000-0000-7000-8000-%012d", generator.next), nil
}

type controlTokens struct {
	token application.LinkToken
	used  bool
}

func (store *controlTokens) SaveToken(_ context.Context, token application.LinkToken) error {
	store.token = token
	return nil
}

func (store *controlTokens) UseToken(
	_ context.Context,
	tenantID, hash string,
	at time.Time,
	telegramUserID, chatID string,
) (application.LinkToken, error) {
	if store.used || tenantID != store.token.TenantID || hash != store.token.TokenHash ||
		!at.Before(store.token.ExpiresAt) || telegramUserID != "7001" || chatID != "7001" {
		return application.LinkToken{}, application.ErrInvalidLinkToken
	}
	store.used = true
	store.token.UsedAt = &at
	return store.token, nil
}

type controlTelegram struct {
	texts, answers int
}

func (transport *controlTelegram) SendText(context.Context, string, string) (string, bool, error) {
	transport.texts++
	return "1", false, nil
}

func (transport *controlTelegram) AnswerCallback(context.Context, string, string) (bool, error) {
	transport.answers++
	return false, nil
}

func TestTelegramControlRedeemsStartTokenOnce(t *testing.T) {
	now := time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)
	tokens, telegram, generator := new(controlTokens), new(controlTelegram), new(controlIDs)
	linker := application.NewLinker(tokens, generator, func() time.Time { return now })
	issued, err := linker.Issue(context.Background(), "tenant", "user", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewTelegramControlHandler(linker, application.SafeCallbackExecutor{}, telegram)
	payload := []byte(fmt.Sprintf(`{
		"update_id":1,
		"message":{"from":{"id":7001},"chat":{"id":7001,"type":"private"},"text":"/start %s"}
	}`, issued.Plaintext))
	handled, err := handler.HandleConnectorControl(
		context.Background(), "tenant", connectordomain.ProviderTelegramConnectedBusinessBot, payload,
	)
	if err != nil || !handled || !tokens.used || telegram.texts != 1 {
		t.Fatalf("/start: handled=%v used=%v texts=%d err=%v", handled, tokens.used, telegram.texts, err)
	}
	// Повтор не меняет привязку; пользователю возвращается безопасное сообщение.
	if handled, err := handler.HandleConnectorControl(
		context.Background(), "tenant", connectordomain.ProviderTelegramConnectedBusinessBot, payload,
	); err != nil || !handled || telegram.texts != 2 {
		t.Fatalf("повтор /start: handled=%v texts=%d err=%v", handled, telegram.texts, err)
	}
}

type callbackMemory struct {
	commands map[string]application.CallbackCommand
}

func (store *callbackMemory) CallbackTarget(
	_ context.Context,
	tenantID, notificationID, telegramUserID, chatID string,
) (string, string, bool, error) {
	if tenantID != "tenant" || notificationID != "018f0000-0000-7000-8000-000000000100" ||
		telegramUserID != "7001" || chatID != "7001" {
		return "", "", false, nil
	}
	return "user", "018f0000-0000-7000-8000-000000000200", true, nil
}

func (store *callbackMemory) CallbackRecorded(_ context.Context, _ string, key string) (bool, error) {
	_, found := store.commands[key]
	return found, nil
}

func (store *callbackMemory) RecordCallback(
	_ context.Context,
	command application.CallbackCommand,
	_ string,
	_ time.Time,
) (bool, error) {
	if _, found := store.commands[command.IdempotencyKey]; found {
		return false, nil
	}
	store.commands[command.IdempotencyKey] = command
	return true, nil
}

type callbackRadar struct{ acknowledgements int }

func (radar *callbackRadar) Acknowledge(context.Context, string, string, string) (riskdomain.Risk, error) {
	radar.acknowledgements++
	return riskdomain.Risk{}, nil
}

func TestTelegramControlAllowsOnlySafeTenantLinkedCallbacks(t *testing.T) {
	store := &callbackMemory{commands: make(map[string]application.CallbackCommand)}
	radar, telegram, generator := new(callbackRadar), new(controlTelegram), new(controlIDs)
	executor := application.NewSafeCallbackExecutor(store, radar, generator, time.Now)
	handler := NewTelegramControlHandler(application.Linker{}, executor, telegram)

	for index, action := range []application.CallbackAction{
		application.CallbackOpen, application.CallbackAcknowledge, application.CallbackSnooze,
	} {
		payload := []byte(fmt.Sprintf(`{
			"update_id":%d,
			"callback_query":{"id":"callback-%d","from":{"id":7001},
			"message":{"chat":{"id":7001,"type":"private"}},
			"data":"%s:018f0000-0000-7000-8000-000000000100"}
		}`, index+1, index+1, action))
		handled, err := handler.HandleConnectorControl(
			context.Background(), "tenant", connectordomain.ProviderTelegramConnectedBusinessBot, payload,
		)
		if err != nil || !handled {
			t.Fatalf("%s: handled=%v err=%v", action, handled, err)
		}
	}
	if len(store.commands) != 3 || radar.acknowledgements != 1 || telegram.answers != 3 {
		t.Fatalf("commands=%d acknowledge=%d answers=%d", len(store.commands), radar.acknowledgements, telegram.answers)
	}
	unsafe := []byte(`{
		"update_id":9,
		"callback_query":{"id":"callback-unsafe","from":{"id":7001},
		"message":{"chat":{"id":7001,"type":"private"}},
		"data":"CONFIRM_REVENUE:018f0000-0000-7000-8000-000000000100"}
	}`)
	if handled, err := handler.HandleConnectorControl(
		context.Background(), "tenant", connectordomain.ProviderTelegramConnectedBusinessBot, unsafe,
	); err != nil || !handled || len(store.commands) != 3 {
		t.Fatalf("небезопасная команда: handled=%v commands=%d err=%v", handled, len(store.commands), err)
	}
	if handled, err := handler.HandleConnectorControl(
		context.Background(), "tenant", connectordomain.ProviderTest, unsafe,
	); err != nil || handled {
		t.Fatalf("чужой канал обработан: handled=%v err=%v", handled, err)
	}
}
