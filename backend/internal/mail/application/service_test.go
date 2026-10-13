package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"lidradar/backend/internal/mail/domain"
)

// Служба очереди писем на подставном хранилище и транспорте: классификация сбоев, повторы, срок и
// отсутствие адресов и текстов в журнале. Поведение PostgreSQL проверяют тесты хранилища.

type memoryStore struct {
	mu       sync.Mutex
	messages map[string]*storedMail
	order    []string
	// claimErr, markErr — принудительные ошибки хранилища.
	claimErr, markErr error
}

type storedMail struct {
	domain.Message
	status      domain.Status
	attempt     int
	owner       string
	failureCode string
	availableAt time.Time
	scrubbed    bool
}

func newMemoryStore() *memoryStore { return &memoryStore{messages: map[string]*storedMail{}} }

func (s *memoryStore) Enqueue(_ context.Context, message domain.Message, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages[message.ID] = &storedMail{Message: message, status: domain.StatusPending, availableAt: at}
	s.order = append(s.order, message.ID)
	return nil
}

func (s *memoryStore) Claim(_ context.Context, owner string, now, _ time.Time) (domain.Delivery, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claimErr != nil {
		return domain.Delivery{}, false, s.claimErr
	}
	for _, id := range s.order {
		stored := s.messages[id]
		if stored.status == domain.StatusPending && !stored.availableAt.After(now) {
			stored.status, stored.owner = domain.StatusProcessing, owner
			stored.attempt++
			return domain.Delivery{Message: stored.Message, Attempt: stored.attempt}, true, nil
		}
	}
	return domain.Delivery{}, false, nil
}

func (s *memoryStore) leased(id, owner string) (*storedMail, error) {
	stored := s.messages[id]
	if s.markErr != nil {
		return nil, s.markErr
	}
	if stored == nil || stored.status != domain.StatusProcessing || stored.owner != owner {
		return nil, domain.ErrLeaseLost
	}
	return stored, nil
}

func (s *memoryStore) MarkSent(_ context.Context, id, owner string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, err := s.leased(id, owner)
	if err != nil {
		return err
	}
	stored.status, stored.scrubbed, stored.owner = domain.StatusSent, true, ""
	return nil
}

func (s *memoryStore) Retry(_ context.Context, id, owner string, _, availableAt time.Time, code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, err := s.leased(id, owner)
	if err != nil {
		return err
	}
	stored.status, stored.availableAt, stored.failureCode, stored.owner = domain.StatusPending, availableAt, code, ""
	return nil
}

func (s *memoryStore) MarkDead(_ context.Context, id, owner string, _ time.Time, code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, err := s.leased(id, owner)
	if err != nil {
		return err
	}
	stored.status, stored.failureCode, stored.scrubbed, stored.owner = domain.StatusDead, code, true, ""
	return nil
}

func (s *memoryStore) Stats(context.Context, time.Time) (Stats, error) { return Stats{}, nil }

type scriptedTransport struct {
	results []error
	calls   []Outgoing
}

func (t *scriptedTransport) Send(_ context.Context, message Outgoing) error {
	t.calls = append(t.calls, message)
	if len(t.results) == 0 {
		return nil
	}
	result := t.results[0]
	t.results = t.results[1:]
	return result
}

type recordedLog struct{ lines []string }

func (r *recordedLog) Info(message string, arguments ...any) { r.add("info", message, arguments) }
func (r *recordedLog) Warn(message string, arguments ...any) { r.add("warn", message, arguments) }
func (r *recordedLog) add(level, message string, arguments []any) {
	r.lines = append(r.lines, level+" "+message+" "+fmt.Sprint(arguments...))
}

type sequenceIDs struct{ next int }

func (i *sequenceIDs) NewID() (string, error) {
	i.next++
	return fmt.Sprintf("mail-%d", i.next), nil
}

type fixture struct {
	service   Service
	store     *memoryStore
	transport *scriptedTransport
	log       *recordedLog
	now       time.Time
}

func newFixture() *fixture {
	f := &fixture{
		store: newMemoryStore(), transport: &scriptedTransport{}, log: &recordedLog{},
		now: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC),
	}
	f.service = NewService(f.store, f.transport, &sequenceIDs{}, func() time.Time { return f.now }).WithReporter(f.log)
	return f
}

func (f *fixture) enqueue(t *testing.T, ttl time.Duration) string {
	t.Helper()
	if err := f.service.Enqueue(context.Background(), "PASSWORD_RESET", "Owner@Example.com", "Сброс пароля", "Ссылка: https://x#token=SECRET", f.now.Add(ttl)); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	return f.store.order[len(f.store.order)-1]
}

func (f *fixture) dispatch(t *testing.T) bool {
	t.Helper()
	progressed, err := f.service.DispatchOne(context.Background(), "worker-1", time.Minute)
	if err != nil {
		t.Fatalf("DispatchOne() error = %v", err)
	}
	return progressed
}

