package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"lidradar/backend/internal/lifecycle/domain"
)

// Обезличивание одного пользователя одной транзакцией (ADR 0059). Порядок блокировок: строка пользователя,
// его неиспользованные приглашения, затем все членства каждой организации, где у него есть членство, по
// возрастанию tenant_id и id — так же, как держат их команды команды организации (lockMemberships модуля tenant).
// Приём приглашения берёт строку принимающего, затем приглашение, затем его членство; общий порядок «приглашение
// раньше членства» и пропуск занятого пользователя (SKIP LOCKED) исключают ожидание по кругу. lock_timeout не даёт
// транзакции стоять в очереди дольше пяти секунд: пользователь остаётся в очереди исполнителя до следующего прохода.

const (
	operationAnonymized      = "ACCOUNT_ANONYMIZED"
	operationDeletionBlocked = "ACCOUNT_DELETION_BLOCKED"
	operationUserErased      = "USER_ERASED"
)

func (s *PostgresStore) Erase(ctx context.Context, command domain.Erasure) (domain.ErasureResult, error) {
	if s == nil || s.platform == nil {
		return domain.ErasureResult{}, domain.ErrInvalid
	}
	if err := command.Validate(); err != nil {
		return domain.ErasureResult{}, err
	}
	tx, err := s.platform.Begin(ctx)
	if err != nil {
		return domain.ErasureResult{}, fmt.Errorf("begin erasure: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '5s'`); err != nil {
		return domain.ErasureResult{}, fmt.Errorf("set erasure lock timeout: %w", err)
	}

	var (
		email   string
		due     *time.Time
		deleted *time.Time
	)
	err = tx.QueryRow(ctx, `
		SELECT email, deletion_due_at, deleted_at FROM users
		WHERE id = $1::uuid
		FOR UPDATE SKIP LOCKED`, command.UserID).Scan(&email, &due, &deleted)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErasureResult{Outcome: domain.ErasureSkipped}, nil
	}
	if err != nil {
		return domain.ErasureResult{}, fmt.Errorf("lock user for erasure: %w", err)
	}
	if deleted != nil || (command.Mode == domain.ModeDue && (due == nil || due.After(command.At))) {
		return domain.ErasureResult{Outcome: domain.ErasureSkipped}, nil
	}

	if err := lockInvitations(ctx, tx, command.UserID); err != nil {
		return domain.ErasureResult{}, err
	}
	if err := lockOrganizations(ctx, tx, command.UserID); err != nil {
		return domain.ErasureResult{}, err
	}
	blockers, err := loadBlockers(ctx, tx, command.UserID)
	if err != nil {
		return domain.ErasureResult{}, err
	}
	at := command.At.UTC()
	if len(blockers) > 0 {
		blocked := domain.ErasureResult{Outcome: domain.ErasureBlocked, Blockers: blockers, Email: email}
		if command.Mode == domain.ModeImmediate {
			return blocked, nil // откат: оператор ничего не менял
		}
		// Запрос пользователя снимается: он остаётся с действующей учётной записью и узнаёт причину из письма.
		if _, err := tx.Exec(ctx, `
			UPDATE users SET deletion_requested_at = NULL, deletion_due_at = NULL, updated_at = $2
			WHERE id = $1::uuid`, command.UserID, at); err != nil {
			return domain.ErasureResult{}, fmt.Errorf("cancel blocked deletion: %w", err)
		}
		if err := insertAuthAudit(ctx, tx, command.AuditID, command.UserID, operationDeletionBlocked, at); err != nil {
			return domain.ErasureResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return domain.ErasureResult{}, fmt.Errorf("commit blocked deletion: %w", err)
		}
		return blocked, nil
	}

	counts := map[string]int64{}
	run := func(table, statement string, arguments ...any) error {
		tag, err := tx.Exec(ctx, statement, arguments...)
		if err != nil {
			return fmt.Errorf("erase %s: %w", table, err)
		}
		counts[table] += tag.RowsAffected()
		return nil
	}
	// Порядок удаления: сначала то, что ссылается на уведомления и членство, затем сами строки. Доставки
	// уведомлений и команды обратного вызова уходят каскадом вместе с уведомлениями.
	steps := []struct{ table, statement string }{
		{"telegram_callback_commands", `DELETE FROM telegram_callback_commands WHERE user_id = $1::uuid`},
		{"telegram_link_tokens", `DELETE FROM telegram_link_tokens WHERE user_id = $1::uuid`},
		{"telegram_user_links", `DELETE FROM telegram_user_links WHERE user_id = $1::uuid`},
		{"notification_digest_items", `DELETE FROM notification_digest_items WHERE user_id = $1::uuid`},
		{"notification_preferences", `DELETE FROM notification_preferences WHERE user_id = $1::uuid`},
		{"notifications", `DELETE FROM notifications WHERE user_id = $1::uuid`},
		{"sessions", `DELETE FROM sessions WHERE user_id = $1::uuid`},
		{"account_tokens", `DELETE FROM account_tokens WHERE user_id = $1::uuid`},
	}
	for _, step := range steps {
		if err := run(step.table, step.statement, command.UserID); err != nil {
			return domain.ErasureResult{}, err
		}
	}
	// Неиспользованные приглашения пользователя отзываются от его имени: код, выданный закрытой учётной записью,
	// нельзя принять и после обезличивания. Принятые приглашения не меняются.
	if err := run("membership_invitations", `
		UPDATE membership_invitations SET revoked_at = $2, revoked_by_user_id = $1::uuid
		WHERE created_by_user_id = $1::uuid AND accepted_at IS NULL AND revoked_at IS NULL`, command.UserID, at); err != nil {
		return domain.ErasureResult{}, err
	}
	if err := run("memberships", `
		UPDATE memberships SET status = 'DISABLED', revoked_at = $2, updated_at = $2
		WHERE user_id = $1::uuid AND status <> 'DISABLED'`, command.UserID, at); err != nil {
		return domain.ErasureResult{}, err
	}
	if err := run("users", `
		UPDATE users SET email = $3, display_name = $4, password_hash = $5, status = 'DISABLED',
		       email_verified_at = NULL, organization_rights = 0, deleted_at = $2, updated_at = $2
		WHERE id = $1::uuid`,
		command.UserID, at, domain.AnonymizedEmail(command.UserID), domain.AnonymizedDisplayName, command.PasswordHash); err != nil {
		return domain.ErasureResult{}, err
	}
	if err := insertAuthAudit(ctx, tx, command.AuditID, command.UserID, operationAnonymized, at); err != nil {
		return domain.ErasureResult{}, err
	}
	if command.Mode == domain.ModeImmediate {
		if _, err := tx.Exec(ctx, `
			INSERT INTO admin_audit_log(id, actor_user_id, source, operation, entity_type, entity_id, tenant_id, details, created_at)
			VALUES ($1::uuid, NULL, 'CLI', $2, 'USER', $3::uuid, NULL, jsonb_build_object('note', $4::text), $5)`,
			command.AdminAuditID, operationUserErased, command.UserID, command.Note, at); err != nil {
			return domain.ErasureResult{}, fmt.Errorf("record operator erasure: %w", err)
		}
	}
	result := domain.ErasureResult{Outcome: domain.ErasureDone, Email: email, Counts: counts}
	if command.DryRun {
		return result, nil // откат: счётчики те же, что дал бы боевой запуск
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.ErasureResult{}, fmt.Errorf("commit erasure: %w", err)
	}
	return result, nil
}

