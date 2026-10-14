// Package infrastructure provides PostgreSQL and secret-generation adapters for Identity.
package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/internal/identity/domain"
)

type PostgresRepository struct{ pool *pgxpool.Pool }

func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

func (r *PostgresRepository) CreateUserSession(ctx context.Context, user domain.User, session domain.Session) error {
	if r == nil || r.pool == nil || user.ID != session.UserID {
		return domain.ErrInvalid
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin identity registration: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := insertUserSession(ctx, tx, user, session); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit identity registration: %w", err)
	}
	return nil
}

// CreateUserSessionWithPlatformInvitation гасит код раньше, чем вставляет пользователя: недействительный код
// отвечает ErrInvitationInvalid, не касаясь почты, поэтому по ответу нельзя узнать, занят ли адрес (ADR 0057).
func (r *PostgresRepository) CreateUserSessionWithPlatformInvitation(ctx context.Context, user domain.User, session domain.Session, codeHash string, at time.Time) error {
	if r == nil || r.pool == nil || user.ID != session.UserID || len(codeHash) != 64 || at.IsZero() {
		return domain.ErrInvalid
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin invited registration: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Строка кода блокируется до вставки пользователя: недействительный код отвечает ErrInvitationInvalid,
	// не касаясь почты, поэтому по ответу нельзя узнать, занят ли адрес (ADR 0057). Блокировка же не даёт
	// двум регистрациям погасить один код.
	var invitationID string
	err = tx.QueryRow(ctx, `
		SELECT id FROM platform_invitations
		WHERE code_hash = $1 AND consumed_at IS NULL AND revoked_at IS NULL AND expires_at > $2
		FOR UPDATE`, codeHash, at.UTC()).Scan(&invitationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrInvitationInvalid
	}
	if err != nil {
		return fmt.Errorf("lock platform invitation: %w", err)
	}
	if err := insertUserSession(ctx, tx, user, session); err != nil {
		return err
	}
	command, err := tx.Exec(ctx, `
		UPDATE platform_invitations SET consumed_at = $2, consumed_by_user_id = $3
		WHERE id = $1 AND consumed_at IS NULL AND revoked_at IS NULL`,
		invitationID, at.UTC(), user.ID)
	if err != nil {
		return mapPostgresError("redeem platform invitation", err)
	}
	if command.RowsAffected() != 1 {
		return domain.ErrInvitationInvalid
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit invited registration: %w", err)
	}
	return nil
}

func (r *PostgresRepository) PlatformInvitationUsable(ctx context.Context, codeHash string, at time.Time) (bool, error) {
	if r == nil || r.pool == nil || len(codeHash) != 64 || at.IsZero() {
		return false, domain.ErrInvalid
	}
	var usable bool
	if err := r.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM platform_invitations
			WHERE code_hash = $1 AND consumed_at IS NULL AND revoked_at IS NULL AND expires_at > $2
		)`, codeHash, at.UTC()).Scan(&usable); err != nil {
		return false, fmt.Errorf("check platform invitation: %w", err)
	}
	return usable, nil
}

func (r *PostgresRepository) DeleteNewUser(ctx context.Context, userID string) error {
	if r == nil || r.pool == nil || userID == "" {
		return domain.ErrInvalid
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin new user removal: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Удаляется только пользователь без следов; сессии уходят каскадом. С членством, записью журнала входа
	// или погашенным кодом платформы строка остаётся: это ошибка, а не молчаливая потеря данных.
	removed, err := tx.Exec(ctx, `
		DELETE FROM users
		WHERE id = $1
		  AND NOT EXISTS (SELECT 1 FROM memberships WHERE user_id = $1)
		  AND NOT EXISTS (SELECT 1 FROM auth_audit_log WHERE user_id = $1)
		  AND NOT EXISTS (SELECT 1 FROM platform_invitations WHERE consumed_by_user_id = $1)`, userID)
	if err != nil {
		return fmt.Errorf("remove new user: %w", err)
	}
	if removed.RowsAffected() == 0 {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1)`, userID).Scan(&exists); err != nil {
			return fmt.Errorf("check new user: %w", err)
		}
		if exists {
			return domain.ErrConflict
		}
		return nil // уже удалён: повтор компенсации безопасен
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit new user removal: %w", err)
	}
	return nil
}

func (r *PostgresRepository) UserByEmail(ctx context.Context, email string) (domain.User, bool, error) {
	if r == nil || r.pool == nil {
		return domain.User{}, false, domain.ErrInvalid
	}
	return scanUser(r.pool.QueryRow(ctx, `
		SELECT `+userColumns("")+`
		FROM users
		WHERE email = $1`, email))
}

func (r *PostgresRepository) UserBySessionHash(ctx context.Context, tokenHash string, at time.Time) (domain.User, bool, error) {
	if r == nil || r.pool == nil {
		return domain.User{}, false, domain.ErrInvalid
	}
	return scanUser(r.pool.QueryRow(ctx, `
		SELECT `+userColumns("u.")+`
		FROM sessions s
		JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1
		  AND s.revoked_at IS NULL
		  AND s.expires_at > $2`, tokenHash, at))
}

