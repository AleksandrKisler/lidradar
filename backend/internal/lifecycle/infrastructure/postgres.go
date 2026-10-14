// Package infrastructure provides the PostgreSQL store of the lifecycle module: personal export, organization
// snapshot and account anonymization over tables owned by other modules (ADR 0059).
package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/internal/lifecycle/application"
	"lidradar/backend/internal/lifecycle/domain"
)

// Идентификаторы кодов блокировки совпадают со значениями identity.application.Blocker*; тест хранилища сверяет их.
const (
	blockerSoleOwner     = "SOLE_OWNER"
	blockerPlatformAdmin = "PLATFORM_ADMIN"
)

// PostgresStore читает и пишет таблицы других модулей по реестру домена. Платформенный пул нужен там, где данные
// охватывают несколько организаций и идут строго по идентификатору пользователя: проверка блокировок, личная
// выгрузка и обезличивание (список платформенных запросов ADR 0041 дополнен). Выгрузка организации идёт под ролью
// приложения в контексте организации, поэтому RLS остаётся страховкой к условию tenant_id; пул приложения не нужен
// воркеру и операторской команде, и там он может быть nil.
type PostgresStore struct {
	platform *pgxpool.Pool
	app      *pgxpool.Pool
}

func NewPostgresStore(platform, app *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{platform: platform, app: app}
}

// queryer — общая часть пула и транзакции.
type queryer interface {
	Query(ctx context.Context, sql string, arguments ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, arguments ...any) pgx.Row
}

// loadBlockers перечисляет причины, по которым пользователя нельзя закрыть: единственный активный владелец
// организации (ACTIVE или SUSPENDED) и действующий платформенный администратор. Вызывается и вне транзакции, и
// внутри исполнителя под блокировками.
func loadBlockers(ctx context.Context, source queryer, userID string) ([]domain.Blocker, error) {
	rows, err := source.Query(ctx, `
		SELECT o.name,
		       EXISTS (SELECT 1 FROM memberships x
		               WHERE x.tenant_id = m.tenant_id AND x.user_id <> m.user_id AND x.status = 'ACTIVE')
		FROM memberships m
		JOIN organizations o ON o.id = m.tenant_id
		WHERE m.user_id = $1::uuid AND m.role = 'OWNER' AND m.status = 'ACTIVE' AND o.status IN ('ACTIVE', 'SUSPENDED')
		  AND NOT EXISTS (SELECT 1 FROM memberships x
		                  WHERE x.tenant_id = m.tenant_id AND x.user_id <> m.user_id
		                    AND x.role = 'OWNER' AND x.status = 'ACTIVE')
		ORDER BY o.created_at, o.id`, userID)
	if err != nil {
		return nil, fmt.Errorf("find sole owner organizations: %w", err)
	}
	defer rows.Close()
	blockers := make([]domain.Blocker, 0)
	for rows.Next() {
		blocker := domain.Blocker{Code: blockerSoleOwner}
		if err := rows.Scan(&blocker.OrganizationName, &blocker.HasOtherMembers); err != nil {
			return nil, fmt.Errorf("scan sole owner organization: %w", err)
		}
		blockers = append(blockers, blocker)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sole owner organizations: %w", err)
	}
	var admin bool
	if err := source.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM platform_admins WHERE user_id = $1::uuid AND revoked_at IS NULL)`, userID).Scan(&admin); err != nil {
		return nil, fmt.Errorf("check platform admin: %w", err)
	}
	if admin {
		blockers = append(blockers, domain.Blocker{Code: blockerPlatformAdmin})
	}
	return blockers, nil
}

func (s *PostgresStore) DeletionBlockers(ctx context.Context, userID string) ([]domain.Blocker, error) {
	if s == nil || s.platform == nil || userID == "" {
		return nil, domain.ErrInvalid
	}
	return loadBlockers(ctx, s.platform, userID)
}

func (s *PostgresStore) DueDeletions(ctx context.Context, now time.Time, limit int) ([]string, error) {
	if s == nil || s.platform == nil || now.IsZero() || limit < 1 {
		return nil, domain.ErrInvalid
	}
	rows, err := s.platform.Query(ctx, `
		SELECT id::text FROM users
		WHERE deletion_due_at <= $1 AND deleted_at IS NULL
		ORDER BY deletion_due_at, id
		LIMIT $2`, now.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("list due deletions: %w", err)
	}
	defer rows.Close()
	users := make([]string, 0, limit)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan due deletion: %w", err)
		}
		users = append(users, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate due deletions: %w", err)
	}
	return users, nil
}

func (s *PostgresStore) UserIDByEmail(ctx context.Context, email string) (string, bool, error) {
	if s == nil || s.platform == nil || email == "" {
		return "", false, domain.ErrInvalid
	}
	var id string
	err := s.platform.QueryRow(ctx, `SELECT id::text FROM users WHERE email = $1`, email).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("find user by email: %w", err)
	}
	return id, true, nil
}

func (s *PostgresStore) DeletionStats(ctx context.Context, now time.Time) (application.DeletionStats, error) {
	if s == nil || s.platform == nil || now.IsZero() {
		return application.DeletionStats{}, domain.ErrInvalid
	}
	var stats application.DeletionStats
	if err := s.platform.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE deletion_due_at <= $1),
		       count(*) FILTER (WHERE deletion_due_at <= $1::timestamptz - INTERVAL '1 hour')
		FROM users
		WHERE deletion_due_at IS NOT NULL AND deleted_at IS NULL`, now.UTC(),
	).Scan(&stats.Scheduled, &stats.Due, &stats.Overdue); err != nil {
		return application.DeletionStats{}, fmt.Errorf("deletion stats: %w", err)
	}
	return stats, nil
}

var _ application.Store = (*PostgresStore)(nil)
