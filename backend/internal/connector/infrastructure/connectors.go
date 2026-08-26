package infrastructure

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"lidradar/backend/internal/connector/domain"
)

const (
	developmentSecretHeader = "X-LidRadar-Webhook-Secret"
	telegramSecretHeader    = "X-Telegram-Bot-Api-Secret-Token"
	telegramConfigErrorCode = "TELEGRAM_CONFIGURATION_REQUIRED"
	defaultTelegramAPIURL   = "https://api.telegram.org"
	telegramResponseLimit   = 64 << 10
)

var ErrTelegramAPI = errors.New("ошибка Telegram Bot API")

var telegramSecretPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,256}$`)
var telegramBotTokenPattern = regexp.MustCompile(`^[0-9]{5,20}:[A-Za-z0-9_-]{20,128}$`)

// TelegramConfiguration задаёт сетевой клиент и публичный адрес webhook.
type TelegramConfiguration struct {
	WebhookBaseURL string
	APIBaseURL     string
	Client         *http.Client
}

// Registry содержит доступные в текущей сборке адаптеры каналов.
type Registry struct {
	registrations map[domain.Provider]domain.ConnectorRegistration
}

// NewRegistry создаёт реестр локальных адаптеров и Telegram-коннектора.
func NewRegistry(configurations ...TelegramConfiguration) Registry {
	telegram := newTelegramConnector(configurations...)
	registry := Registry{registrations: map[domain.Provider]domain.ConnectorRegistration{
		domain.ProviderTest: {
			Connector: TestConnector{},
			Capabilities: []domain.Capability{
				domain.CapabilityReceiveMessages, domain.CapabilityImportHistory,
				domain.CapabilityReceiveEdits, domain.CapabilityReceiveDeletions,
				domain.CapabilityReceiveAttachments, domain.CapabilityIdentifyContact,
			},
		},
		domain.ProviderImport: {
			Connector: ImportConnector{},
			Capabilities: []domain.Capability{
				domain.CapabilityReceiveMessages, domain.CapabilityImportHistory,
				domain.CapabilityReceiveEdits, domain.CapabilityReceiveDeletions,
				domain.CapabilityReceiveAttachments, domain.CapabilityIdentifyContact,
			},
		},
		domain.ProviderGenericWebhook: {
			Connector: GenericWebhookConnector{},
			Capabilities: []domain.Capability{
				domain.CapabilityReceiveMessages, domain.CapabilityReceiveEdits,
				domain.CapabilityReceiveDeletions, domain.CapabilityReceiveAttachments,
				domain.CapabilityIdentifyContact,
			},
		},
		domain.ProviderTelegramConnectedBusinessBot: {
			Connector: telegram,
			Capabilities: []domain.Capability{
				domain.CapabilityReceiveMessages, domain.CapabilitySendMessages,
				domain.CapabilityReceiveEdits, domain.CapabilityReceiveDeletions,
				domain.CapabilityReceiveAttachments, domain.CapabilityIdentifyContact,
			},
		},
	}}
	if telegram.configured() {
		registration := registry.registrations[domain.ProviderTelegramConnectedBusinessBot]
		registration.Provisioner = telegram
		registry.registrations[domain.ProviderTelegramConnectedBusinessBot] = registration
	}
	return registry
}

func (registry Registry) Lookup(provider domain.Provider) (domain.ConnectorRegistration, bool) {
	registration, found := registry.registrations[provider]
	if !found {
		return domain.ConnectorRegistration{}, false
	}
	registration.Capabilities = append([]domain.Capability(nil), registration.Capabilities...)
	return registration, true
}

// TestConnector принимает канонические тестовые события без сетевых вызовов.
type TestConnector struct{}

func (TestConnector) Provider() domain.Provider { return domain.ProviderTest }
func (connector TestConnector) VerifyEvent(_ context.Context, connection domain.ChannelConnection, payload []byte, headers domain.Headers) error {
	return verifyEnvelopeEvent(connection, connector.Provider(), payload, headers)
}
func (TestConnector) ExternalEventID(payload []byte, _ domain.Headers) (string, error) {
	event, err := decodeEnvelope(payload)
	return event.ID, err
}
func (connector TestConnector) NormalizeEvent(_ context.Context, connection domain.ChannelConnection, event domain.RawEvent) ([]domain.CanonicalEvent, error) {
	return normalizeEnvelope(connection, connector.Provider(), event)
}
func (TestConnector) Health(_ context.Context, _ domain.ChannelConnection) domain.ConnectionHealth {
	return activeHealth()
}

// ImportConnector принимает локальные события импорта без сетевых вызовов.
type ImportConnector struct{}

func (ImportConnector) Provider() domain.Provider { return domain.ProviderImport }
func (connector ImportConnector) VerifyEvent(_ context.Context, connection domain.ChannelConnection, payload []byte, headers domain.Headers) error {
	return verifyEnvelopeEvent(connection, connector.Provider(), payload, headers)
}
func (ImportConnector) ExternalEventID(payload []byte, _ domain.Headers) (string, error) {
	event, err := decodeEnvelope(payload)
	return event.ID, err
}
func (connector ImportConnector) NormalizeEvent(_ context.Context, connection domain.ChannelConnection, event domain.RawEvent) ([]domain.CanonicalEvent, error) {
	return normalizeEnvelope(connection, connector.Provider(), event)
}
func (ImportConnector) Health(_ context.Context, _ domain.ChannelConnection) domain.ConnectionHealth {
	return activeHealth()
}

// GenericWebhookConnector принимает канонический формат произвольного HTTP-источника.
type GenericWebhookConnector struct{}

func (GenericWebhookConnector) Provider() domain.Provider { return domain.ProviderGenericWebhook }
func (connector GenericWebhookConnector) VerifyEvent(_ context.Context, connection domain.ChannelConnection, payload []byte, headers domain.Headers) error {
	return verifyEnvelopeEvent(connection, connector.Provider(), payload, headers)
}
func (GenericWebhookConnector) ExternalEventID(payload []byte, _ domain.Headers) (string, error) {
	event, err := decodeEnvelope(payload)
	return event.ID, err
}
func (connector GenericWebhookConnector) NormalizeEvent(_ context.Context, connection domain.ChannelConnection, event domain.RawEvent) ([]domain.CanonicalEvent, error) {
	return normalizeEnvelope(connection, connector.Provider(), event)
}
func (GenericWebhookConnector) Health(_ context.Context, _ domain.ChannelConnection) domain.ConnectionHealth {
	return activeHealth()
}

// TelegramConnectedBusinessConnector принимает события Telegram Business и,
// при наличии конфигурации, управляет webhook через официальный Bot API.
type TelegramConnectedBusinessConnector struct {
	webhookBaseURL string
	apiBaseURL     string
	client         *http.Client
	now            func() time.Time
}

func (TelegramConnectedBusinessConnector) Provider() domain.Provider {
	return domain.ProviderTelegramConnectedBusinessBot
}

func (connector TelegramConnectedBusinessConnector) VerifyEvent(
	_ context.Context,
	connection domain.ChannelConnection,
	payload []byte,
	headers domain.Headers,
) error {
	if connection.Provider != connector.Provider() {
		return domain.ErrInvalid
	}
	if err := verifySecret(connection.VerificationSecretHash, headers.Get(telegramSecretHeader)); err != nil {
		return err
	}
	_, _, err := decodeTelegramUpdate(payload)
	return err
}

func (TelegramConnectedBusinessConnector) ExternalEventID(payload []byte, _ domain.Headers) (string, error) {
	updateID, _, err := decodeTelegramUpdate(payload)
	return updateID, err
}

func (connector TelegramConnectedBusinessConnector) NormalizeEvent(
	_ context.Context,
	connection domain.ChannelConnection,
	event domain.RawEvent,
) ([]domain.CanonicalEvent, error) {
	if connection.Provider != connector.Provider() || event.Provider != connector.Provider() || connection.ID != event.ConnectionID {
		return nil, domain.ErrInvalid
	}
	updateID, eventType, err := decodeTelegramUpdate(event.Payload)
	if err != nil {
		return nil, err
	}
	if eventType == "notification.control.v1" {
		return nil, nil
	}
	return normalizeTelegramUpdate(connection, event, updateID, eventType)
}

func (connector TelegramConnectedBusinessConnector) Health(_ context.Context, _ domain.ChannelConnection) domain.ConnectionHealth {
	if connector.configured() {
		return domain.ConnectionHealth{Status: domain.ConnectionActive, CheckedAt: connector.now().UTC()}
	}
	code := telegramConfigErrorCode
	return domain.ConnectionHealth{
		Status: domain.ConnectionDegraded, LastErrorCode: &code, CheckedAt: time.Now().UTC(),
	}
}

type telegramCredentials struct {
	BotToken string `json:"botToken"`
}

type telegramWebhookInfo struct {
	URL                string `json:"url"`
	PendingUpdateCount int    `json:"pending_update_count"`
	LastErrorDate      int64  `json:"last_error_date"`
}

type telegramAPIResponse struct {
	OK        bool            `json:"ok"`
	Result    json.RawMessage `json:"result"`
	ErrorCode int             `json:"error_code"`
}

func newTelegramConnector(configurations ...TelegramConfiguration) *TelegramConnectedBusinessConnector {
	configuration := TelegramConfiguration{}
	if len(configurations) > 0 {
		configuration = configurations[0]
	}
	apiBaseURL := strings.TrimRight(strings.TrimSpace(configuration.APIBaseURL), "/")
	if apiBaseURL == "" {
		apiBaseURL = defaultTelegramAPIURL
	}
	client := configuration.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &TelegramConnectedBusinessConnector{
		webhookBaseURL: strings.TrimRight(strings.TrimSpace(configuration.WebhookBaseURL), "/"),
		apiBaseURL:     apiBaseURL, client: client, now: time.Now,
	}
}

func (connector TelegramConnectedBusinessConnector) configured() bool {
	webhook, webhookErr := url.Parse(connector.webhookBaseURL)
	api, apiErr := url.Parse(connector.apiBaseURL)
	return connector.client != nil && connector.now != nil && webhookErr == nil && apiErr == nil &&
		webhook.Scheme == "https" && webhook.Host != "" && api.Scheme == "https" && api.Host != ""
}

func (connector TelegramConnectedBusinessConnector) Provision(
	ctx context.Context,
	connection domain.ChannelConnection,
	webhookSecret string,
	credentials json.RawMessage,
) (domain.ConnectionHealth, error) {
	parsed, err := decodeTelegramCredentials(credentials)
	if !connector.configured() || err != nil || connection.Provider != connector.Provider() ||
		!telegramSecretPattern.MatchString(webhookSecret) {
		return domain.ConnectionHealth{}, ErrTelegramAPI
	}
	webhookURL := connector.webhookURL(connection)
	request := map[string]any{
		"url": webhookURL, "secret_token": webhookSecret,
		"allowed_updates": []string{
			"business_connection", "business_message", "edited_business_message", "deleted_business_messages",
			"message", "callback_query",
		},
	}
	var installed bool
	if err := connector.call(ctx, parsed.BotToken, "setWebhook", request, &installed); err != nil || !installed {
		return domain.ConnectionHealth{}, ErrTelegramAPI
	}
	var info telegramWebhookInfo
	if err := connector.call(ctx, parsed.BotToken, "getWebhookInfo", struct{}{}, &info); err != nil || info.URL != webhookURL {
		return domain.ConnectionHealth{}, ErrTelegramAPI
	}
	now := connector.now().UTC()
	return domain.ConnectionHealth{Status: domain.ConnectionActive, LastSuccessAt: &now, CheckedAt: now}, nil
}

func (connector TelegramConnectedBusinessConnector) Deprovision(
	ctx context.Context,
	connection domain.ChannelConnection,
	credentials json.RawMessage,
) error {
	parsed, err := decodeTelegramCredentials(credentials)
	if !connector.configured() || err != nil || connection.Provider != connector.Provider() {
		return ErrTelegramAPI
	}
	var deleted bool
	if err := connector.call(ctx, parsed.BotToken, "deleteWebhook", map[string]bool{"drop_pending_updates": false}, &deleted); err != nil || !deleted {
		return ErrTelegramAPI
	}
	return nil
}

func (connector TelegramConnectedBusinessConnector) webhookURL(connection domain.ChannelConnection) string {
	return fmt.Sprintf(
		"%s/api/v1/webhooks/%s/%s/%s", connector.webhookBaseURL,
		url.PathEscape(string(connection.Provider)), url.PathEscape(connection.TenantID), url.PathEscape(connection.ID),
	)
}

func (connector TelegramConnectedBusinessConnector) call(
	ctx context.Context,
	botToken, method string,
	payload any,
	result any,
) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return ErrTelegramAPI
	}
	endpoint := connector.apiBaseURL + "/bot" + botToken + "/" + method
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return ErrTelegramAPI
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := connector.client.Do(request)
	if err != nil {
		return ErrTelegramAPI
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, telegramResponseLimit+1))
	if err != nil || len(body) > telegramResponseLimit || response.StatusCode != http.StatusOK {
		return ErrTelegramAPI
	}
	var envelope telegramAPIResponse
	if json.Unmarshal(body, &envelope) != nil || !envelope.OK || len(envelope.Result) == 0 || envelope.ErrorCode != 0 {
		return ErrTelegramAPI
	}
	if result != nil && json.Unmarshal(envelope.Result, result) != nil {
		return ErrTelegramAPI
	}
	return nil
}

func decodeTelegramCredentials(value json.RawMessage) (telegramCredentials, error) {
	var credentials telegramCredentials
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&credentials); err != nil {
		return telegramCredentials{}, ErrTelegramAPI
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) ||
		!telegramBotTokenPattern.MatchString(strings.TrimSpace(credentials.BotToken)) {
		return telegramCredentials{}, ErrTelegramAPI
	}
	credentials.BotToken = strings.TrimSpace(credentials.BotToken)
	return credentials, nil
}

type envelope struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	OccurredAt time.Time       `json:"occurredAt"`
	Data       json.RawMessage `json:"data"`
}

type canonicalEnvelopeData struct {
	ConversationExternalID   string                       `json:"conversationExternalId"`
	MessageExternalID        string                       `json:"messageExternalId"`
	ContactExternalID        string                       `json:"contactExternalId"`
	ContactDisplayName       *string                      `json:"contactDisplayName"`
	ContactPhoneNormalized   *string                      `json:"contactPhoneNormalized"`
	ContactEmailNormalized   *string                      `json:"contactEmailNormalized"`
	Direction                domain.CanonicalDirection    `json:"direction"`
	MessageType              domain.CanonicalMessageType  `json:"messageType"`
	Text                     *string                      `json:"text"`
	SenderExternalID         *string                      `json:"senderExternalId"`
	ReplyToMessageExternalID *string                      `json:"replyToMessageExternalId"`
	SentAt                   time.Time                    `json:"sentAt"`
	Attachments              []domain.CanonicalAttachment `json:"attachments"`
	Metadata                 json.RawMessage              `json:"metadata"`
}

func verifyEnvelopeEvent(
	connection domain.ChannelConnection,
	provider domain.Provider,
	payload []byte,
	headers domain.Headers,
) error {
	if connection.Provider != provider {
		return domain.ErrInvalid
	}
	if err := verifySecret(connection.VerificationSecretHash, headers.Get(developmentSecretHeader)); err != nil {
		return err
	}
	_, err := decodeEnvelope(payload)
	return err
}

func normalizeEnvelope(connection domain.ChannelConnection, provider domain.Provider, event domain.RawEvent) ([]domain.CanonicalEvent, error) {
	if connection.Provider != provider || event.Provider != provider || connection.ID != event.ConnectionID {
		return nil, domain.ErrInvalid
	}
	decoded, err := decodeEnvelope(event.Payload)
	if err != nil {
		return nil, err
	}
	var data canonicalEnvelopeData
	decoder := json.NewDecoder(bytes.NewReader(decoded.Data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&data); err != nil {
		return nil, domain.ErrInvalidPayload
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, domain.ErrInvalidPayload
	}
	if len(data.Metadata) == 0 {
		data.Metadata = json.RawMessage(`{}`)
	}
	canonical := domain.CanonicalEvent{
		SourceEventID: decoded.ID, Type: domain.CanonicalEventType(decoded.Type),
		TenantID: event.TenantID, ConnectionID: connection.ID, LocationID: connection.LocationID,
		Provider: provider, ConversationExternalID: strings.TrimSpace(data.ConversationExternalID),
		MessageExternalID: strings.TrimSpace(data.MessageExternalID), ContactExternalID: strings.TrimSpace(data.ContactExternalID),
		ContactDisplayName: cleanOptional(data.ContactDisplayName), ContactPhoneNormalized: cleanOptional(data.ContactPhoneNormalized),
		ContactEmailNormalized: cleanOptional(data.ContactEmailNormalized), Direction: data.Direction,
		MessageType: data.MessageType, Text: data.Text, SenderExternalID: cleanOptional(data.SenderExternalID),
		ReplyToMessageExternalID: cleanOptional(data.ReplyToMessageExternalID), SentAt: data.SentAt.UTC(),
		OccurredAt: decoded.OccurredAt.UTC(), ReceivedAt: event.ReceivedAt.UTC(),
		Attachments: append([]domain.CanonicalAttachment(nil), data.Attachments...), Metadata: append(json.RawMessage(nil), data.Metadata...),
	}
	if canonical.Validate() != nil {
		return nil, domain.ErrInvalidPayload
	}
	return []domain.CanonicalEvent{canonical}, nil
}

func decodeEnvelope(payload []byte) (envelope, error) {
	var event envelope
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&event); err != nil {
		return envelope{}, domain.ErrInvalidPayload
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return envelope{}, domain.ErrInvalidPayload
	}
	event.ID = strings.TrimSpace(event.ID)
	event.Type = strings.TrimSpace(event.Type)
	if event.ID == "" || len(event.ID) > 512 || event.Type == "" || len(event.Type) > 200 || event.OccurredAt.IsZero() || !json.Valid(event.Data) {
		return envelope{}, domain.ErrInvalidPayload
	}
	return event, nil
}

func decodeTelegramUpdate(payload []byte) (string, string, error) {
	var update map[string]json.RawMessage
	if err := json.Unmarshal(payload, &update); err != nil {
		return "", "", domain.ErrInvalidPayload
	}
	rawUpdateID, found := update["update_id"]
	if !found {
		return "", "", domain.ErrInvalidPayload
	}
	var updateID json.Number
	decoder := json.NewDecoder(bytes.NewReader(rawUpdateID))
	decoder.UseNumber()
	if err := decoder.Decode(&updateID); err != nil {
		return "", "", domain.ErrInvalidPayload
	}
	numericID, err := strconv.ParseInt(updateID.String(), 10, 64)
	if err != nil || numericID < 0 {
		return "", "", domain.ErrInvalidPayload
	}
	types := []struct {
		field string
		kind  string
	}{
		{"business_connection", "connection.updated.v1"},
		{"business_message", "message.received.v1"},
		{"edited_business_message", "message.edited.v1"},
		{"deleted_business_messages", "message.deleted.v1"},
		{"message", "notification.control.v1"},
		{"callback_query", "notification.control.v1"},
	}
	eventType := ""
	for _, candidate := range types {
		if raw, exists := update[candidate.field]; exists && len(raw) > 0 && string(raw) != "null" {
			if eventType != "" {
				return "", "", domain.ErrInvalidPayload
			}
			eventType = candidate.kind
		}
	}
	if eventType == "" {
		return "", "", domain.ErrInvalidPayload
	}
	return strconv.FormatInt(numericID, 10), eventType, nil
}

type telegramUpdate struct {
	BusinessConnection      json.RawMessage          `json:"business_connection"`
	BusinessMessage         *telegramMessage         `json:"business_message"`
	EditedBusinessMessage   *telegramMessage         `json:"edited_business_message"`
	DeletedBusinessMessages *telegramDeletedMessages `json:"deleted_business_messages"`
}

type telegramUser struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}

type telegramChat struct {
	ID        int64  `json:"id"`
	Type      string `json:"type"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}

