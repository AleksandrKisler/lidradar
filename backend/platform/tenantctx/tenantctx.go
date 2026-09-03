// Package tenantctx переносит организацию и пользователя запроса до
// соединения PostgreSQL: политики RLS читают их из настроек сеанса
// (ADR 0034). Пустое значение означает отсутствие контекста — fail-closed.
package tenantctx

import "context"

type key int

const (
	tenantKey key = iota
	actorKey
)

// WithTenant привязывает организацию к контексту запроса или задания.
func WithTenant(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, tenantKey, tenantID)
}

// Tenant возвращает организацию контекста или пустую строку.
func Tenant(ctx context.Context) string {
	value, _ := ctx.Value(tenantKey).(string)
	return value
}

// WithActor привязывает пользователя: политика членств показывает ему
// собственные строки без организации (список организаций пользователя).
func WithActor(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, actorKey, userID)
}

// Actor возвращает пользователя контекста или пустую строку.
func Actor(ctx context.Context) string {
	value, _ := ctx.Value(actorKey).(string)
	return value
}
