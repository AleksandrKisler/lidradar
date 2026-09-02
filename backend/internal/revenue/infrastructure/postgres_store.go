package infrastructure

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/internal/revenue/application"
	"lidradar/backend/internal/revenue/domain"
	"lidradar/backend/platform/ids"
)

const revenueOperation = "revenue.confirm"

// PostgresStore хранит событие выручки, единственную атрибуцию, результат
// идемпотентной команды и аудит одной транзакцией.
type PostgresStore struct{ pool *pgxpool.Pool }

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

func (store *PostgresStore) Confirm(
	ctx context.Context,
	confirmation application.Confirmation,
	key string,
	requestHash [32]byte,
	audit application.AuditRecord,
	window time.Duration,
) (application.Confirmation, bool, error) {
	if store == nil || store.pool == nil || !validConfirmation(confirmation, audit) ||
		key == "" || key != strings.TrimSpace(key) || len([]rune(key)) > 255 || window <= 0 {
		return application.Confirmation{}, false, application.ErrInvalid
	}
	responseBody, err := json.Marshal(confirmation)
	if err != nil {
		return application.Confirmation{}, false, fmt.Errorf("кодирование подтверждения выручки: %w", err)
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return application.Confirmation{}, false, fmt.Errorf("начало подтверждения выручки: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	created, storedBody, err := reserveRevenueIdempotency(
		ctx, tx, confirmation.Event.TenantID, key, requestHash,
		responseBody, confirmation.Event.ConfirmedAt,
	)
	if err != nil {
		return application.Confirmation{}, false, err
	}
	if !created {
		stored, decodeErr := decodeStoredConfirmation(storedBody, confirmation.Event.TenantID)
		if decodeErr != nil {
			return application.Confirmation{}, false, decodeErr
		}
		if err := tx.Commit(ctx); err != nil {
			return application.Confirmation{}, false, fmt.Errorf("фиксация повтора подтверждения выручки: %w", err)
		}
		return stored, false, nil
	}
	if confirmation.Attribution.Type == domain.AttributionRecovered {
		if err := validateRecoveredChain(ctx, tx, confirmation, window); err != nil {
			return application.Confirmation{}, false, err
		}
	}
	event := confirmation.Event
	inserted, err := tx.Exec(ctx, `
		INSERT INTO revenue_events(
			id, tenant_id, opportunity_id, amount, currency, status, source,
			confirmed_by_user_id, confirmed_at, created_at
		)
		SELECT $1,$2,$3,$4::numeric,$5,$6,$7,$8,$9,$9
		WHERE EXISTS (
			SELECT 1 FROM memberships
			WHERE tenant_id = $2 AND user_id = $8 AND status = 'ACTIVE'
		)`,
		event.ID, event.TenantID, event.OpportunityID, event.Amount.String(),
		event.Currency, event.Status, event.Source, event.ConfirmedBy, event.ConfirmedAt,
	)
	if err != nil {
		return application.Confirmation{}, false, mapRevenueError("запись события выручки", err)
	}
	if inserted.RowsAffected() != 1 {
		return application.Confirmation{}, false, application.ErrForbidden
	}
	attribution := confirmation.Attribution
	if _, err := tx.Exec(ctx, `
		INSERT INTO revenue_attributions(
			id, tenant_id, revenue_event_id, opportunity_id, type,
			risk_id, action_id, outcome_id, created_at
		) VALUES ($1,$2,$3,$4,$5,NULLIF($6,'')::uuid,NULLIF($7,'')::uuid,NULLIF($8,'')::uuid,$9)`,
		attribution.ID, attribution.TenantID, attribution.RevenueEventID,
		attribution.OpportunityID, attribution.Type, attribution.RiskID,
		attribution.ActionID, attribution.OutcomeID, attribution.CreatedAt,
	); err != nil {
		return application.Confirmation{}, false, mapRevenueError("запись атрибуции выручки", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_log(
			id, tenant_id, actor_user_id, operation, entity_type, entity_id, created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		audit.ID, audit.TenantID, audit.ActorID, audit.Operation,
		audit.ResourceType, audit.ResourceID, audit.At,
	); err != nil {
		return application.Confirmation{}, false, mapRevenueError("запись аудита выручки", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return application.Confirmation{}, false, fmt.Errorf("фиксация подтверждения выручки: %w", err)
	}
	return confirmation, true, nil
}

func validateRecoveredChain(
	ctx context.Context,
	tx pgx.Tx,
	confirmation application.Confirmation,
	window time.Duration,
) error {
	event := confirmation.Event
	attribution := confirmation.Attribution
	facts := []struct {
		name  string
		query string
		id    string
		args  []any
	}{
		{"риска", `SELECT opportunity_id, detected_at FROM risk_signals WHERE tenant_id=$1 AND id=$2`, attribution.RiskID, []any{event.TenantID, attribution.RiskID}},
		{"действия", `SELECT risk.opportunity_id, action.created_at FROM actions AS action JOIN risk_signals AS risk ON risk.tenant_id=action.tenant_id AND risk.id=action.risk_id WHERE action.tenant_id=$1 AND action.id=$2 AND action.risk_id=$3`, attribution.ActionID, []any{event.TenantID, attribution.ActionID, attribution.RiskID}},
		{"исхода", `SELECT opportunity_id, created_at FROM outcomes WHERE tenant_id=$1 AND id=$2`, attribution.OutcomeID, []any{event.TenantID, attribution.OutcomeID}},
	}
	for _, fact := range facts {
		if !ids.Valid(fact.id) {
			return application.ErrInvalid
		}
		var opportunityID string
		var factAt time.Time
		err := tx.QueryRow(ctx, fact.query, fact.args...).Scan(&opportunityID, &factAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return application.ErrNotFound
		}
		if err != nil {
			return mapRevenueError("чтение "+fact.name, err)
		}
		age := event.ConfirmedAt.Sub(factAt)
		if opportunityID != event.OpportunityID || age < 0 || age > window {
			return application.ErrInvalid
		}
	}
	return nil
}

func reserveRevenueIdempotency(
	ctx context.Context,
	tx pgx.Tx,
	tenantID, key string,
	requestHash [32]byte,
	responseBody []byte,
	createdAt time.Time,
) (bool, []byte, error) {
	var storedBody []byte
	err := tx.QueryRow(ctx, `
		INSERT INTO idempotency_keys(
			tenant_id, key, operation, request_hash, response_status, response_body, created_at
		) VALUES ($1,$2,$3,$4,201,$5::jsonb,$6)
		ON CONFLICT (tenant_id, key, operation) DO NOTHING
		RETURNING response_body`,
		tenantID, key, revenueOperation, requestHash[:], string(responseBody), createdAt,
	).Scan(&storedBody)
	if err == nil {
		return true, storedBody, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, nil, mapRevenueError("резервирование ключа идемпотентности выручки", err)
	}
	var existingHash []byte
	if err := tx.QueryRow(ctx, `
		SELECT request_hash, response_body
		FROM idempotency_keys
		WHERE tenant_id=$1 AND key=$2 AND operation=$3`,
		tenantID, key, revenueOperation,
	).Scan(&existingHash, &storedBody); err != nil {
		return false, nil, mapRevenueError("чтение ключа идемпотентности выручки", err)
	}
	if !bytes.Equal(existingHash, requestHash[:]) {
		return false, nil, application.ErrConflict
	}
	return false, storedBody, nil
}

func (store *PostgresStore) ConfirmedRecovered(
	ctx context.Context,
	tenantID, currency string,
) (domain.Money, error) {
	if store == nil || store.pool == nil || !ids.Valid(tenantID) || !domain.ValidCurrency(currency) {
		return domain.Money{}, application.ErrInvalid
	}
	var total string
	if err := store.pool.QueryRow(ctx, `
		SELECT COALESCE(sum(event.amount), 0)::numeric(20,2)::text
		FROM revenue_events AS event
		JOIN revenue_attributions AS attribution
		  ON attribution.tenant_id=event.tenant_id
		 AND attribution.revenue_event_id=event.id
		WHERE event.tenant_id=$1 AND event.currency=$2
		  AND event.status='CONFIRMED' AND attribution.type='RECOVERED'`,
		tenantID, currency,
	).Scan(&total); err != nil {
		return domain.Money{}, mapRevenueError("чтение возвращённой выручки", err)
	}
	money, err := domain.ParseNonNegativeMoney(total)
	if err != nil {
		return domain.Money{}, fmt.Errorf("проверка суммы возвращённой выручки: %w", err)
	}
	return money, nil
}

func decodeStoredConfirmation(body []byte, tenantID string) (application.Confirmation, error) {
	var confirmation application.Confirmation
	if err := json.Unmarshal(body, &confirmation); err != nil {
		return application.Confirmation{}, fmt.Errorf("чтение сохранённого подтверждения выручки: %w", err)
	}
	confirmation.Event.TenantID = tenantID
	confirmation.Attribution.TenantID = tenantID
	if !validConfirmationData(confirmation) {
		return application.Confirmation{}, fmt.Errorf("проверка сохранённого подтверждения выручки: %w", domain.ErrInvalid)
	}
	return confirmation, nil
}

func validConfirmation(confirmation application.Confirmation, audit application.AuditRecord) bool {
	return validConfirmationData(confirmation) && ids.Valid(audit.ID) &&
		audit.TenantID == confirmation.Event.TenantID &&
		audit.ActorID == confirmation.Event.ConfirmedBy &&
		audit.Operation == "REVENUE_CONFIRMED" && audit.ResourceType == "REVENUE_EVENT" &&
		audit.ResourceID == confirmation.Event.ID && audit.At.Equal(confirmation.Event.ConfirmedAt)
}

func validConfirmationData(confirmation application.Confirmation) bool {
	event := confirmation.Event
	attribution := confirmation.Attribution
	if !ids.Valid(event.ID) || !ids.Valid(event.TenantID) || !ids.Valid(event.OpportunityID) ||
		!ids.Valid(event.ConfirmedBy) || !ids.Valid(attribution.ID) ||
		event.Status != domain.StatusConfirmed || event.Source != domain.SourceUser {
		return false
	}
	validatedEvent, err := domain.NewConfirmedEvent(
		event.ID, event.TenantID, event.OpportunityID, event.Amount.String(),
		event.Currency, event.ConfirmedBy, event.ConfirmedAt,
	)
	if err != nil || validatedEvent.Status != event.Status || validatedEvent.Source != event.Source {
		return false
	}
	validatedAttribution, err := domain.NewAttribution(
		attribution.ID, event, attribution.Type, attribution.RiskID,
		attribution.ActionID, attribution.OutcomeID, attribution.CreatedAt,
	)
	return err == nil && attribution.TenantID == event.TenantID &&
		attribution.RevenueEventID == event.ID && attribution.OpportunityID == event.OpportunityID &&
		validatedAttribution.CreatedAt.Equal(event.ConfirmedAt)
}

func mapRevenueError(operation string, err error) error {
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) {
		switch postgresError.Code {
		case "22P02", "22001", "22003", "23514":
			return application.ErrInvalid
		case "23503":
			return application.ErrNotFound
		case "23505":
			if postgresError.ConstraintName == "revenue_attributions_one_recovered_per_opportunity_idx" {
				return application.ErrRecoveredAlreadyAttributed
			}
			return application.ErrConflict
		}
	}
	return fmt.Errorf("%s: %w", operation, err)
}

var _ application.Store = (*PostgresStore)(nil)