func (r *PostgresRepository) CreateSession(ctx context.Context, session domain.Session) error {
	if r == nil || r.pool == nil {
		return domain.ErrInvalid
	}
	return insertSession(ctx, r.pool, session)
}

func (r *PostgresRepository) RotateSession(ctx context.Context, currentHash string, replacement domain.Session, at time.Time) (domain.User, bool, error) {
	if r == nil || r.pool == nil {
		return domain.User{}, false, domain.ErrInvalid
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return domain.User{}, false, fmt.Errorf("begin session rotation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	user, found, err := scanUser(tx.QueryRow(ctx, `
		SELECT `+userColumns("u.")+`
		FROM sessions s
		JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1
		  AND s.revoked_at IS NULL
		  AND s.expires_at > $2
		  AND u.status = 'ACTIVE'
		FOR UPDATE OF s`, currentHash, at))
	if err != nil || !found {
		return domain.User{}, found, err
	}
	if replacement.UserID != user.ID {
		return domain.User{}, false, domain.ErrInvalid
	}
	command, err := tx.Exec(ctx, `
		UPDATE sessions SET revoked_at = $2
		WHERE token_hash = $1 AND revoked_at IS NULL`, currentHash, at)
	if err != nil {
		return domain.User{}, false, fmt.Errorf("revoke rotated session: %w", err)
	}
	if command.RowsAffected() != 1 {
		return domain.User{}, false, nil
	}
	if err := insertSession(ctx, tx, replacement); err != nil {
		return domain.User{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.User{}, false, fmt.Errorf("commit session rotation: %w", err)
	}
	return user, true, nil
}

func (r *PostgresRepository) RevokeSession(ctx context.Context, tokenHash string, at time.Time) error {
	if r == nil || r.pool == nil {
		return domain.ErrInvalid
	}
	if _, err := r.pool.Exec(ctx, `
		UPDATE sessions SET revoked_at = $2
		WHERE token_hash = $1 AND revoked_at IS NULL`, tokenHash, at); err != nil {
		return fmt.Errorf("revoke session: %w", err)
	}
	return nil
}

type rowScanner interface{ Scan(...any) error }

// userColumns перечисляет колонки пользователя в порядке scanUser; prefix — псевдоним таблицы с точкой.
func userColumns(prefix string) string {
	columns := []string{
		"id", "email", "password_hash", "display_name", "status", "created_at", "updated_at",
		"organization_rights", "email_verified_at", "deletion_requested_at", "deletion_due_at",
	}
	for index, column := range columns {
		columns[index] = prefix + column
	}
	return strings.Join(columns, ", ")
}

func scanUser(row rowScanner) (domain.User, bool, error) {
	var user domain.User
	err := row.Scan(&user.ID, &user.Email, &user.PasswordHash, &user.DisplayName, &user.Status, &user.CreatedAt, &user.UpdatedAt,
		&user.OrganizationRights, &user.EmailVerifiedAt, &user.DeletionRequestedAt, &user.DeletionDueAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.User{}, false, nil
	}
	if err != nil {
		return domain.User{}, false, fmt.Errorf("scan user: %w", err)
	}
	return user, true, nil
}

type sessionExecer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func insertUserSession(ctx context.Context, executor sessionExecer, user domain.User, session domain.Session) error {
	if _, err := executor.Exec(ctx, `
		INSERT INTO users(id, email, password_hash, display_name, status, created_at, updated_at, organization_rights)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		user.ID, user.Email, user.PasswordHash, user.DisplayName, user.Status, user.CreatedAt, user.UpdatedAt, user.OrganizationRights,
	); err != nil {
		return mapPostgresError("insert user", err)
	}
	return insertSession(ctx, executor, session)
}

// insertSession создаёт сессию только для действующего пользователя. Блокировка FOR SHARE строки пользователя
// сериализует вставку с обезличиванием (ADR 0059), которое держит эту строку FOR UPDATE: вход, начавшийся до
// закрытия учётной записи, не оставит сессию с IP и user agent после него. Отключённый пользователь — ErrNotFound.
func insertSession(ctx context.Context, executor sessionExecer, session domain.Session) error {
	command, err := executor.Exec(ctx, `
		INSERT INTO sessions(id, user_id, token_hash, expires_at, ip, user_agent, created_at)
		SELECT $1::uuid, $2::uuid, $3, $4::timestamptz, NULLIF($5, '')::inet, NULLIF($6, ''), $7::timestamptz
		WHERE EXISTS (SELECT 1 FROM users WHERE id = $2::uuid AND status = 'ACTIVE' FOR SHARE)`,
		session.ID, session.UserID, session.TokenHash, session.ExpiresAt, session.IPAddress, session.UserAgent, session.CreatedAt,
	)
	if err != nil {
		return mapPostgresError("insert session", err)
	}
	if command.RowsAffected() != 1 {
		return domain.ErrNotFound
	}
	return nil
}

func mapPostgresError(operation string, err error) error {
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) && postgresError.Code == "23505" {
		return domain.ErrConflict
	}
	return fmt.Errorf("%s: %w", operation, err)
}
