package infrastructure

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	connectordomain "lidradar/backend/internal/connector/domain"
	"lidradar/backend/internal/notification/application"
	"lidradar/backend/platform/ids"
)

type TelegramControlTransport interface {
	SendText(context.Context, string, string) (string, bool, error)
	AnswerCallback(context.Context, string, string) (bool, error)
}

type TelegramControlHandler struct {
	linker    application.Linker
	callbacks application.SafeCallbackExecutor
	telegram  TelegramControlTransport
}

func NewTelegramControlHandler(
	linker application.Linker,
	callbacks application.SafeCallbackExecutor,
	telegram TelegramControlTransport,
) TelegramControlHandler {
	return TelegramControlHandler{linker: linker, callbacks: callbacks, telegram: telegram}
}

type controlUpdate struct {
	BusinessConnection      json.RawMessage `json:"business_connection"`
	BusinessMessage         json.RawMessage `json:"business_message"`
	EditedBusinessMessage   json.RawMessage `json:"edited_business_message"`
	DeletedBusinessMessages json.RawMessage `json:"deleted_business_messages"`
	Message                 *controlMessage `json:"message"`
	Callback                *callbackQuery  `json:"callback_query"`
}

type controlUser struct {
	ID int64 `json:"id"`
}

type controlChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type controlMessage struct {
	From *controlUser `json:"from"`
	Chat controlChat  `json:"chat"`
	Text string       `json:"text"`
}

type callbackQuery struct {
	ID      string          `json:"id"`
	From    controlUser     `json:"from"`
	Message *controlMessage `json:"message"`
	Data    string          `json:"data"`
}

// HandleConnectorControl реализует служебную ветку единого Telegram webhook.
// Неизвестные сообщения безопасно поглощаются и не становятся переписками.
func (handler TelegramControlHandler) HandleConnectorControl(
	ctx context.Context,
	tenantID string,
	provider connectordomain.Provider,
	payload []byte,
) (bool, error) {
	if provider != connectordomain.ProviderTelegramConnectedBusinessBot {
		return false, nil
	}
	var update controlUpdate
	if json.Unmarshal(payload, &update) != nil {
		return false, nil
	}
	if len(update.BusinessConnection) > 0 || len(update.BusinessMessage) > 0 ||
		len(update.EditedBusinessMessage) > 0 || len(update.DeletedBusinessMessages) > 0 {
		return false, nil
	}
	if update.Message != nil {
		return true, handler.handleStart(ctx, tenantID, *update.Message)
	}
	if update.Callback != nil {
		return true, handler.handleCallback(ctx, tenantID, *update.Callback)
	}
	return false, nil
}

func (handler TelegramControlHandler) handleStart(ctx context.Context, tenantID string, message controlMessage) error {
	if message.From == nil || message.From.ID <= 0 || message.Chat.ID == 0 || message.Chat.Type != "private" ||
		message.Chat.ID != message.From.ID {
		return nil
	}
	fields := strings.Fields(message.Text)
	if len(fields) != 2 || (fields[0] != "/start" && !strings.HasPrefix(fields[0], "/start@")) || len(fields[1]) > 128 {
		return nil
	}
	chatID := integerText(message.Chat.ID)
	_, err := handler.linker.Redeem(ctx, tenantID, fields[1], integerText(message.From.ID), chatID)
	if err != nil {
		if handler.telegram != nil {
			_, _, _ = handler.telegram.SendText(ctx, chatID, "Код привязки недействителен или уже использован.")
		}
		return application.ClassifyControlError(err)
	}
	if handler.telegram != nil {
		_, _, _ = handler.telegram.SendText(ctx, chatID, "Telegram успешно привязан к LidRadar.")
	}
	return nil
}

func (handler TelegramControlHandler) handleCallback(ctx context.Context, tenantID string, query callbackQuery) error {
	if query.ID == "" || len(query.ID) > 255 || query.From.ID <= 0 || query.Message == nil ||
		query.Message.Chat.ID == 0 || query.Message.Chat.Type != "private" {
		return nil
	}
	parts := strings.Split(query.Data, ":")
	if len(parts) != 2 || !ids.Valid(parts[1]) {
		return nil
	}
	action := application.CallbackAction(parts[0])
	userID, riskID, found, err := handler.callbacks.Resolve(
		ctx, tenantID, parts[1], integerText(query.From.ID), integerText(query.Message.Chat.ID),
	)
	if err != nil || !found {
		return application.ClassifyControlError(err)
	}
	command := application.CallbackCommand{
		TenantID: tenantID, UserID: userID, NotificationID: parts[1], RiskID: riskID,
		IdempotencyKey: query.ID, Action: action,
	}
	if err := application.HandleCallback(ctx, handler.callbacks, command); err != nil {
		return application.ClassifyControlError(err)
	}
	if handler.telegram != nil {
		_, _ = handler.telegram.AnswerCallback(ctx, query.ID, application.CallbackResponse(action))
	}
	return nil
}

func integerText(value int64) string {
	return strconv.FormatInt(value, 10)
}
