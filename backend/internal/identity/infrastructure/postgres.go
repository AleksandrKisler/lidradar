// Package infrastructure provides PostgreSQL and secret-generation adapters for Identity.
package infrastructure

import (
	"context"
	"errors"
	"fmt"
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
	if _, err := tx.Exec(ctx, `
		INSERT INTO users(id, email, password_hash, display_name, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		user.ID, user.Email, user.PasswordHash, user.DisplayName, user.Status, user.CreatedAt, user.UpdatedAt,
	); err != nil {
		return mapPostgresError("insert user", err)
	}
	if err := insertSession(ctx, tx, session); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit identity registration: %w", err)
	}
	return nil
}

func (r *PostgresRepository) UserByEmail(ctx context.Context, email string) (domain.User, bool, error) {
	if r == nil || r.pool == nil {
		return domain.User{}, false, domain.ErrInvalid
	}
	return scanUser(r.pool.QueryRow(ctx, `
		SELECT id, email, password_hash, display_name, status, created_at, updated_at
		FROM users
		WHERE email = $1`, email))
}

func (r *PostgresRepository) UserBySessionHash(ctx context.Context, tokenHash string, at time.Time) (domain.User, bool, error) {
	if r == nil || r.pool == nil {
		return domain.User{}, false, domain.ErrInvalid
	}
	return scanUser(r.pool.QueryRow(ctx, `
		SELECT u.id, u.email, u.password_hash, u.display_name, u.status, u.created_at, u.updated_at
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
		SELECT u.id, u.email, u.password_hash, u.display_name, u.status, u.created_at, u.updated_at
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

func scanUser(row rowScanner) (domain.User, bool, error) {
	var user domain.User
	err := row.Scan(&user.ID, &user.Email, &user.PasswordHash, &user.DisplayName, &user.Status, &user.CreatedAt, &user.UpdatedAt)
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

func insertSession(ctx context.Context, executor sessionExecer, session domain.Session) error {
	_, err := executor.Exec(ctx, `
		INSERT INTO sessions(id, user_id, token_hash, expires_at, ip, user_agent, created_at)
		VALUES ($1, $2, $3, $4, NULLIF($5, '')::inet, NULLIF($6, ''), $7)`,
		session.ID, session.UserID, session.TokenHash, session.ExpiresAt, session.IPAddress, session.UserAgent, session.CreatedAt,
	)
	if err != nil {
		return mapPostgresError("insert session", err)
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