func TestEnqueueValidatesAndStoresTheNormalizedMessage(t *testing.T) {
	f := newFixture()
	id := f.enqueue(t, time.Hour)
	stored := f.store.messages[id]
	if stored.To != "owner@example.com" || stored.Kind != "PASSWORD_RESET" || !stored.ExpiresAt.Equal(f.now.Add(time.Hour)) {
		t.Fatalf("stored = %#v", stored.Message)
	}
	for name, call := range map[string]func() error{
		"плохой адрес": func() error {
			return f.service.Enqueue(context.Background(), "PASSWORD_RESET", "x", "Тема", "Текст", f.now.Add(time.Hour))
		},
		"плохой вид": func() error {
			return f.service.Enqueue(context.Background(), "bad kind", "a@example.com", "Тема", "Текст", f.now.Add(time.Hour))
		},
		"срок в прошлом": func() error {
			return f.service.Enqueue(context.Background(), "PASSWORD_RESET", "a@example.com", "Тема", "Текст", f.now)
		},
		"заголовок": func() error {
			return f.service.Enqueue(context.Background(), "PASSWORD_RESET", "a@example.com", "Тема\r\nBcc: x@y.z", "Текст", f.now.Add(time.Hour))
		},
	} {
		before := len(f.store.order)
		if err := call(); !errors.Is(err, domain.ErrInvalid) || len(f.store.order) != before {
			t.Errorf("%s: error = %v, queued = %d", name, err, len(f.store.order)-before)
		}
	}
	if err := (Service{}).Enqueue(context.Background(), "PASSWORD_RESET", "a@example.com", "Тема", "Текст", f.now.Add(time.Hour)); !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("an unconfigured service must refuse, error = %v", err)
	}
}

func TestDispatchOneDeliversAndScrubsTheMessage(t *testing.T) {
	f := newFixture()
	id := f.enqueue(t, time.Hour)
	if !f.dispatch(t) {
		t.Fatal("a due message must be taken")
	}
	if len(f.transport.calls) != 1 || f.transport.calls[0].To != "owner@example.com" || !strings.Contains(f.transport.calls[0].Text, "SECRET") {
		t.Fatalf("transport got %#v", f.transport.calls)
	}
	if stored := f.store.messages[id]; stored.status != domain.StatusSent || !stored.scrubbed {
		t.Fatalf("stored = %#v", stored)
	}
	if f.dispatch(t) {
		t.Fatal("a finished message must not be taken again")
	}
}

func TestDispatchOneRetriesTemporaryFailuresWithBackoffAndThenDelivers(t *testing.T) {
	f := newFixture()
	id := f.enqueue(t, time.Hour)
	f.transport.results = []error{
		&domain.SendFailure{Code: domain.CodeSMTPTemporary, Retryable: true, Err: errors.New("451 try later")},
		errors.New("plain error is treated as temporary"),
	}
	f.dispatch(t)
	stored := f.store.messages[id]
	if stored.status != domain.StatusPending || stored.failureCode != domain.CodeSMTPTemporary || !stored.availableAt.Equal(f.now.Add(domain.RetryDelay(1))) {
		t.Fatalf("after the first failure: %#v", stored)
	}
	if f.dispatch(t) {
		t.Fatal("the message must wait for its backoff")
	}
	f.now = f.now.Add(domain.RetryDelay(1))
	f.dispatch(t)
	if stored.status != domain.StatusPending || stored.failureCode != domain.CodeSMTPUnspecified || !stored.availableAt.Equal(f.now.Add(domain.RetryDelay(2))) {
		t.Fatalf("after the second failure: %#v", stored)
	}
	f.now = f.now.Add(domain.RetryDelay(2))
	f.dispatch(t)
	if stored.status != domain.StatusSent || stored.attempt != 3 {
		t.Fatalf("after recovery: %#v", stored)
	}
}

func TestDispatchOneKillsPermanentFailuresWithoutRetry(t *testing.T) {
	f := newFixture()
	id := f.enqueue(t, time.Hour)
	f.transport.results = []error{&domain.SendFailure{Code: domain.CodeSMTPPermanent, Err: errors.New("550 no such user")}}
	f.dispatch(t)
	stored := f.store.messages[id]
	if stored.status != domain.StatusDead || stored.failureCode != domain.CodeSMTPPermanent || !stored.scrubbed || len(f.transport.calls) != 1 {
		t.Fatalf("stored = %#v, calls = %d", stored, len(f.transport.calls))
	}
}

func TestDispatchOneStopsAfterTheAttemptBudget(t *testing.T) {
	f := newFixture()
	id := f.enqueue(t, 24*time.Hour)
	f.transport.results = make([]error, domain.MaxAttempts+2)
	for index := range f.transport.results {
		f.transport.results[index] = &domain.SendFailure{Code: domain.CodeSMTPNetwork, Retryable: true, Err: errors.New("reset")}
	}
	for attempt := 1; attempt <= domain.MaxAttempts; attempt++ {
		f.now = f.now.Add(time.Hour)
		if !f.dispatch(t) {
			t.Fatalf("attempt %d must run", attempt)
		}
	}
	stored := f.store.messages[id]
	if stored.status != domain.StatusDead || stored.failureCode != domain.CodeAttemptsSpent || len(f.transport.calls) != domain.MaxAttempts {
		t.Fatalf("stored = %#v, calls = %d", stored, len(f.transport.calls))
	}
}

