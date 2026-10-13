// Package infrastructure provides the PostgreSQL outbox and the SMTP and log transports for mail.
package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/internal/mail/application"
	"lidradar/backend/internal/mail/domain"
)

// PostgresStore хранит очередь в таблице mail_outbox. Таблица не принадлежит организации, поэтому подходит
// любая рабочая роль: API ставит письма, воркер доставляет.
type PostgresStore struct{ pool *pgxpool.Pool }

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

func (s *PostgresStore) Enqueue(ctx context.Context, message domain.Message, at time.Time) error {
	if s == nil || s.pool == nil || at.IsZero() {
		return domain.ErrInvalid
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO mail_outbox(
			id, kind, subject, recipient, recipient_domain, body, status, attempts,
			available_at, expires_at, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, 'PENDING', 0, $7, $8, $7, $7)`,
		message.ID, string(message.Kind), message.Subject, message.To, message.RecipientDomain(), message.Text,
		at.UTC(), message.ExpiresAt,
	); err != nil {
		return fmt.Errorf("enqueue mail: %w", err)
	}
	return nil
}

// Claim арендует самое раннее готовое письмо: ожидающее, срок которого наступил, или арендованное
// исполнителем, чья аренда истекла. SKIP LOCKED даёт нескольким воркерам брать разные письма.
func (s *PostgresStore) Claim(ctx context.Context, owner string, now, leaseUntil time.Time) (domain.Delivery, bool, error) {
	if s == nil || s.pool == nil || owner == "" || !leaseUntil.After(now) {
		return domain.Delivery{}, false, domain.ErrInvalid
	}
	var delivery domain.Delivery
	var kind string
	err := s.pool.QueryRow(ctx, `
		WITH due AS (
			SELECT id FROM mail_outbox
			WHERE (status = 'PENDING' AND available_at <= $2)
			   OR (status = 'PROCESSING' AND lease_until <= $2)
			ORDER BY available_at, id
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		UPDATE mail_outbox AS mail
		SET status = 'PROCESSING', leased_by = $1, lease_until = $3, attempts = mail.attempts + 1, updated_at = $2
		FROM due
		WHERE mail.id = due.id
		RETURNING mail.id, mail.kind, mail.recipient, mail.subject, mail.body, mail.attempts, mail.created_at, mail.expires_at`,
		owner, now.UTC(), leaseUntil.UTC(),
	).Scan(&delivery.ID, &kind, &delivery.To, &delivery.Subject, &delivery.Text, &delivery.Attempt, &delivery.CreatedAt, &delivery.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Delivery{}, false, nil
	}
	if err != nil {
		return domain.Delivery{}, false, fmt.Errorf("claim mail: %w", err)
	}
	delivery.Kind = domain.Kind(kind)
	return delivery, true, nil
}

// MarkSent завершает письмо и стирает адрес и текст: ссылка с токеном не остаётся в базе.
func (s *PostgresStore) MarkSent(ctx context.Context, id, owner string, at time.Time) error {
	return s.finish(ctx, "mark mail sent", `
		UPDATE mail_outbox
		SET status = 'SENT', recipient = NULL, body = NULL, leased_by = NULL, lease_until = NULL,
		    failure_code = NULL, sent_at = $3, updated_at = $3
		WHERE id = $1 AND status = 'PROCESSING' AND leased_by = $2`, id, owner, at.UTC())
}

// Retry возвращает письмо в очередь с отложенным временем; текст остаётся до следующей попытки.
func (s *PostgresStore) Retry(ctx context.Context, id, owner string, at, availableAt time.Time, code string) error {
	return s.finish(ctx, "retry mail", `
		UPDATE mail_outbox
		SET status = 'PENDING', leased_by = NULL, lease_until = NULL, available_at = $4, failure_code = $5, updated_at = $3
		WHERE id = $1 AND status = 'PROCESSING' AND leased_by = $2`, id, owner, at.UTC(), availableAt.UTC(), code)
}

// MarkDead завершает письмо отказом и тоже стирает адрес и текст.
func (s *PostgresStore) MarkDead(ctx context.Context, id, owner string, at time.Time, code string) error {
	return s.finish(ctx, "mark mail dead", `
		UPDATE mail_outbox
		SET status = 'DEAD', recipient = NULL, body = NULL, leased_by = NULL, lease_until = NULL,
		    failure_code = $4, updated_at = $3
		WHERE id = $1 AND status = 'PROCESSING' AND leased_by = $2`, id, owner, at.UTC(), code)
}

func (s *PostgresStore) finish(ctx context.Context, operation, query string, arguments ...any) error {
	if s == nil || s.pool == nil {
		return domain.ErrInvalid
	}
	command, err := s.pool.Exec(ctx, query, arguments...)
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	if command.RowsAffected() != 1 {
		return domain.ErrLeaseLost
	}
	return nil
}

func (s *PostgresStore) Stats(ctx context.Context, now time.Time) (application.Stats, error) {
	if s == nil || s.pool == nil || now.IsZero() {
		return application.Stats{}, domain.ErrInvalid
	}
	var stats application.Stats
	if err := s.pool.QueryRow(ctx, `
		SELECT
			count(*) FILTER (WHERE status = 'PENDING' AND attempts = 0),
			count(*) FILTER (WHERE status = 'PENDING' AND attempts > 0),
			count(*) FILTER (WHERE status = 'PROCESSING'),
			count(*) FILTER (WHERE status = 'DEAD' AND updated_at > $1::timestamptz - INTERVAL '24 hours'),
			count(*) FILTER (WHERE status = 'PENDING' AND available_at < $1::timestamptz - INTERVAL '5 minutes'),
			count(*) FILTER (WHERE status = 'PROCESSING' AND lease_until < $1::timestamptz)
		FROM mail_outbox`, now.UTC(),
	).Scan(&stats.Pending, &stats.Retry, &stats.Processing, &stats.Dead, &stats.Overdue, &stats.ExpiredLeases); err != nil {
		return application.Stats{}, fmt.Errorf("mail queue stats: %w", err)
	}
	return stats, nil
}

var _ application.Store = (*PostgresStore)(nil)
