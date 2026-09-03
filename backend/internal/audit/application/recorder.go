// Package application задаёт контракт записи аудита для остальных модулей.
package application

import (
	"context"
	"time"

	"lidradar/backend/internal/audit/domain"
)

// Recorder пишет запись сразу после успешного изменения состояния. Ошибка
// записи возвращается вызывающему: действие уже совершено, но отсутствие
// следа считается сбоем, а не допустимым исходом (ТЗ §65).
type Recorder interface {
	Tenant(ctx context.Context, entry domain.Entry) error
	Auth(ctx context.Context, entry domain.AuthEntry) error
}

// TenantEntry собирает запись действия участника без идентификатора: его
// присваивает хранилище.
func TenantEntry(tenantID, actorID, operation, entityType, entityID string, at time.Time) domain.Entry {
	return domain.Entry{TenantID: tenantID, ActorID: actorID, Operation: operation, EntityType: entityType, EntityID: entityID, At: at.UTC()}
}

func AuthEvent(userID, operation, ipAddress string, at time.Time) domain.AuthEntry {
	return domain.AuthEntry{UserID: userID, Operation: operation, IPAddress: ipAddress, At: at.UTC()}
}