func TestDispatchOneNeverSendsAnExpiredMessage(t *testing.T) {
	f := newFixture()
	id := f.enqueue(t, 30*time.Minute)
	f.now = f.now.Add(30 * time.Minute)
	f.dispatch(t)
	if stored := f.store.messages[id]; stored.status != domain.StatusDead || stored.failureCode != domain.CodeExpired || !stored.scrubbed {
		t.Fatalf("stored = %#v", stored)
	}
	if len(f.transport.calls) != 0 {
		t.Fatal("a reset link past its lifetime must not be mailed")
	}
}

func TestDispatchOneDoesNotRetryPastTheLifetimeOfTheLink(t *testing.T) {
	f := newFixture()
	// Срок кончается раньше следующей попытки: ждать нечего, письмо умирает с понятной причиной.
	id := f.enqueue(t, 3*time.Second)
	f.transport.results = []error{&domain.SendFailure{Code: domain.CodeSMTPTemporary, Retryable: true, Err: errors.New("451")}}
	f.dispatch(t)
	if stored := f.store.messages[id]; stored.status != domain.StatusDead || stored.failureCode != domain.CodeExpired {
		t.Fatalf("stored = %#v", stored)
	}
}

func TestDispatchOneDropsTheResultWhenTheLeaseIsLost(t *testing.T) {
	f := newFixture()
	id := f.enqueue(t, time.Hour)
	// Аренду отобрали во время отправки: результат этого исполнителя не записывается и не считается ошибкой.
	f.transport.results = []error{nil}
	original := f.store.messages[id]
	f.service = NewService(&leaseStealingStore{memoryStore: f.store, steal: func() { original.owner = "someone-else" }}, f.transport, &sequenceIDs{}, func() time.Time { return f.now }).WithReporter(f.log)
	if progressed, err := f.service.DispatchOne(context.Background(), "worker-1", time.Minute); !progressed || err != nil {
		t.Fatalf("DispatchOne() = %v, %v: a lost lease is not a worker failure", progressed, err)
	}
	if original.status != domain.StatusProcessing {
		t.Fatalf("the stolen message must be left to its new owner: %#v", original)
	}
	if !strings.Contains(strings.Join(f.log.lines, "\n"), "mail.lease_lost") {
		t.Fatalf("the lost lease must be reported: %v", f.log.lines)
	}
}

type leaseStealingStore struct {
	*memoryStore
	steal func()
}

func (s *leaseStealingStore) Claim(ctx context.Context, owner string, now, until time.Time) (domain.Delivery, bool, error) {
	delivery, claimed, err := s.memoryStore.Claim(ctx, owner, now, until)
	if claimed {
		s.steal()
	}
	return delivery, claimed, err
}

func TestDispatchOneReportsStoreFailures(t *testing.T) {
	f := newFixture()
	f.enqueue(t, time.Hour)
	f.store.claimErr = errors.New("database down")
	if progressed, err := f.service.DispatchOne(context.Background(), "worker-1", time.Minute); progressed || err == nil {
		t.Fatalf("a claim failure must surface: %v, %v", progressed, err)
	}
	f.store.claimErr, f.store.markErr = nil, errors.New("database down")
	if _, err := f.service.DispatchOne(context.Background(), "worker-1", time.Minute); err == nil {
		t.Fatal("a failure to record the result must surface")
	}
	if _, err := (Service{}).DispatchOne(context.Background(), "worker-1", time.Minute); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("an unconfigured service must refuse: %v", err)
	}
	if _, err := f.service.DispatchOne(context.Background(), "", time.Minute); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("an empty owner must be refused: %v", err)
	}
	api := NewService(f.store, nil, &sequenceIDs{}, time.Now)
	if _, err := api.DispatchOne(context.Background(), "worker-1", time.Minute); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("the API side has no transport and must not deliver: %v", err)
	}
}

// Журнал воркера хранится дольше самих писем: в нём не должно быть ни адреса, ни текста со ссылкой.
func TestDeliveryLogsCarryNeitherAddressesNorLinks(t *testing.T) {
	f := newFixture()
	f.enqueue(t, time.Hour)
	f.transport.results = []error{&domain.SendFailure{Code: domain.CodeSMTPTemporary, Retryable: true, Err: errors.New("451 owner@example.com")}, nil}
	f.dispatch(t)
	f.now = f.now.Add(time.Minute)
	f.dispatch(t)
	logs := strings.Join(f.log.lines, "\n")
	for _, secret := range []string{"owner@example.com", "Owner@Example.com", "SECRET", "token="} {
		if strings.Contains(logs, secret) {
			t.Fatalf("the log leaks %q:\n%s", secret, logs)
		}
	}
	for _, event := range []string{"mail.retry", "mail.sent"} {
		if !strings.Contains(logs, event) {
			t.Fatalf("missing %s in:\n%s", event, logs)
		}
	}
}