type telegramFile struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileSize     int64  `json:"file_size"`
	MIMEType     string `json:"mime_type"`
}

type telegramPhoto struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileSize     int64  `json:"file_size"`
}

type telegramMessage struct {
	MessageID            int64            `json:"message_id"`
	Date                 int64            `json:"date"`
	EditDate             int64            `json:"edit_date"`
	BusinessConnectionID string           `json:"business_connection_id"`
	From                 *telegramUser    `json:"from"`
	SenderBusinessBot    *telegramUser    `json:"sender_business_bot"`
	Chat                 telegramChat     `json:"chat"`
	Text                 *string          `json:"text"`
	Caption              *string          `json:"caption"`
	Photo                []telegramPhoto  `json:"photo"`
	Voice                *telegramFile    `json:"voice"`
	Audio                *telegramFile    `json:"audio"`
	Video                *telegramFile    `json:"video"`
	Document             *telegramFile    `json:"document"`
	ReplyToMessage       *telegramMessage `json:"reply_to_message"`
}

type telegramDeletedMessages struct {
	BusinessConnectionID string       `json:"business_connection_id"`
	Chat                 telegramChat `json:"chat"`
	MessageIDs           []int64      `json:"message_ids"`
}

func normalizeTelegramUpdate(
	connection domain.ChannelConnection,
	raw domain.RawEvent,
	updateID, eventType string,
) ([]domain.CanonicalEvent, error) {
	var update telegramUpdate
	if err := json.Unmarshal(raw.Payload, &update); err != nil {
		return nil, domain.ErrInvalidPayload
	}
	switch eventType {
	case "connection.updated.v1":
		return []domain.CanonicalEvent{}, nil
	case "message.received.v1", "message.edited.v1":
		message := update.BusinessMessage
		canonicalType := domain.CanonicalMessageReceived
		if eventType == "message.edited.v1" {
			message = update.EditedBusinessMessage
			canonicalType = domain.CanonicalMessageEdited
		}
		if message == nil {
			return nil, domain.ErrInvalidPayload
		}
		canonical, err := canonicalTelegramMessage(connection, raw, updateID, canonicalType, *message)
		if err != nil {
			return nil, err
		}
		return []domain.CanonicalEvent{canonical}, nil
	case "message.deleted.v1":
		deleted := update.DeletedBusinessMessages
		if deleted == nil || deleted.BusinessConnectionID == "" || deleted.Chat.ID == 0 || len(deleted.MessageIDs) == 0 {
			return nil, domain.ErrInvalidPayload
		}
		conversationID := telegramConversationID(deleted.BusinessConnectionID, deleted.Chat.ID)
		result := make([]domain.CanonicalEvent, 0, len(deleted.MessageIDs))
		for _, messageID := range deleted.MessageIDs {
			canonical := domain.CanonicalEvent{
				SourceEventID: updateID + ":" + strconv.FormatInt(messageID, 10), Type: domain.CanonicalMessageDeleted,
				TenantID: raw.TenantID, ConnectionID: connection.ID, LocationID: connection.LocationID,
				Provider: connection.Provider, ConversationExternalID: conversationID,
				MessageExternalID: telegramMessageID(deleted.Chat.ID, messageID), OccurredAt: raw.ReceivedAt.UTC(),
				ReceivedAt: raw.ReceivedAt.UTC(), Attachments: []domain.CanonicalAttachment{}, Metadata: json.RawMessage(`{}`),
			}
			if canonical.Validate() != nil {
				return nil, domain.ErrInvalidPayload
			}
			result = append(result, canonical)
		}
		return result, nil
	default:
		return nil, domain.ErrInvalidPayload
	}
}

