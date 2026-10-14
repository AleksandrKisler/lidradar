package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"lidradar/backend/internal/lifecycle/application"
	"lidradar/backend/internal/lifecycle/domain"
	"lidradar/backend/platform/tenantctx"
)

// Снимок организации для выгрузки (ADR 0059): одна транзакция REPEATABLE READ только по чтению под ролью
// приложения в контексте организации. Условие tenant_id стоит в каждом запросе, а RLS остаётся страховкой.
// Колонки берутся только из явного списка реестра; идентификаторы проверяются по строгому шаблону, хотя список
// статичен: запрос собирается строкой.

var identifierPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

type snapshot struct {
	tx       pgx.Tx
	tenantID string
	at       time.Time
}

func (s *PostgresStore) OpenSnapshot(ctx context.Context, tenantID string) (application.Snapshot, error) {
	if s == nil || s.app == nil || tenantID == "" {
		return nil, domain.ErrInvalid
	}
	// Контекст организации применяется пулом при выдаче соединения (ADR 0041).
	tx, err := s.app.BeginTx(tenantctx.WithTenant(ctx, tenantID), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin organization snapshot: %w", err)
	}
	opened := &snapshot{tx: tx, tenantID: tenantID}
	// Времена в файлах всегда в UTC; первый запрос фиксирует и снимок данных, и его момент.
	if _, err := tx.Exec(ctx, `SET LOCAL TIME ZONE 'UTC'`); err != nil {
		_ = tx.Rollback(ctx)
		return nil, fmt.Errorf("set snapshot time zone: %w", err)
	}
	if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&opened.at); err != nil {
		_ = tx.Rollback(ctx)
		return nil, fmt.Errorf("read snapshot time: %w", err)
	}
	return opened, nil
}

func (snap *snapshot) At() time.Time { return snap.at }

func (snap *snapshot) Close(ctx context.Context) error {
	if err := snap.tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		return fmt.Errorf("close organization snapshot: %w", err)
	}
	return nil
}

func (snap *snapshot) Organization(ctx context.Context) ([]byte, error) {
	for _, column := range domain.OrganizationColumns {
		if !identifierPattern.MatchString(column) {
			return nil, domain.ErrInvalid
		}
	}
	var document string
	err := snap.tx.QueryRow(ctx,
		`SELECT to_json(x)::text FROM (SELECT `+strings.Join(domain.OrganizationColumns, ", ")+` FROM organizations WHERE id = $1::uuid) x`,
		snap.tenantID).Scan(&document)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read organization: %w", err)
	}
	return []byte(document), nil
}

// teamQuery — набор команды: членства вместе с адресом и именем из учётной записи. Адрес обезличенной учётной
// записи не выдаётся: он служебный и бессмысленен для читателя.
const teamQuery = `
	SELECT to_json(x)::text FROM (
		SELECT m.id AS membership_id, m.user_id,
		       CASE WHEN u.deleted_at IS NULL THEN u.email END AS email,
		       u.display_name, m.role, m.status, m.created_at, m.updated_at, m.revoked_at
		FROM memberships m JOIN users u ON u.id = m.user_id
		WHERE m.tenant_id = $1::uuid
		ORDER BY m.created_at, m.id
	) x`

func (snap *snapshot) Rows(ctx context.Context, dataset domain.Dataset, emit func([]byte) error) error {
	query, err := datasetQuery(dataset)
	if err != nil {
		return err
	}
	rows, err := snap.tx.Query(ctx, query, snap.tenantID)
	if err != nil {
		return fmt.Errorf("read %s: %w", dataset.File, err)
	}
	defer rows.Close()
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return fmt.Errorf("scan %s: %w", dataset.File, err)
		}
		if err := emit([]byte(line)); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate %s: %w", dataset.File, err)
	}
	return nil
}

// datasetQuery строит запрос набора: to_json сохраняет порядок колонок, заданный реестром.
func datasetQuery(dataset domain.Dataset) (string, error) {
	if dataset.Source == domain.SourceTeam {
		return teamQuery, nil
	}
	if dataset.Source != domain.SourceTable || len(dataset.Columns) == 0 || !identifierPattern.MatchString(dataset.Table) {
		return "", domain.ErrInvalid
	}
	for _, column := range dataset.Columns {
		if !identifierPattern.MatchString(column) || column == domain.TenantColumn() {
			return "", domain.ErrInvalid
		}
	}
	order := strings.Split(dataset.Order(), ",")
	for index, part := range order {
		order[index] = strings.TrimSpace(part)
		if !identifierPattern.MatchString(order[index]) {
			return "", domain.ErrInvalid
		}
	}
	return `SELECT to_json(x)::text FROM (SELECT ` + strings.Join(dataset.Columns, ", ") + ` FROM ` + dataset.Table +
		` WHERE tenant_id = $1::uuid ORDER BY ` + strings.Join(order, ", ") + `) x`, nil
}

var _ application.Snapshot = (*snapshot)(nil)
