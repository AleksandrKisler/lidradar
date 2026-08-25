package infrastructure

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/internal/risk/application"
	"lidradar/backend/platform/ids"
)

const riskInvalidationChannel = "lidradar_risk_invalidations"

const invalidationTimeout = 2 * time.Second

type invalidationPayload struct {
	TenantID   string `json:"tenantId"`
	Type       string `json:"type"`
	ResourceID string `json:"resourceId"`
}

// PostgresInvalidator связывает независимые процессы API и worker через
// краткоживущий LISTEN/NOTIFY. PostgreSQL-таблицы остаются источником истины:
// потерянный сигнал не теряет данные, а клиент после сигнала перечитывает REST.
type PostgresInvalidator struct {
	pool *pgxpool.Pool
}

func NewPostgresInvalidator(pool *pgxpool.Pool) *PostgresInvalidator {
	return &PostgresInvalidator{pool: pool}
}

func (notifier *PostgresInvalidator) Publish(tenantID, eventType, resourceID string) {
	if notifier == nil || notifier.pool == nil || !ids.Valid(tenantID) || !ids.Valid(resourceID) ||
		!validInvalidationType(eventType) {
		return
	}
	payload, err := json.Marshal(invalidationPayload{TenantID: tenantID, Type: eventType, ResourceID: resourceID})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), invalidationTimeout)
	defer cancel()
	// Сигнал намеренно не участвует в транзакции предметных данных и может
	// быть потерян: подключившийся клиент всегда восстанавливается через REST.
	_, _ = notifier.pool.Exec(ctx, `SELECT pg_notify($1, $2)`, riskInvalidationChannel, string(payload))
}

// Listen занимает отдельное соединение, чтобы длительное ожидание не
// уменьшало рабочий пул API. Возвращаемая ошибка позволяет внешнему циклу
// безопасно переподключиться после разрыва PostgreSQL-сессии.
func (notifier *PostgresInvalidator) Listen(
	ctx context.Context,
	sink application.Invalidator,
	onReady func(),
) error {
	if notifier == nil || notifier.pool == nil || sink == nil {
		return application.ErrInvalidCommand
	}
	configuration := notifier.pool.Config().ConnConfig.Copy()
	connection, err := pgx.ConnectConfig(ctx, configuration)
	if err != nil {
		return err
	}
	defer func() { _ = connection.Close(context.Background()) }()
	if _, err = connection.Exec(ctx, `LISTEN `+riskInvalidationChannel); err != nil {
		return err
	}
	if onReady != nil {
		onReady()
	}
	for {
		notification, waitErr := connection.WaitForNotification(ctx)
		if waitErr != nil {
			if errors.Is(waitErr, context.Canceled) || errors.Is(waitErr, context.DeadlineExceeded) {
				return ctx.Err()
			}
			return waitErr
		}
		var payload invalidationPayload
		if json.Unmarshal([]byte(notification.Payload), &payload) != nil ||
			!ids.Valid(payload.TenantID) || !ids.Valid(payload.ResourceID) ||
			!validInvalidationType(payload.Type) {
			continue
		}
		sink.Publish(payload.TenantID, payload.Type, payload.ResourceID)
	}
}

func validInvalidationType(eventType string) bool {
	switch eventType {
	case "risk.changed", "risk.acknowledged", "risk.resolved":
		return true
	default:
		return false
	}
}

var _ application.Invalidator = (*PostgresInvalidator)(nil)
