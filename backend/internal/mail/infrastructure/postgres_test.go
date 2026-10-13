package infrastructure_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/internal/mail/application"
	"lidradar/backend/internal/mail/domain"
	"lidradar/backend/internal/mail/infrastructure"
	"lidradar/backend/internal/testsupport"
	"lidradar/backend/platform/ids"
)

// Очередь писем на настоящей PostgreSQL (ADR 0058): аренда без двойной выдачи, стирание адреса и текста при
// завершении, результат принимается только от арендатора, ограничения таблицы держат согласованность.

var base = time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)

func newID(t testing.TB) string {
	t.Helper()
	id, err := (ids.Generator{}).NewID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func enqueue(t testing.TB, store *infrastructure.PostgresStore, to string, at time.Time, ttl time.Duration) string {
	t.Helper()
	message, err := domain.NewMessage(newID(t), domain.KindPasswordReset, to, "Сброс пароля", "Ссылка: https://app.example.test/reset-password#token=SECRET", at, at.Add(ttl))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Enqueue(context.Background(), message, at); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	return message.ID
}

type outboxRow struct {
	Status        string
	Attempts      int
	Recipient     *string
	Domain        string
	Body          *string
	Subject       string
	FailureCode   *string
	LeasedBy      *string
	SentAt        *time.Time
	AvailableAt   time.Time
	HasLeaseUntil bool
}

func row(t testing.TB, pool *pgxpool.Pool, id string) outboxRow {
	t.Helper()
	var r outboxRow
	if err := pool.QueryRow(context.Background(), `
		SELECT status, attempts, recipient, recipient_domain, body, subject, failure_code, leased_by, sent_at, available_at, lease_until IS NOT NULL
		FROM mail_outbox WHERE id = $1`, id,
	).Scan(&r.Status, &r.Attempts, &r.Recipient, &r.Domain, &r.Body, &r.Subject, &r.FailureCode, &r.LeasedBy, &r.SentAt, &r.AvailableAt, &r.HasLeaseUntil); err != nil {
		t.Fatalf("read outbox row: %v", err)
	}
	return r
}

func TestEnqueueStoresThePendingMessage(t *testing.T) {
	pool := testsupport.Postgres(t)
	store := infrastructure.NewPostgresStore(pool)
	id := enqueue(t, store, "Owner@Example.com", base, time.Hour)
	got := row(t, pool, id)
	if got.Status != "PENDING" || got.Attempts != 0 || got.Recipient == nil || *got.Recipient != "owner@example.com" ||
		got.Domain != "example.com" || got.Body == nil || got.Subject != "Сброс пароля" || got.LeasedBy != nil || !got.AvailableAt.Equal(base) {
		t.Fatalf("row = %+v", got)
	}
}

func TestClaimTakesDueMessagesOldestFirstAndOnlyOnce(t *testing.T) {
	pool := testsupport.Postgres(t)
	store := infrastructure.NewPostgresStore(pool)
	late := enqueue(t, store, "late@example.com", base.Add(2*time.Minute), time.Hour)
	first := enqueue(t, store, "first@example.com", base, time.Hour)
	second := enqueue(t, store, "second@example.com", base.Add(time.Minute), time.Hour)
	future := enqueue(t, store, "future@example.com", base.Add(time.Hour), 3*time.Hour)
	_ = future
	now := base.Add(3 * time.Minute)

	var order []string
	for {
		delivery, claimed, err := store.Claim(context.Background(), "worker-1", now, now.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if !claimed {
			break
		}
		order = append(order, delivery.ID)
		if delivery.Attempt != 1 || delivery.To == "" || delivery.Text == "" || delivery.Kind != domain.KindPasswordReset {
			t.Fatalf("delivery = %+v", delivery)
		}
	}
	if len(order) != 3 || order[0] != first || order[1] != second || order[2] != late {
		t.Fatalf("claim order = %v, want [%s %s %s] and the future message untouched", order, first, second, late)
	}
	if got := row(t, pool, first); got.Status != "PROCESSING" || got.LeasedBy == nil || *got.LeasedBy != "worker-1" || !got.HasLeaseUntil {
		t.Fatalf("claimed row = %+v", got)
	}
	if got := row(t, pool, future); got.Status != "PENDING" || got.Attempts != 0 {
		t.Fatalf("a message that is not due yet must stay untouched: %+v", got)
	}
}

func TestClaimReclaimsAMessageWhoseLeaseExpired(t *testing.T) {
	pool := testsupport.Postgres(t)
	store := infrastructure.NewPostgresStore(pool)
	id := enqueue(t, store, "owner@example.com", base, time.Hour)
	if _, claimed, err := store.Claim(context.Background(), "crashed-worker", base, base.Add(time.Minute)); err != nil || !claimed {
		t.Fatalf("first claim: %v, %v", claimed, err)
	}
	if _, claimed, _ := store.Claim(context.Background(), "worker-2", base.Add(30*time.Second), base.Add(2*time.Minute)); claimed {
		t.Fatal("a message with a live lease must not be taken")
	}
	delivery, claimed, err := store.Claim(context.Background(), "worker-2", base.Add(time.Minute), base.Add(2*time.Minute))
	if err != nil || !claimed || delivery.ID != id || delivery.Attempt != 2 {
		t.Fatalf("reclaim: %+v, %v, %v", delivery, claimed, err)
	}
	if got := row(t, pool, id); got.LeasedBy == nil || *got.LeasedBy != "worker-2" {
		t.Fatalf("row = %+v", got)
	}
}

func TestParallelWorkersNeverTakeTheSameMessage(t *testing.T) {
	pool := testsupport.Postgres(t)
	store := infrastructure.NewPostgresStore(pool)
	const messages = 24
	want := map[string]bool{}
	for index := 0; index < messages; index++ {
		want[enqueue(t, store, "owner@example.com", base, time.Hour)] = true
	}
	var mu sync.Mutex
	claimed := map[string]int{}
	var group sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		group.Add(1)
		go func(owner string) {
			defer group.Done()
			for {
				delivery, ok, err := store.Claim(context.Background(), owner, base, base.Add(time.Minute))
				if err != nil {
					t.Error(err)
					return
				}
				if !ok {
					return
				}
				mu.Lock()
				claimed[delivery.ID]++
				mu.Unlock()
			}
		}("worker-" + string(rune('a'+worker)))
	}
	group.Wait()
	if len(claimed) != messages {
		t.Fatalf("claimed %d of %d messages", len(claimed), messages)
	}
	for id, times := range claimed {
		if times != 1 || !want[id] {
			t.Fatalf("message %s was claimed %d times", id, times)
		}
	}
}

func claimOne(t testing.TB, store *infrastructure.PostgresStore, owner string, at time.Time) domain.Delivery {
	t.Helper()
	delivery, claimed, err := store.Claim(context.Background(), owner, at, at.Add(time.Minute))
	if err != nil || !claimed {
		t.Fatalf("Claim() = %v, %v", claimed, err)
	}
	return delivery
}

func TestMarkSentErasesTheAddressAndTheLink(t *testing.T) {
	pool := testsupport.Postgres(t)
	store := infrastructure.NewPostgresStore(pool)
	id := enqueue(t, store, "owner@example.com", base, time.Hour)
	claimOne(t, store, "worker-1", base)
	sentAt := base.Add(time.Second)
	if err := store.MarkSent(context.Background(), id, "worker-1", sentAt); err != nil {
		t.Fatalf("MarkSent() error = %v", err)
	}
	got := row(t, pool, id)
	if got.Status != "SENT" || got.Recipient != nil || got.Body != nil || got.LeasedBy != nil || got.HasLeaseUntil ||
		got.SentAt == nil || !got.SentAt.Equal(sentAt) || got.FailureCode != nil {
		t.Fatalf("row = %+v", got)
	}
	// Остаются вид, тема и домен получателя: статистика без персональных данных.
	if got.Domain != "example.com" || got.Subject != "Сброс пароля" {
		t.Fatalf("diagnostics columns lost: %+v", got)
	}
	var leftovers int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM mail_outbox WHERE body LIKE '%SECRET%' OR recipient IS NOT NULL`).Scan(&leftovers); err != nil || leftovers != 0 {
		t.Fatalf("the link or the address is still in the table: %d, %v", leftovers, err)
	}
}

func TestRetryKeepsTheBodyAndWaitsForItsTime(t *testing.T) {
	pool := testsupport.Postgres(t)
	store := infrastructure.NewPostgresStore(pool)
	id := enqueue(t, store, "owner@example.com", base, time.Hour)
	claimOne(t, store, "worker-1", base)
	next := base.Add(30 * time.Second)
	if err := store.Retry(context.Background(), id, "worker-1", base.Add(time.Second), next, domain.CodeSMTPTemporary); err != nil {
		t.Fatalf("Retry() error = %v", err)
	}
	got := row(t, pool, id)
	if got.Status != "PENDING" || got.Body == nil || got.Recipient == nil || got.FailureCode == nil || *got.FailureCode != domain.CodeSMTPTemporary ||
		got.LeasedBy != nil || !got.AvailableAt.Equal(next) || got.Attempts != 1 {
		t.Fatalf("row = %+v", got)
	}
	if _, claimed, _ := store.Claim(context.Background(), "worker-1", base.Add(10*time.Second), base.Add(time.Minute)); claimed {
		t.Fatal("a retried message must wait for its time")
	}
	if delivery := claimOne(t, store, "worker-1", next); delivery.ID != id || delivery.Attempt != 2 {
		t.Fatalf("delivery = %+v", delivery)
	}
	// Успех после повтора стирает прежний код сбоя.
	if err := store.MarkSent(context.Background(), id, "worker-1", next.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := row(t, pool, id); got.FailureCode != nil || got.Status != "SENT" {
		t.Fatalf("row = %+v", got)
	}
}

func TestMarkDeadErasesTheContentAndKeepsTheReason(t *testing.T) {
	pool := testsupport.Postgres(t)
	store := infrastructure.NewPostgresStore(pool)
	id := enqueue(t, store, "owner@example.com", base, time.Hour)
	claimOne(t, store, "worker-1", base)
	if err := store.MarkDead(context.Background(), id, "worker-1", base.Add(time.Second), domain.CodeSMTPPermanent); err != nil {
		t.Fatal(err)
	}
	got := row(t, pool, id)
	if got.Status != "DEAD" || got.Recipient != nil || got.Body != nil || got.FailureCode == nil || *got.FailureCode != domain.CodeSMTPPermanent {
		t.Fatalf("row = %+v", got)
	}
}

func TestResultsAreAcceptedOnlyFromTheLeaseHolder(t *testing.T) {
	pool := testsupport.Postgres(t)
	store := infrastructure.NewPostgresStore(pool)
	id := enqueue(t, store, "owner@example.com", base, time.Hour)
	ctx := context.Background()
	if err := store.MarkSent(ctx, id, "worker-1", base); !errors.Is(err, domain.ErrLeaseLost) {
		t.Fatalf("a message nobody leased must not be completed: %v", err)
	}
	claimOne(t, store, "worker-1", base)
	for name, call := range map[string]func() error{
		"чужой исполнитель — отправлено": func() error { return store.MarkSent(ctx, id, "worker-2", base) },
		"чужой исполнитель — повтор":     func() error { return store.Retry(ctx, id, "worker-2", base, base.Add(time.Minute), "X") },
		"чужой исполнитель — смерть":     func() error { return store.MarkDead(ctx, id, "worker-2", base, "X") },
	} {
		if err := call(); !errors.Is(err, domain.ErrLeaseLost) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Аренду забрал другой исполнитель после истечения: прежний держатель результат записать не может.
	claimOne(t, store, "worker-2", base.Add(2*time.Minute))
	if err := store.MarkSent(ctx, id, "worker-1", base.Add(3*time.Minute)); !errors.Is(err, domain.ErrLeaseLost) {
		t.Fatalf("a stale lease holder must be rejected: %v", err)
	}
	if err := store.MarkSent(ctx, id, "worker-2", base.Add(3*time.Minute)); err != nil {
		t.Fatalf("the current holder must succeed: %v", err)
	}
	if err := store.MarkSent(ctx, id, "worker-2", base.Add(4*time.Minute)); !errors.Is(err, domain.ErrLeaseLost) {
		t.Fatalf("a finished message must not be completed twice: %v", err)
	}
	if got := row(t, pool, id); got.Status != "SENT" || got.Attempts != 2 {
		t.Fatalf("row = %+v", got)
	}
}

func TestOutboxConstraintsRejectInconsistentRows(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	const columns = `id, kind, subject, recipient, recipient_domain, body, status, attempts, available_at, expires_at, leased_by, lease_until, failure_code, created_at, updated_at, sent_at`
	insert := func(overrides map[string]any) error {
		values := map[string]any{
			"kind": "PASSWORD_RESET", "subject": "Тема", "recipient": "a@example.com", "domain": "example.com", "body": "текст",
			"status": "PENDING", "attempts": 0, "expires": base.Add(time.Hour), "leased_by": nil, "lease_until": nil, "failure": nil, "sent_at": nil,
		}
		for key, value := range overrides {
			values[key] = value
		}
		_, err := pool.Exec(ctx, `INSERT INTO mail_outbox(`+columns+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$9,$9,$14)`,
			newID(t), values["kind"], values["subject"], values["recipient"], values["domain"], values["body"], values["status"],
			values["attempts"], base, values["expires"], values["leased_by"], values["lease_until"], values["failure"], values["sent_at"])
		return err
	}
	if err := insert(nil); err != nil {
		t.Fatalf("the baseline row must be valid: %v", err)
	}
	cases := map[string]map[string]any{
		"отправленное письмо хранит текст": {"status": "SENT", "sent_at": base, "recipient": nil},
		"отправленное письмо хранит адрес": {"status": "SENT", "sent_at": base, "body": nil},
		"отправленное без отметки времени": {"status": "SENT", "recipient": nil, "body": nil},
		"ожидающее без текста":             {"body": nil},
		"ожидающее без адреса":             {"recipient": nil},
		"в работе без аренды":              {"status": "PROCESSING"},
		"ожидающее с арендатором":          {"leased_by": "w", "lease_until": base},
		"мёртвое без причины":              {"status": "DEAD", "recipient": nil, "body": nil},
		"мёртвое хранит текст":             {"status": "DEAD", "failure": "X"},
		"тема с переводом строки":          {"subject": "Тема\nBcc: x@y.z"},
		"пустая тема":                      {"subject": ""},
		"адрес в верхнем регистре":         {"recipient": "A@Example.com"},
		"адрес с переводом строки":         {"recipient": "a@example.com\nBcc: x@y.z"},
		"неизвестное состояние":            {"status": "DONE"},
		"вид строчными":                    {"kind": "password_reset"},
		"срок раньше создания":             {"expires": base},
		"код сбоя строчными":               {"failure": "smtp error"},
		"слишком много попыток":            {"attempts": 21},
		"отрицательные попытки":            {"attempts": -1},
		"пустой домен получателя":          {"domain": ""},
		"пустое тело":                      {"body": ""},
	}
	for name, overrides := range cases {
		if err := insert(overrides); err == nil {
			t.Errorf("%s: the row must be rejected by the table", name)
		}
	}
}

