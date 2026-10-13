// Package application coordinates the outbound mail queue: other modules enqueue, the worker delivers (ADR 0058).
package application

import (
	"context"
	"errors"
	"time"

	"lidradar/backend/internal/mail/domain"
)

type IDs interface{ NewID() (string, error) }

// Outgoing — то, что получает транспорт: письмо без служебных полей очереди.
type Outgoing struct {
	ID      string
	To      string
	Subject string
	Text    string
}

// Transport отправляет одно письмо. Отказ возвращается как *domain.SendFailure; любая другая ошибка
// считается временной.
type Transport interface {
	Send(ctx context.Context, message Outgoing) error
}

// Stats — состояние очереди для периодической записи в журнал воркера.
type Stats struct {
	Pending, Retry, Processing int
	// Dead — письма, завершённые отказом за последние сутки.
	Dead int
	// Overdue — ожидающие письма, которые должны были уйти больше пяти минут назад.
	Overdue int
	// ExpiredLeases — письма, арендованные упавшим исполнителем.
	ExpiredLeases int
}

// Store — хранилище очереди. Claim арендует не больше одного письма; Mark* и Retry принимают результат
// только от арендатора и иначе возвращают domain.ErrLeaseLost.
type Store interface {
	Enqueue(ctx context.Context, message domain.Message, at time.Time) error
	Claim(ctx context.Context, owner string, now, leaseUntil time.Time) (domain.Delivery, bool, error)
	MarkSent(ctx context.Context, id, owner string, at time.Time) error
	Retry(ctx context.Context, id, owner string, at, availableAt time.Time, code string) error
	MarkDead(ctx context.Context, id, owner string, at time.Time, code string) error
	Stats(ctx context.Context, now time.Time) (Stats, error)
}

// Reporter получает события доставки без адресов и текстов: идентификатор, вид, номер попытки, код.
type Reporter interface {
	Info(message string, arguments ...any)
	Warn(message string, arguments ...any)
}

type Service struct {
	store     Store
	transport Transport
	ids       IDs
	now       func() time.Time
	reporter  Reporter
}

// NewService собирает службу. Процессу API транспорт не нужен: он только ставит письма в очередь.
func NewService(store Store, transport Transport, ids IDs, now func() time.Time) Service {
	return Service{store: store, transport: transport, ids: ids, now: now}
}

// WithReporter включает запись исходов доставки в журнал.
func (s Service) WithReporter(reporter Reporter) Service {
	s.reporter = reporter
	return s
}

// Enqueue ставит письмо в очередь. Контракт узкий и строковый: так его потребляют другие модули, не
// импортируя типы почты. Ошибка означает, что письмо в очередь не попало.
func (s Service) Enqueue(ctx context.Context, kind, to, subject, text string, expiresAt time.Time) error {
	if s.store == nil || s.ids == nil || s.now == nil {
		return domain.ErrInvalid
	}
	id, err := s.ids.NewID()
	if err != nil {
		return err
	}
	now := s.now().UTC()
	message, err := domain.NewMessage(id, domain.Kind(kind), to, subject, text, now, expiresAt)
	if err != nil {
		return err
	}
	return s.store.Enqueue(ctx, message, now)
}

// DispatchOne арендует и выполняет не больше одной попытки. Возвращает true, если письмо было взято.
// Исход попытки записывается только арендатору: если аренда потеряна, результат отбрасывается, а письмо
// достанется следующей попытке (доставка «хотя бы раз»).
func (s Service) DispatchOne(ctx context.Context, owner string, lease time.Duration) (bool, error) {
	if s.store == nil || s.transport == nil || s.now == nil || owner == "" {
		return false, domain.ErrInvalid
	}
	if lease <= 0 {
		lease = domain.DefaultLease
	}
	now := s.now().UTC()
	delivery, claimed, err := s.store.Claim(ctx, owner, now, now.Add(lease))
	if err != nil || !claimed {
		return false, err
	}
	switch {
	case !now.Before(delivery.ExpiresAt):
		return true, s.finishDead(ctx, owner, delivery, domain.CodeExpired)
	case delivery.Attempt > domain.MaxAttempts:
		return true, s.finishDead(ctx, owner, delivery, domain.CodeAttemptsSpent)
	}

	sendErr := s.transport.Send(ctx, Outgoing{
		ID: delivery.ID, To: delivery.To, Subject: delivery.Subject, Text: delivery.Text,
	})
	finished := s.now().UTC()
	if sendErr == nil {
		if err := s.store.MarkSent(ctx, delivery.ID, owner, finished); err != nil {
			return true, s.lostOrFailed(delivery, err)
		}
		s.info("Письмо отправлено", "event", "mail.sent", "mail_id", delivery.ID, "kind", string(delivery.Kind), "attempt", delivery.Attempt)
		return true, nil
	}

	failure := classify(sendErr)
	next := finished.Add(domain.RetryDelay(delivery.Attempt))
	if !failure.Retryable || delivery.Attempt >= domain.MaxAttempts || !next.Before(delivery.ExpiresAt) {
		code := failure.Code
		if failure.Retryable {
			// Временный сбой, но ждать больше нечего: попытки или срок ссылки исчерпаны.
			code = domain.CodeAttemptsSpent
			if !next.Before(delivery.ExpiresAt) {
				code = domain.CodeExpired
			}
		}
		s.warn("Письмо не доставлено", "event", "mail.failed", "mail_id", delivery.ID, "kind", string(delivery.Kind),
			"attempt", delivery.Attempt, "code", failure.Code, "final", code)
		return true, s.finishDead(ctx, owner, delivery, code)
	}
	if err := s.store.Retry(ctx, delivery.ID, owner, finished, next, failure.Code); err != nil {
		return true, s.lostOrFailed(delivery, err)
	}
	s.warn("Отправка письма не удалась, будет повтор", "event", "mail.retry", "mail_id", delivery.ID, "kind", string(delivery.Kind),
		"attempt", delivery.Attempt, "code", failure.Code)
	return true, nil
}

// Stats возвращает состояние очереди.
func (s Service) Stats(ctx context.Context) (Stats, error) {
	if s.store == nil || s.now == nil {
		return Stats{}, domain.ErrInvalid
	}
	return s.store.Stats(ctx, s.now().UTC())
}

func (s Service) finishDead(ctx context.Context, owner string, delivery domain.Delivery, code string) error {
	if err := s.store.MarkDead(ctx, delivery.ID, owner, s.now().UTC(), code); err != nil {
		return s.lostOrFailed(delivery, err)
	}
	return nil
}

// lostOrFailed: потерянная аренда не ошибка воркера (письмо возьмёт другой исполнитель), остальное — ошибка.
func (s Service) lostOrFailed(delivery domain.Delivery, err error) error {
	if errors.Is(err, domain.ErrLeaseLost) {
		s.warn("Аренда письма потеряна, результат отброшен", "event", "mail.lease_lost", "mail_id", delivery.ID)
		return nil
	}
	return err
}

func classify(err error) *domain.SendFailure {
	var failure *domain.SendFailure
	if errors.As(err, &failure) {
		return failure
	}
	return &domain.SendFailure{Code: domain.CodeSMTPUnspecified, Retryable: true, Err: err}
}

func (s Service) info(message string, arguments ...any) {
	if s.reporter != nil {
		s.reporter.Info(message, arguments...)
	}
}

func (s Service) warn(message string, arguments ...any) {
	if s.reporter != nil {
		s.reporter.Warn(message, arguments...)
	}
}