func canonicalTelegramMessage(
	connection domain.ChannelConnection,
	raw domain.RawEvent,
	updateID string,
	eventType domain.CanonicalEventType,
	message telegramMessage,
) (domain.CanonicalEvent, error) {
	if message.BusinessConnectionID == "" || message.Chat.ID == 0 || message.MessageID == 0 || message.Date <= 0 {
		return domain.CanonicalEvent{}, domain.ErrInvalidPayload
	}
	direction := domain.CanonicalIncoming
	if message.SenderBusinessBot != nil || (message.From != nil && message.From.ID != message.Chat.ID) {
		direction = domain.CanonicalOutgoing
	}
	messageType, attachments := telegramMessageContent(message)
	text := message.Text
	if text == nil {
		text = message.Caption
	}
	var senderExternalID *string
	if message.From != nil {
		value := strconv.FormatInt(message.From.ID, 10)
		senderExternalID = &value
	}
	var replyExternalID *string
	if message.ReplyToMessage != nil && message.ReplyToMessage.MessageID != 0 {
		value := telegramMessageID(message.Chat.ID, message.ReplyToMessage.MessageID)
		replyExternalID = &value
	}
	displayName := strings.TrimSpace(strings.Join([]string{message.Chat.FirstName, message.Chat.LastName}, " "))
	var displayNamePointer *string
	if displayName != "" {
		displayNamePointer = &displayName
	}
	metadata, _ := json.Marshal(map[string]string{"businessConnectionId": message.BusinessConnectionID})
	occurredAt := time.Unix(message.Date, 0).UTC()
	if message.EditDate > 0 {
		occurredAt = time.Unix(message.EditDate, 0).UTC()
	}
	canonical := domain.CanonicalEvent{
		SourceEventID: updateID, Type: eventType, TenantID: raw.TenantID, ConnectionID: connection.ID,
		LocationID: connection.LocationID, Provider: connection.Provider,
		ConversationExternalID: telegramConversationID(message.BusinessConnectionID, message.Chat.ID),
		MessageExternalID:      telegramMessageID(message.Chat.ID, message.MessageID),
		ContactExternalID:      strconv.FormatInt(message.Chat.ID, 10), ContactDisplayName: displayNamePointer,
		Direction: direction, MessageType: messageType, Text: text, SenderExternalID: senderExternalID,
		ReplyToMessageExternalID: replyExternalID, SentAt: time.Unix(message.Date, 0).UTC(),
		OccurredAt: occurredAt, ReceivedAt: raw.ReceivedAt.UTC(), Attachments: attachments, Metadata: metadata,
	}
	if eventType == domain.CanonicalMessageEdited {
		canonical.ContactExternalID = ""
		canonical.ContactDisplayName = nil
	}
	if canonical.Validate() != nil {
		return domain.CanonicalEvent{}, domain.ErrInvalidPayload
	}
	return canonical, nil
}