func TestStatsCountsEachStateOnce(t *testing.T) {
	pool := testsupport.Postgres(t)
	store := infrastructure.NewPostgresStore(pool)
	ctx := context.Background()
	now := base.Add(time.Hour)

	// Два письма берём и доводим до повтора и до смерти.
	enqueue(t, store, "c@example.com", base, 3*time.Hour)
	enqueue(t, store, "d@example.com", base, 3*time.Hour)
	retried := claimOne(t, store, "w-1", base)
	if err := store.Retry(ctx, retried.ID, "w-1", base, now.Add(time.Hour), domain.CodeSMTPTemporary); err != nil {
		t.Fatal(err)
	}
	dead := claimOne(t, store, "w-1", base)
	if err := store.MarkDead(ctx, dead.ID, "w-1", now, domain.CodeSMTPPermanent); err != nil {
		t.Fatal(err)
	}
	// Письмо, которое взял и бросил упавший исполнитель: аренда истекла.
	enqueue(t, store, "e@example.com", base.Add(time.Minute), 3*time.Hour)
	claimOne(t, store, "crashed", base.Add(time.Minute))
	// Ожидающее давно и ожидающее только что.
	enqueue(t, store, "b@example.com", base.Add(2*time.Minute), 3*time.Hour)
	enqueue(t, store, "a@example.com", now, time.Hour)

	stats, err := store.Stats(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	want := application.Stats{Pending: 2, Retry: 1, Processing: 1, Dead: 1, Overdue: 1, ExpiredLeases: 1}
	if stats != want {
		t.Fatalf("stats = %+v, want %+v", stats, want)
	}
	// Мёртвые старше суток в сводку не попадают.
	later, err := store.Stats(ctx, now.Add(25*time.Hour))
	if err != nil || later.Dead != 0 {
		t.Fatalf("old dead messages must age out of the summary: %+v, %v", later, err)
	}
}
