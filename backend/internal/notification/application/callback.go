package application

import (
	"context"
	"errors"
	"time"

	riskdomain "lidradar/backend/internal/risk/domain"
)

type CallbackStore interface {
	CallbackTarget(context.Context, string, string, string, string) (userID, riskID string, found bool, err error)
	CallbackRecorded(context.Context, string, string) (bool, error)
	RecordCallback(context.Context, CallbackCommand, string, time.Time) (bool, error)
}

type RiskAcknowledger interface {
	Acknowledge(context.Context, string, string, string) (riskdomain.Risk, error)
}

// SafeCallbackExecutor выполняет только операции, разрешённые ТЗ. OPEN_RISK
// фиксирует навигационное намерение, SNOOZE помечает это уведомление, а
// ACKNOWLEDGE идемпотентно переводит Risk через его собственный сервис.
type SafeCallbackExecutor struct {
	store CallbackStore
	radar RiskAcknowledger
	ids   IDs
	now   func() time.Time
}

func NewSafeCallbackExecutor(store CallbackStore, radar RiskAcknowledger, ids IDs, now func() time.Time) SafeCallbackExecutor {
	return SafeCallbackExecutor{store: store, radar: radar, ids: ids, now: now}
}

func (executor SafeCallbackExecutor) Resolve(
	ctx context.Context,
	tenantID, notificationID, telegramUserID, chatID string,
) (userID, riskID string, found bool, err error) {
	if executor.store == nil || tenantID == "" || notificationID == "" || telegramUserID == "" || chatID == "" {
		return "", "", false, ErrUnsafeCallback
	}
	return executor.store.CallbackTarget(ctx, tenantID, notificationID, telegramUserID, chatID)
}

func (executor SafeCallbackExecutor) ExecuteSafeCallback(ctx context.Context, command CallbackCommand) error {
	if executor.store == nil || executor.ids == nil || executor.now == nil {
		return ErrUnsafeCallback
	}
	recorded, err := executor.store.CallbackRecorded(ctx, command.TenantID, command.IdempotencyKey)
	if err != nil || recorded {
		return err
	}
	if command.Action == CallbackAcknowledge {
		if executor.radar == nil {
			return ErrUnsafeCallback
		}
		if _, err := executor.radar.Acknowledge(ctx, command.UserID, command.TenantID, command.RiskID); err != nil {
			return err
		}
	}
	id, err := executor.ids.NewID()
	if err != nil {
		return err
	}
	_, err = executor.store.RecordCallback(ctx, command, id, executor.now().UTC())
	return err
}

func CallbackResponse(action CallbackAction) string {
	switch action {
	case CallbackOpen:
		return "Риск готов к открытию в Radar"
	case CallbackAcknowledge:
		return "Риск принят в работу"
	case CallbackSnooze:
		return "Уведомление отложено"
	default:
		return "Команда отклонена"
	}
}

func ClassifyControlError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrUnsafeCallback) || errors.Is(err, ErrInvalidLinkToken) || errors.Is(err, ErrForbidden) {
		return nil
	}
	return err
}