func telegramMessageContent(message telegramMessage) (domain.CanonicalMessageType, []domain.CanonicalAttachment) {
	if len(message.Photo) > 0 {
		photo := message.Photo[len(message.Photo)-1]
		mime := "image/jpeg"
		return domain.CanonicalImage, []domain.CanonicalAttachment{telegramAttachment(photo.FileID, photo.FileUniqueID, photo.FileSize, &mime)}
	}
	for _, candidate := range []struct {
		file        *telegramFile
		messageType domain.CanonicalMessageType
	}{
		{message.Voice, domain.CanonicalVoice}, {message.Audio, domain.CanonicalAudio},
		{message.Video, domain.CanonicalVideo}, {message.Document, domain.CanonicalDocument},
	} {
		if candidate.file != nil {
			mime := cleanStringPointer(candidate.file.MIMEType)
			return candidate.messageType, []domain.CanonicalAttachment{
				telegramAttachment(candidate.file.FileID, candidate.file.FileUniqueID, candidate.file.FileSize, mime),
			}
		}
	}
	if message.Text != nil {
		return domain.CanonicalText, []domain.CanonicalAttachment{}
	}
	return domain.CanonicalOther, []domain.CanonicalAttachment{}
}

func telegramAttachment(fileID, uniqueID string, size int64, mime *string) domain.CanonicalAttachment {
	providerFileID := strings.TrimSpace(fileID)
	if providerFileID == "" {
		return domain.CanonicalAttachment{}
	}
	keyPart := strings.TrimSpace(uniqueID)
	if keyPart == "" {
		keyPart = providerFileID
	}
	return domain.CanonicalAttachment{
		ObjectKey: "stub/telegram/" + keyPart, MIMEType: mime, SizeBytes: size, ProviderFileID: &providerFileID,
	}
}

