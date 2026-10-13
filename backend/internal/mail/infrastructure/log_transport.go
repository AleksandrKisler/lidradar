package infrastructure

import (
	"context"

	"lidradar/backend/internal/mail/application"
)

// LogTransport «отправляет» письмо записью в журнал вместе с адресом и текстом (в них ссылка с токеном).
// Только для разработки и тестов: конфигурация не допускает его в staging и production (ADR 0058).
type LogTransport struct {
	Logger interface {
		Info(message string, arguments ...any)
	}
}

func (t LogTransport) Send(_ context.Context, message application.Outgoing) error {
	t.Logger.Info("Письмо записано в журнал вместо отправки",
		"event", "mail.logged", "mail_id", message.ID, "to", message.To, "subject", message.Subject, "text", message.Text)
	return nil
}

var _ application.Transport = LogTransport{}
