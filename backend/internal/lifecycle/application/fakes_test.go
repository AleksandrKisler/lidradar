package application

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	auditdomain "lidradar/backend/internal/audit/domain"
	identityapplication "lidradar/backend/internal/identity/application"
	"lidradar/backend/internal/lifecycle/domain"
)

// Подставные зависимости тестов службы. Порядок вызовов записывается в общий журнал, чтобы проверять, что аудит
// идёт раньше снимка, а снимок раньше первого байта.

type journal struct {
	mu    sync.Mutex
	steps []string
}

func (j *journal) add(step string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.steps = append(j.steps, step)
}

func (j *journal) text() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return strings.Join(j.steps, ",")
}

type fakeSnapshot struct {
	journal   *journal
	at        time.Time
	org       []byte
	orgErr    error
	rows      map[string][]string
	rowsErr   map[string]error
	onRows    func(dataset domain.Dataset) error
	rowsCalls []string
	closed    bool
	deadline  time.Time
}

func (s *fakeSnapshot) At() time.Time { return s.at }

func (s *fakeSnapshot) Organization(context.Context) ([]byte, error) {
	return s.org, s.orgErr
}

func (s *fakeSnapshot) Rows(ctx context.Context, dataset domain.Dataset, emit func([]byte) error) error {
	s.rowsCalls = append(s.rowsCalls, dataset.File)
	if s.onRows != nil {
		if err := s.onRows(dataset); err != nil {
			return err
		}
	}
	if err := s.rowsErr[dataset.File]; err != nil {
		return err
	}
	for _, line := range s.rows[dataset.File] {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := emit([]byte(line)); err != nil {
			return err
		}
	}
	return nil
}

func (s *fakeSnapshot) Close(context.Context) error {
	s.closed = true
	s.journal.add("close")
	return nil
}

type fakeStore struct {
	journal *journal

	blockers    []domain.Blocker
	blockersErr error
	blockersFor []string

	personal    domain.PersonalExport
	personalErr error
	personalFor []string

	snapshot *fakeSnapshot
	openErr  error
	openFor  []string
	openCtx  context.Context

	due      []string
	dueErr   error
	dueLimit int
	results  map[string]domain.ErasureResult
	failures map[string]error
	commands []domain.Erasure

	users map[string]string
	stats DeletionStats
}

func (s *fakeStore) DeletionBlockers(_ context.Context, userID string) ([]domain.Blocker, error) {
	s.blockersFor = append(s.blockersFor, userID)
	return s.blockers, s.blockersErr
}

func (s *fakeStore) PersonalExport(_ context.Context, userID string) (domain.PersonalExport, error) {
	s.personalFor = append(s.personalFor, userID)
	return s.personal, s.personalErr
}

func (s *fakeStore) OpenSnapshot(ctx context.Context, tenantID string) (Snapshot, error) {
	s.journal.add("open")
	s.openFor = append(s.openFor, tenantID)
	s.openCtx = ctx
	if s.openErr != nil {
		return nil, s.openErr
	}
	if deadline, ok := ctx.Deadline(); ok {
		s.snapshot.deadline = deadline
	}
	return s.snapshot, nil
}

func (s *fakeStore) DueDeletions(_ context.Context, _ time.Time, limit int) ([]string, error) {
	s.dueLimit = limit
	return s.due, s.dueErr
}

func (s *fakeStore) Erase(_ context.Context, command domain.Erasure) (domain.ErasureResult, error) {
	s.commands = append(s.commands, command)
	if err := s.failures[command.UserID]; err != nil {
		return domain.ErasureResult{}, err
	}
	if result, ok := s.results[command.UserID]; ok {
		return result, nil
	}
	return domain.ErasureResult{Outcome: domain.ErasureSkipped}, nil
}

func (s *fakeStore) UserIDByEmail(_ context.Context, email string) (string, bool, error) {
	id, ok := s.users[email]
	return id, ok, nil
}

func (s *fakeStore) DeletionStats(context.Context, time.Time) (DeletionStats, error) {
	return s.stats, nil
}

type fakePermissions struct {
	journal *journal
	allowed bool
	err     error
	asked   []string
}

func (p *fakePermissions) Allowed(_ context.Context, actorID, tenantID, permission string) (bool, error) {
	p.journal.add("permission")
	p.asked = append(p.asked, actorID+"|"+tenantID+"|"+permission)
	return p.allowed, p.err
}

type fakeAuditor struct {
	journal *journal
	err     error
	entries []auditdomain.Entry
}

func (a *fakeAuditor) Tenant(_ context.Context, entry auditdomain.Entry) error {
	a.journal.add("audit")
	if a.err != nil {
		return a.err
	}
	a.entries = append(a.entries, entry)
	return nil
}

func (a *fakeAuditor) Auth(context.Context, auditdomain.AuthEntry) error { return nil }

type fakeLimiter struct {
	journal  *journal
	allowed  bool
	err      error
	expires  time.Duration
	taken    []string
	limit    int
	window   time.Duration
	subjects []string
}

func (l *fakeLimiter) Take(_ context.Context, scope, subject string, limit int, window time.Duration, now time.Time) (identityapplication.RateLimitDecision, error) {
	l.journal.add("limit")
	l.taken = append(l.taken, scope)
	l.subjects = append(l.subjects, subject)
	l.limit, l.window = limit, window
	return identityapplication.RateLimitDecision{Allowed: l.allowed, ExpiresAt: now.Add(l.expires)}, l.err
}

type sentMail struct {
	Kind, To, Subject, Text string
	ExpiresAt               time.Time
}

type fakeMailer struct {
	mails []sentMail
	err   error
}

func (m *fakeMailer) Enqueue(_ context.Context, kind, to, subject, text string, expiresAt time.Time) error {
	if m.err != nil {
		return m.err
	}
	m.mails = append(m.mails, sentMail{Kind: kind, To: to, Subject: subject, Text: text, ExpiresAt: expiresAt})
	return nil
}

type fakeIDs struct{ next int }

func (i *fakeIDs) NewID() (string, error) {
	i.next++
	return fmt.Sprintf("id-%d", i.next), nil
}

// fakePasswords запоминает секреты, из которых сделаны хэши: тест проверяет, что секрет случаен и нигде не хранится.
type fakePasswords struct {
	secrets []string
	err     error
}

func (p *fakePasswords) Hash(secret string) (string, error) {
	if p.err != nil {
		return "", p.err
	}
	p.secrets = append(p.secrets, secret)
	return "hash-of-" + secret[:8], nil
}

type fakeReporter struct {
	events []string
	lines  []string
}

func (r *fakeReporter) record(level, message string, arguments ...any) {
	var event string
	for index := 0; index+1 < len(arguments); index += 2 {
		if arguments[index] == "event" {
			event = fmt.Sprint(arguments[index+1])
		}
	}
	r.events = append(r.events, event)
	r.lines = append(r.lines, level+" "+message+" "+fmt.Sprint(arguments...))
}

func (r *fakeReporter) Info(message string, arguments ...any) {
	r.record("info", message, arguments...)
}
func (r *fakeReporter) Warn(message string, arguments ...any) {
	r.record("warn", message, arguments...)
}
func (r *fakeReporter) Error(message string, arguments ...any) {
	r.record("error", message, arguments...)
}