func telegramConversationID(businessConnectionID string, chatID int64) string {
	return businessConnectionID + ":" + strconv.FormatInt(chatID, 10)
}

func telegramMessageID(chatID, messageID int64) string {
	return strconv.FormatInt(chatID, 10) + ":" + strconv.FormatInt(messageID, 10)
}

func cleanOptional(value *string) *string {
	if value == nil {
		return nil
	}
	cleaned := strings.TrimSpace(*value)
	if cleaned == "" {
		return nil
	}
	return &cleaned
}

func cleanStringPointer(value string) *string {
	return cleanOptional(&value)
}

func verifySecret(expectedHash, providedSecret string) error {
	if expectedHash == "" || providedSecret == "" {
		return domain.ErrUnauthenticated
	}
	expected, err := hex.DecodeString(expectedHash)
	if err != nil || len(expected) != sha256.Size {
		return domain.ErrUnauthenticated
	}
	digest := sha256.Sum256([]byte(providedSecret))
	if subtle.ConstantTimeCompare(expected, digest[:]) != 1 {
		return domain.ErrUnauthenticated
	}
	return nil
}

func activeHealth() domain.ConnectionHealth {
	return domain.ConnectionHealth{Status: domain.ConnectionActive, CheckedAt: time.Now().UTC()}
}