// lockInvitations блокирует неиспользованные приглашения, выданные пользователем, раньше членств: приём приглашения
// держит приглашение и затем членство принимающего, поэтому обратный порядок дал бы взаимное ожидание.
func lockInvitations(ctx context.Context, tx pgx.Tx, userID string) error {
	rows, err := tx.Query(ctx, `
		SELECT id FROM membership_invitations
		WHERE created_by_user_id = $1::uuid AND accepted_at IS NULL AND revoked_at IS NULL
		ORDER BY tenant_id, id
		FOR UPDATE`, userID)
	if err != nil {
		return fmt.Errorf("lock invitations for erasure: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("lock invitations for erasure: %w", err)
	}
	return nil
}

// lockOrganizations блокирует все членства каждой организации, где у пользователя есть членство: роль в такой
// организации нельзя изменить, пока идёт проверка блокировок и отзыв доступа.
func lockOrganizations(ctx context.Context, tx pgx.Tx, userID string) error {
	rows, err := tx.Query(ctx, `
		SELECT m.id FROM memberships m
		WHERE m.tenant_id IN (SELECT tenant_id FROM memberships WHERE user_id = $1::uuid)
		ORDER BY m.tenant_id, m.id
		FOR UPDATE OF m`, userID)
	if err != nil {
		return fmt.Errorf("lock organizations for erasure: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("lock organizations for erasure: %w", err)
	}
	return nil
}

func insertAuthAudit(ctx context.Context, tx pgx.Tx, id, userID, operation string, at time.Time) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO auth_audit_log(id, user_id, operation, ip_address, created_at)
		VALUES ($1::uuid, $2::uuid, $3, NULL, $4)`, id, userID, operation, at); err != nil {
		return fmt.Errorf("record %s: %w", operation, err)
	}
	return nil
}
