package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"lidradar/backend/internal/identity/domain"
)

// Команды безопасности учётной записи (ADR 0058). Каждая выполняется одной транзакцией вместе со своей
// записью в auth_audit_log: след изменения либо есть, либо изменения нет. Строка пользователя блокируется
// первой, затем строки токенов: одинаковый порядок исключает взаимную блокировку между выпуском,
// погашением и сменой пароля.

func (r *PostgresRepository) ChangePassword(ctx context.Context, change domain.PasswordChange) error {
	if r == nil || r.pool == nil || change.UserID == "" || change.PreviousHash == "" || change.NewHash == "" ||
		len(change.KeepSessionHash) != 64 || change.At.IsZero() {
		return domain.ErrInvalid
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin password change: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var current string
	err = tx.QueryRow(ctx, `SELECT password_hash FROM users WHERE id = $1 AND status = 'ACTIVE' FOR UPDATE`, change.UserID).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lock user for password change: %w", err)
	}
	// Пароль уже сменили из другого окна: подтверждение прежним паролем устарело.
	if current != change.PreviousHash {
		return domain.ErrConflict
	}
	at := change.At.UTC()
	if _, err := tx.Exec(ctx, `UPDATE users SET password_hash = $2, updated_at = $3 WHERE id = $1`, change.UserID, change.NewHash, at); err != nil {
		return fmt.Errorf("update password: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE sessions SET revoked_at = $2
		WHERE user_id = $1 AND revoked_at IS NULL AND token_hash <> $3`, change.UserID, at, change.KeepSessionHash); err != nil {
		return fmt.Errorf("revoke other sessions: %w", err)
	}
	if err := revokeResetTokens(ctx, tx, change.UserID, at); err != nil {
		return err
	}
	if err := insertAuditRecord(ctx, tx, change.UserID, change.Audit); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit password change: %w", err)
	}
	return nil
}

func (r *PostgresRepository) IssueAccountToken(ctx context.Context, token domain.AccountToken, audit *domain.AuditRecord) error {
	if r == nil || r.pool == nil || token.ID == "" || token.UserID == "" || len(token.TokenHash) != 64 {
		return domain.ErrInvalid
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin token issue: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var locked string
	if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id = $1 FOR UPDATE`, token.UserID).Scan(&locked); errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrNotFound
	} else if err != nil {
		return fmt.Errorf("lock user for token issue: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE account_tokens SET revoked_at = $3
		WHERE user_id = $1 AND purpose = $2 AND used_at IS NULL AND revoked_at IS NULL`,
		token.UserID, string(token.Purpose), token.CreatedAt); err != nil {
		return fmt.Errorf("revoke previous tokens: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO account_tokens(id, user_id, purpose, token_hash, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		token.ID, token.UserID, string(token.Purpose), token.TokenHash, token.CreatedAt, token.ExpiresAt); err != nil {
		return mapPostgresError("insert account token", err)
	}
	if audit != nil {
		if err := insertAuditRecord(ctx, tx, token.UserID, *audit); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit token issue: %w", err)
	}
	return nil
}

func (r *PostgresRepository) AccountTokenUsable(ctx context.Context, purpose domain.TokenPurpose, tokenHash string, at time.Time) (bool, error) {
	if r == nil || r.pool == nil || len(tokenHash) != 64 || at.IsZero() {
		return false, domain.ErrInvalid
	}
	var usable bool
	if err := r.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM account_tokens t
			JOIN users u ON u.id = t.user_id
			WHERE t.token_hash = $1 AND t.purpose = $2
			  AND t.used_at IS NULL AND t.revoked_at IS NULL AND t.expires_at > $3
			  AND u.status = 'ACTIVE'
		)`, tokenHash, string(purpose), at.UTC()).Scan(&usable); err != nil {
		return false, fmt.Errorf("check account token: %w", err)
	}
	return usable, nil
}

func (r *PostgresRepository) CompletePasswordReset(ctx context.Context, reset domain.PasswordReset) (string, error) {
	if r == nil || r.pool == nil || len(reset.TokenHash) != 64 || reset.NewHash == "" || reset.At.IsZero() {
		return "", domain.ErrInvalid
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin password reset: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	at := reset.At.UTC()
	userID, email, tokenID, err := lockUsableToken(ctx, tx, domain.TokenPasswordReset, reset.TokenHash, at)
	if err != nil {
		return "", err
	}
	// Токен дошёл до ящика, поэтому адрес заодно считается подтверждённым.
	if _, err := tx.Exec(ctx, `
		UPDATE users SET password_hash = $2, updated_at = $3, email_verified_at = COALESCE(email_verified_at, $3)
		WHERE id = $1`, userID, reset.NewHash, at); err != nil {
		return "", fmt.Errorf("update password: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE account_tokens SET used_at = $2 WHERE id = $1`, tokenID, at); err != nil {
		return "", fmt.Errorf("consume reset token: %w", err)
	}
	if err := revokeResetTokens(ctx, tx, userID, at); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `UPDATE sessions SET revoked_at = $2 WHERE user_id = $1 AND revoked_at IS NULL`, userID, at); err != nil {
		return "", fmt.Errorf("revoke sessions: %w", err)
	}
	if err := insertAuditRecord(ctx, tx, userID, reset.Audit); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit password reset: %w", err)
	}
	return email, nil
}

func (r *PostgresRepository) CompleteEmailVerification(ctx context.Context, tokenHash string, at time.Time, audit domain.AuditRecord) error {
	if r == nil || r.pool == nil || len(tokenHash) != 64 || at.IsZero() {
		return domain.ErrInvalid
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin email verification: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	at = at.UTC()
	userID, _, tokenID, err := lockUsableToken(ctx, tx, domain.TokenEmailVerification, tokenHash, at)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE account_tokens SET used_at = $2 WHERE id = $1`, tokenID, at); err != nil {
		return fmt.Errorf("consume verification token: %w", err)
	}
	// Уже подтверждённый адрес не меняется и в журнал повторно не пишется: токен при этом всё равно гасится.
	changed, err := tx.Exec(ctx, `
		UPDATE users SET email_verified_at = $2, updated_at = $2
		WHERE id = $1 AND email_verified_at IS NULL`, userID, at)
	if err != nil {
		return fmt.Errorf("verify email: %w", err)
	}
	if changed.RowsAffected() == 1 {
		if err := insertAuditRecord(ctx, tx, userID, audit); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit email verification: %w", err)
	}
	return nil
}

// lockUsableToken находит владельца токена, блокирует его строку, затем строку действующего токена и
// возвращает пользователя, адрес и идентификатор токена. Любой изъян токена — ErrTokenInvalid.
func lockUsableToken(ctx context.Context, tx pgx.Tx, purpose domain.TokenPurpose, tokenHash string, at time.Time) (userID, email, tokenID string, err error) {
	if err = tx.QueryRow(ctx, `SELECT user_id FROM account_tokens WHERE token_hash = $1 AND purpose = $2`,
		tokenHash, string(purpose)).Scan(&userID); errors.Is(err, pgx.ErrNoRows) {
		return "", "", "", domain.ErrTokenInvalid
	} else if err != nil {
		return "", "", "", fmt.Errorf("find account token: %w", err)
	}
	if err = tx.QueryRow(ctx, `SELECT email FROM users WHERE id = $1 AND status = 'ACTIVE' FOR UPDATE`, userID).Scan(&email); errors.Is(err, pgx.ErrNoRows) {
		return "", "", "", domain.ErrTokenInvalid
	} else if err != nil {
		return "", "", "", fmt.Errorf("lock user for token: %w", err)
	}
	if err = tx.QueryRow(ctx, `
		SELECT id FROM account_tokens
		WHERE token_hash = $1 AND purpose = $2 AND used_at IS NULL AND revoked_at IS NULL AND expires_at > $3
		FOR UPDATE`, tokenHash, string(purpose), at).Scan(&tokenID); errors.Is(err, pgx.ErrNoRows) {
		return "", "", "", domain.ErrTokenInvalid
	} else if err != nil {
		return "", "", "", fmt.Errorf("lock account token: %w", err)
	}
	return userID, email, tokenID, nil
}

// revokeResetTokens отзывает все ещё не использованные токены сброса пользователя.
func revokeResetTokens(ctx context.Context, tx pgx.Tx, userID string, at time.Time) error {
	if _, err := tx.Exec(ctx, `
		UPDATE account_tokens SET revoked_at = $2
		WHERE user_id = $1 AND purpose = 'PASSWORD_RESET' AND used_at IS NULL AND revoked_at IS NULL`, userID, at); err != nil {
		return fmt.Errorf("revoke reset tokens: %w", err)
	}
	return nil
}

func insertAuditRecord(ctx context.Context, tx pgx.Tx, userID string, record domain.AuditRecord) error {
	if record.ID == "" || record.Operation == "" || record.At.IsZero() {
		return domain.ErrInvalid
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO auth_audit_log(id, user_id, operation, ip_address, created_at)
		VALUES ($1, $2, $3, NULLIF($4, ''), $5)`,
		record.ID, userID, record.Operation, record.IPAddress, record.At); err != nil {
		return fmt.Errorf("record auth audit: %w", err)
	}
	return nil
}
