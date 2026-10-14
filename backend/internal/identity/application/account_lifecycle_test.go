package application

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	auditdomain "lidradar/backend/internal/audit/domain"
	"lidradar/backend/internal/identity/domain"
)

// Тесты команд выгрузки и закрытия учётной записи (ADR 0059) на памяти. Поведение PostgreSQL проверяют тесты
// хранилища, данные и исполнение обезличивания — тесты модуля lifecycle.

func (r *memoryRepository) ScheduleDeletion(_ context.Context, schedule domain.DeletionSchedule) (time.Time, bool, error) {
	email, user, ok := r.userByID(schedule.UserID)
	if !ok || user.Status != domain.UserActive {
		return time.Time{}, false, domain.ErrNotFound
	}
	if user.DeletionDueAt != nil {
		return *user.DeletionDueAt, false, nil
	}
	requested, due := schedule.At, schedule.Due
	user.DeletionRequestedAt, user.DeletionDueAt = &requested, &due
	r.users[email] = user
	r.audits = append(r.audits, memoryAudit{UserID: user.ID, AuditRecord: schedule.Audit})
	return due, true, nil
}

func (r *memoryRepository) CancelDeletion(_ context.Context, userID string, _ time.Time, audit domain.AuditRecord) (bool, error) {
	email, user, ok := r.userByID(userID)
	if !ok || user.Status != domain.UserActive {
		return false, domain.ErrNotFound
	}
	if user.DeletionDueAt == nil {
		return false, nil
	}
	user.DeletionRequestedAt, user.DeletionDueAt = nil, nil
	r.users[email] = user
	r.audits = append(r.audits, memoryAudit{UserID: user.ID, AuditRecord: audit})
	return true, nil
}

// fakeAccountData подменяет модуль lifecycle: возвращает заданные блокировки и документ и запоминает порядок
// вызовов вместе с аудитом.
type fakeAccountData struct {
	blockers []DeletionBlocker
	err      error
	document []byte
	calls    *[]string
	exported []string
}

func (d *fakeAccountData) DeletionBlockers(_ context.Context, userID string) ([]DeletionBlocker, error) {
	d.record("blockers:" + userID)
	return d.blockers, d.err
}

func (d *fakeAccountData) PersonalExport(_ context.Context, userID string) ([]byte, error) {
	d.record("export:" + userID)
	if d.err != nil {
		return nil, d.err
	}
	d.exported = append(d.exported, userID)
	return d.document, nil
}

func (d *fakeAccountData) record(call string) {
	if d.calls != nil {
		*d.calls = append(*d.calls, call)
	}
}

// recordingAuditor запоминает операции входа и безопасности в порядке появления.
type recordingAuditor struct {
	calls *[]string
	err   error
	users []string
}

func (a *recordingAuditor) Tenant(context.Context, auditdomain.Entry) error { return nil }

func (a *recordingAuditor) Auth(_ context.Context, entry auditdomain.AuthEntry) error {
	if a.err != nil {
		return a.err
	}
	if a.calls != nil {
		*a.calls = append(*a.calls, "audit:"+entry.Operation)
	}
	a.users = append(a.users, entry.UserID)
	return nil
}

func (f *accountFixture) withAccountData(data AccountData) {
	f.service = f.service.WithAccountData(data)
}

func TestRequestAccountDeletionSchedulesAfterTheGracePeriodAndNotifies(t *testing.T) {
	f := newAccountFixture()
	f.withAccountData(&fakeAccountData{})
	registered := f.register(t, "owner@example.com")
	f.mailer.mails = nil

	due, err := f.service.RequestAccountDeletion(context.Background(), registered.Token, accountPass, Client{IPAddress: "192.0.2.7"})
	if err != nil {
		t.Fatalf("RequestAccountDeletion() error = %v", err)
	}
	if want := f.now.Add(30 * 24 * time.Hour); !due.Equal(want) {
		t.Fatalf("due = %s, want %s", due, want)
	}
	stored := f.repo.users["owner@example.com"]
	if stored.DeletionDueAt == nil || !stored.DeletionDueAt.Equal(due) || stored.DeletionRequestedAt == nil || !stored.DeletionRequestedAt.Equal(f.now) {
		t.Fatalf("stored schedule = %v / %v", stored.DeletionRequestedAt, stored.DeletionDueAt)
	}
	if got := f.repo.auditOperations(); len(got) != 1 || got[0] != "ACCOUNT_DELETION_REQUESTED" {
		t.Fatalf("audit = %v", got)
	}
	if f.repo.audits[0].IPAddress != "192.0.2.7" {
		t.Fatalf("audit address = %q", f.repo.audits[0].IPAddress)
	}
	mail := f.mailer.last(t)
	if mail.Kind != "ACCOUNT_DELETION_REQUESTED" || mail.To != "owner@example.com" {
		t.Fatalf("mail = %#v", mail)
	}
	for _, want := range []string{"12.11.2026 09:00 UTC", accountBaseURL + "/settings/account", accountBaseURL + "/forgot-password"} {
		if !strings.Contains(mail.Text, want) {
			t.Fatalf("mail text lacks %q: %q", want, mail.Text)
		}
	}
	// До срока учётная запись работает как обычно.
	if _, err := f.login(t, "owner@example.com", accountPass); err != nil {
		t.Fatalf("login during the grace period error = %v", err)
	}
	if user, err := f.service.Authenticate(context.Background(), registered.Token); err != nil || user.DeletionDueAt == nil {
		t.Fatalf("Authenticate() = %v, %v", user.DeletionDueAt, err)
	}
}

func TestRequestAccountDeletionIsIdempotentAndKeepsTheOriginalDue(t *testing.T) {
	f := newAccountFixture()
	f.withAccountData(&fakeAccountData{})
	registered := f.register(t, "owner@example.com")
	first, err := f.service.RequestAccountDeletion(context.Background(), registered.Token, accountPass, Client{})
	if err != nil {
		t.Fatal(err)
	}
	mails := len(f.mailer.mails)

	f.now = f.now.Add(5 * 24 * time.Hour)
	again, err := f.login(t, "owner@example.com", accountPass) // прежняя сессия за пять суток истекла
	if err != nil {
		t.Fatal(err)
	}
	// Вход пишет собственную запись в журнал; считаются только записи самого повторного запроса.
	audits := len(f.repo.audits)
	second, err := f.service.RequestAccountDeletion(context.Background(), again.Token, accountPass, Client{})
	if err != nil {
		t.Fatalf("repeated RequestAccountDeletion() error = %v", err)
	}
	if !second.Equal(first) {
		t.Fatalf("repeat moved the due date from %s to %s", first, second)
	}
	if len(f.mailer.mails) != mails || len(f.repo.audits) != audits {
		t.Fatalf("repeat sent mail or wrote audit: mails %d→%d audits %d→%d", mails, len(f.mailer.mails), audits, len(f.repo.audits))
	}
}

// Назначенное закрытие уже проверено: повтор того же запроса возвращает прежний срок и не спрашивает блокировки
// заново. Если за это время пользователь стал единственным владельцем, исполнение сработает в срок само: снимет
// назначение и напишет письмо (ADR 0059, п. 5), а не повтор запроса отвечает отказом.
func TestRequestAccountDeletionOfAnAlreadyScheduledUserDoesNotAskForBlockersAgain(t *testing.T) {
	f := newAccountFixture()
	var calls []string
	data := &fakeAccountData{calls: &calls}
	f.withAccountData(data)
	registered := f.register(t, "owner@example.com")
	first, err := f.service.RequestAccountDeletion(context.Background(), registered.Token, accountPass, Client{})
	if err != nil {
		t.Fatal(err)
	}

	data.blockers = []DeletionBlocker{{Code: BlockerSoleOwner, OrganizationName: "Салон"}}
	calls = nil
	second, err := f.service.RequestAccountDeletion(context.Background(), registered.Token, accountPass, Client{})
	if err != nil {
		t.Fatalf("повторный запрос стал отказом: %v", err)
	}
	if !second.Equal(first) {
		t.Fatalf("срок сдвинулся: %s → %s", first, second)
	}
	if len(calls) != 0 {
		t.Errorf("повторный запрос обращался к данным: %v", calls)
	}
}

func TestRequestAccountDeletionRaceKeepsTheFirstSchedule(t *testing.T) {
	// Два окна нажали «удалить» одновременно: сервис видел пользователя без назначения, а хранилище уже держит срок.
	f := newAccountFixture()
	f.withAccountData(&fakeAccountData{})
	registered := f.register(t, "owner@example.com")
	f.mailer.mails = nil
	earlier := f.now.Add(-24 * time.Hour).Add(30 * 24 * time.Hour)
	f.service = f.service.withRepository(racingRepository{memoryRepository: f.repo, existing: earlier})

	due, err := f.service.RequestAccountDeletion(context.Background(), registered.Token, accountPass, Client{})
	if err != nil {
		t.Fatal(err)
	}
	if !due.Equal(earlier) {
		t.Fatalf("due = %s, want the already stored %s", due, earlier)
	}
	if len(f.mailer.mails) != 0 {
		t.Fatalf("the losing request sent mail: %v", f.mailer.mails)
	}
}

// racingRepository моделирует назначение, сделанное другим запросом между чтением пользователя и записью.
type racingRepository struct {
	*memoryRepository
	existing time.Time
}

func (r racingRepository) ScheduleDeletion(context.Context, domain.DeletionSchedule) (time.Time, bool, error) {
	return r.existing, false, nil
}

func (s Service) withRepository(repository domain.Repository) Service {
	s.repository = repository
	return s
}

func TestRequestAccountDeletionRejectsAWrongPasswordAndSharesTheGuessLimiter(t *testing.T) {
	f := newAccountFixture()
	f.withAccountData(&fakeAccountData{document: []byte(`{}`)})
	registered := f.register(t, "owner@example.com")
	ctx := context.Background()

	if _, err := f.service.RequestAccountDeletion(ctx, registered.Token, "wrong-password-1", Client{}); !errors.Is(err, ErrInvalidCurrentPassword) {
		t.Fatalf("wrong password error = %v", err)
	}
	if f.repo.users["owner@example.com"].DeletionDueAt != nil || len(f.repo.audits) != 0 {
		t.Fatal("a wrong password changed state")
	}
	// Пять попыток на все команды, которые просят пароль: смена, выгрузка и закрытие считают вместе.
	if _, err := f.service.ExportAccountData(ctx, registered.Token, "wrong-password-2", Client{}); !errors.Is(err, ErrInvalidCurrentPassword) {
		t.Fatalf("export wrong password error = %v", err)
	}
	if err := f.service.ChangePassword(ctx, registered.Token, "wrong-password-3", accountNewPass, Client{}); !errors.Is(err, ErrInvalidCurrentPassword) {
		t.Fatalf("change wrong password error = %v", err)
	}
	if _, err := f.service.RequestAccountDeletion(ctx, registered.Token, "wrong-password-4", Client{}); !errors.Is(err, ErrInvalidCurrentPassword) {
		t.Fatalf("fourth wrong password error = %v", err)
	}
	if _, err := f.service.ExportAccountData(ctx, registered.Token, "wrong-password-5", Client{}); !errors.Is(err, ErrInvalidCurrentPassword) {
		t.Fatalf("fifth wrong password error = %v", err)
	}
	for name, call := range map[string]func() error{
		"deletion": func() error {
			_, err := f.service.RequestAccountDeletion(ctx, registered.Token, accountPass, Client{})
			return err
		},
		"export": func() error {
			_, err := f.service.ExportAccountData(ctx, registered.Token, accountPass, Client{})
			return err
		},
		"change": func() error {
			return f.service.ChangePassword(ctx, registered.Token, accountPass, accountNewPass, Client{})
		},
	} {
		if err := call(); !errors.Is(err, ErrRateLimited) {
			t.Fatalf("%s after five wrong passwords error = %v, want a rate limit", name, err)
		}
	}
}

func TestRequestAccountDeletionSuccessResetsThePasswordCounter(t *testing.T) {
	f := newAccountFixture()
	f.withAccountData(&fakeAccountData{})
	registered := f.register(t, "owner@example.com")
	ctx := context.Background()
	for attempt := 0; attempt < passwordChangeAccountLimit-1; attempt++ {
		if _, err := f.service.RequestAccountDeletion(ctx, registered.Token, "wrong-password", Client{}); !errors.Is(err, ErrInvalidCurrentPassword) {
			t.Fatal(err)
		}
	}
	if _, err := f.service.RequestAccountDeletion(ctx, registered.Token, accountPass, Client{}); err != nil {
		t.Fatalf("correct password error = %v", err)
	}
	for attempt := 0; attempt < passwordChangeAccountLimit-1; attempt++ {
		if err := f.service.ChangePassword(ctx, registered.Token, "wrong-password", accountNewPass, Client{}); !errors.Is(err, ErrInvalidCurrentPassword) {
			t.Fatalf("attempt %d after the reset error = %v", attempt, err)
		}
	}
}

func TestRequestAccountDeletionReportsBlockersWithoutScheduling(t *testing.T) {
	f := newAccountFixture()
	blockers := []DeletionBlocker{
		{Code: BlockerSoleOwner, OrganizationName: "Салон Лотос", HasOtherMembers: true},
		{Code: BlockerPlatformAdmin},
	}
	f.withAccountData(&fakeAccountData{blockers: blockers})
	registered := f.register(t, "owner@example.com")
	f.mailer.mails = nil

	_, err := f.service.RequestAccountDeletion(context.Background(), registered.Token, accountPass, Client{})
	if !errors.Is(err, ErrDeletionBlocked) {
		t.Fatalf("error = %v, want ErrDeletionBlocked", err)
	}
	var blocked DeletionBlockedError
	if !errors.As(err, &blocked) || len(blocked.Blockers) != 2 || blocked.Blockers[0] != blockers[0] || blocked.Blockers[1] != blockers[1] {
		t.Fatalf("blockers = %#v", blocked.Blockers)
	}
	if f.repo.users["owner@example.com"].DeletionDueAt != nil || len(f.repo.audits) != 0 || len(f.mailer.mails) != 0 {
		t.Fatal("a blocked request left a trace")
	}
}

func TestRequestAccountDeletionPropagatesALookupFailureWithoutScheduling(t *testing.T) {
	f := newAccountFixture()
	f.withAccountData(&fakeAccountData{err: errors.New("database unavailable")})
	registered := f.register(t, "owner@example.com")

	if _, err := f.service.RequestAccountDeletion(context.Background(), registered.Token, accountPass, Client{}); err == nil ||
		errors.Is(err, ErrDeletionBlocked) {
		t.Fatalf("error = %v, want the lookup failure", err)
	}
	if f.repo.users["owner@example.com"].DeletionDueAt != nil {
		t.Fatal("deletion was scheduled although blockers could not be checked")
	}
}

func TestAccountDeletionCommandsRequireASessionAndValidInput(t *testing.T) {
	f := newAccountFixture()
	f.withAccountData(&fakeAccountData{})
	registered := f.register(t, "owner@example.com")
	ctx := context.Background()

	if _, err := f.service.RequestAccountDeletion(ctx, "", accountPass, Client{}); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("anonymous request error = %v", err)
	}
	if _, err := f.service.RequestAccountDeletion(ctx, "no-such-session", accountPass, Client{}); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("unknown session error = %v", err)
	}
	if err := f.service.CancelAccountDeletion(ctx, "", Client{}); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("anonymous cancel error = %v", err)
	}
	if _, err := f.service.ExportAccountData(ctx, "", accountPass, Client{}); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("anonymous export error = %v", err)
	}
	for _, password := range []string{"", strings.Repeat("a", maximumPasswordLength+1)} {
		if _, err := f.service.RequestAccountDeletion(ctx, registered.Token, password, Client{}); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("password of %d bytes error = %v, want ErrInvalidInput", len(password), err)
		}
		if _, err := f.service.ExportAccountData(ctx, registered.Token, password, Client{}); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("export password of %d bytes error = %v, want ErrInvalidInput", len(password), err)
		}
	}
	// Ошибки формата пароль не проверяют, поэтому предел подбора они не тратят.
	for attempt := 0; attempt < passwordChangeAccountLimit; attempt++ {
		if _, err := f.service.RequestAccountDeletion(ctx, registered.Token, "", Client{}); !errors.Is(err, ErrInvalidInput) {
			t.Fatal(err)
		}
	}
	if _, err := f.service.RequestAccountDeletion(ctx, registered.Token, accountPass, Client{}); err != nil {
		t.Fatalf("correct password after format errors error = %v", err)
	}
}

func TestAccountDeletionCommandsAreLimitedPerAccount(t *testing.T) {
	f := newAccountFixture()
	f.withAccountData(&fakeAccountData{})
	registered := f.register(t, "owner@example.com")
	ctx := context.Background()

	for round := 0; round < accountDeletionAccountLimit/2; round++ {
		if _, err := f.service.RequestAccountDeletion(ctx, registered.Token, accountPass, Client{}); err != nil {
			t.Fatalf("request %d error = %v", round, err)
		}
		if err := f.service.CancelAccountDeletion(ctx, registered.Token, Client{}); err != nil {
			t.Fatalf("cancel %d error = %v", round, err)
		}
	}
	// Пятое действие ещё допустимо, шестое нет: каждое действие отправляет письмо.
	if _, err := f.service.RequestAccountDeletion(ctx, registered.Token, accountPass, Client{}); err != nil {
		t.Fatalf("fifth action error = %v", err)
	}
	err := f.service.CancelAccountDeletion(ctx, registered.Token, Client{})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("sixth action error = %v, want a rate limit", err)
	}
	if retry := RateLimitRetryAfter(err); retry != accountLimitWindow {
		t.Fatalf("retry after = %s", retry)
	}
	f.now = f.now.Add(accountLimitWindow)
	if err := f.service.CancelAccountDeletion(ctx, registered.Token, Client{}); err != nil {
		t.Fatalf("cancel after the window error = %v", err)
	}
}

func TestCancelAccountDeletionClearsTheScheduleAndNotifies(t *testing.T) {
	f := newAccountFixture()
	f.withAccountData(&fakeAccountData{})
	registered := f.register(t, "owner@example.com")
	ctx := context.Background()
	if _, err := f.service.RequestAccountDeletion(ctx, registered.Token, accountPass, Client{}); err != nil {
		t.Fatal(err)
	}
	f.mailer.mails = nil

	if err := f.service.CancelAccountDeletion(ctx, registered.Token, Client{IPAddress: "192.0.2.9"}); err != nil {
		t.Fatalf("CancelAccountDeletion() error = %v", err)
	}
	stored := f.repo.users["owner@example.com"]
	if stored.DeletionDueAt != nil || stored.DeletionRequestedAt != nil {
		t.Fatalf("schedule was not cleared: %v / %v", stored.DeletionRequestedAt, stored.DeletionDueAt)
	}
	if got := f.repo.auditOperations(); len(got) != 2 || got[1] != "ACCOUNT_DELETION_CANCELLED" {
		t.Fatalf("audit = %v", got)
	}
	mail := f.mailer.last(t)
	if mail.Kind != "ACCOUNT_DELETION_CANCELLED" || mail.To != "owner@example.com" || !strings.Contains(mail.Text, accountBaseURL+"/forgot-password") {
		t.Fatalf("mail = %#v", mail)
	}

	// Повтор без назначения успешен и ничего не пишет.
	audits, mails := len(f.repo.audits), len(f.mailer.mails)
	if err := f.service.CancelAccountDeletion(ctx, registered.Token, Client{}); err != nil {
		t.Fatalf("repeated cancel error = %v", err)
	}
	if len(f.repo.audits) != audits || len(f.mailer.mails) != mails {
		t.Fatal("a repeated cancel wrote audit or sent mail")
	}
}

func TestAccountDeletionSurvivesMailFailureAndMissingMail(t *testing.T) {
	f := newAccountFixture()
	f.withAccountData(&fakeAccountData{})
	registered := f.register(t, "owner@example.com")
	f.mailer.err = errors.New("queue unavailable")
	f.reporter.events = nil

	if _, err := f.service.RequestAccountDeletion(context.Background(), registered.Token, accountPass, Client{}); err != nil {
		t.Fatalf("request with a failing queue error = %v", err)
	}
	if f.repo.users["owner@example.com"].DeletionDueAt == nil {
		t.Fatal("deletion was not scheduled when the notice could not be queued")
	}
	if len(f.reporter.events) != 1 || f.reporter.events[0] != "identity.account_notice_enqueue_failed" {
		t.Fatalf("reported events = %v", f.reporter.events)
	}

	// Без настроенной почты запрос работает без писем.
	bare := newAccountFixture()
	bare.service = NewService(bare.repo, newTestRateLimiter(), testPasswords{}, &testIDs{}, &longTokens{},
		func() time.Time { return bare.now }, 24*time.Hour).WithRegistration(RegistrationOpen, nil).WithAccountData(&fakeAccountData{})
	user := bare.register(t, "bare@example.com")
	if _, err := bare.service.RequestAccountDeletion(context.Background(), user.Token, accountPass, Client{}); err != nil {
		t.Fatalf("request without mail error = %v", err)
	}
	if err := bare.service.CancelAccountDeletion(context.Background(), user.Token, Client{}); err != nil {
		t.Fatalf("cancel without mail error = %v", err)
	}
}

func TestAccountDeletionNeedsAnActiveUserAtWriteTime(t *testing.T) {
	// Пользователя отключили между проверкой сессии и записью: команда отвечает как на потерянную сессию.
	f := newAccountFixture()
	f.withAccountData(&fakeAccountData{})
	registered := f.register(t, "owner@example.com")
	f.service = f.service.withRepository(vanishingRepository{memoryRepository: f.repo})

	if _, err := f.service.RequestAccountDeletion(context.Background(), registered.Token, accountPass, Client{}); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("request error = %v", err)
	}
	if err := f.service.CancelAccountDeletion(context.Background(), registered.Token, Client{}); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("cancel error = %v", err)
	}
}

type vanishingRepository struct{ *memoryRepository }

func (vanishingRepository) ScheduleDeletion(context.Context, domain.DeletionSchedule) (time.Time, bool, error) {
	return time.Time{}, false, domain.ErrNotFound
}

func (vanishingRepository) CancelDeletion(context.Context, string, time.Time, domain.AuditRecord) (bool, error) {
	return false, domain.ErrNotFound
}

func TestExportAccountDataAuditsBeforeTheDocumentIsProduced(t *testing.T) {
	f := newAccountFixture()
	var calls []string
	data := &fakeAccountData{document: []byte(`{"format":"lidradar.account-export/1"}`), calls: &calls}
	auditor := &recordingAuditor{calls: &calls}
	f.service = f.service.WithAccountData(data).WithAuditor(auditor)
	registered := f.register(t, "owner@example.com")
	calls = nil

	document, err := f.service.ExportAccountData(context.Background(), registered.Token, accountPass, Client{IPAddress: "192.0.2.3"})
	if err != nil {
		t.Fatalf("ExportAccountData() error = %v", err)
	}
	if string(document) != `{"format":"lidradar.account-export/1"}` {
		t.Fatalf("document = %s", document)
	}
	userID := f.repo.users["owner@example.com"].ID
	want := []string{"audit:ACCOUNT_DATA_EXPORTED", "export:" + userID}
	if strings.Join(calls, ",") != strings.Join(want, ",") {
		t.Fatalf("call order = %v, want %v", calls, want)
	}
}

func TestExportAccountDataWithoutATraceProducesNothing(t *testing.T) {
	f := newAccountFixture()
	data := &fakeAccountData{document: []byte(`{}`)}
	auditor := &recordingAuditor{}
	f.service = f.service.WithAccountData(data).WithAuditor(auditor)
	registered := f.register(t, "owner@example.com")
	auditor.err = errors.New("audit unavailable")

	if _, err := f.service.ExportAccountData(context.Background(), registered.Token, accountPass, Client{}); err == nil {
		t.Fatal("export succeeded without an audit record")
	}
	if len(data.exported) != 0 {
		t.Fatalf("the document was produced for %v although the audit failed", data.exported)
	}
}

func TestExportAccountDataIsLimitedPerAccount(t *testing.T) {
	f := newAccountFixture()
	f.withAccountData(&fakeAccountData{document: []byte(`{}`)})
	registered := f.register(t, "owner@example.com")
	ctx := context.Background()

	for attempt := 1; attempt <= accountExportAccountLimit; attempt++ {
		if _, err := f.service.ExportAccountData(ctx, registered.Token, accountPass, Client{}); err != nil {
			t.Fatalf("export %d error = %v", attempt, err)
		}
	}
	if _, err := f.service.ExportAccountData(ctx, registered.Token, accountPass, Client{}); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("export over the limit error = %v", err)
	}
	other := f.register(t, "other@example.com")
	if _, err := f.service.ExportAccountData(ctx, other.Token, accountPass, Client{}); err != nil {
		t.Fatalf("another account was affected by the limit: %v", err)
	}
	f.now = f.now.Add(accountLimitWindow)
	if _, err := f.service.ExportAccountData(ctx, registered.Token, accountPass, Client{}); err != nil {
		t.Fatalf("export after the window error = %v", err)
	}
}

func TestExportAccountDataPassesOnlyTheAuthenticatedUser(t *testing.T) {
	f := newAccountFixture()
	data := &fakeAccountData{document: []byte(`{}`)}
	f.withAccountData(data)
	first := f.register(t, "first@example.com")
	second := f.register(t, "second@example.com")
	ctx := context.Background()

	if _, err := f.service.ExportAccountData(ctx, first.Token, accountPass, Client{}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.ExportAccountData(ctx, second.Token, accountPass, Client{}); err != nil {
		t.Fatal(err)
	}
	if len(data.exported) != 2 || data.exported[0] != first.User.ID || data.exported[1] != second.User.ID || first.User.ID == second.User.ID {
		t.Fatalf("exported users = %v, want [%s %s]", data.exported, first.User.ID, second.User.ID)
	}
}

func TestAccountDataCommandsFailWithoutTheContract(t *testing.T) {
	f := newAccountFixture()
	registered := f.register(t, "owner@example.com")
	ctx := context.Background()

	if _, err := f.service.ExportAccountData(ctx, registered.Token, accountPass, Client{}); err == nil || errors.Is(err, ErrInvalidInput) {
		t.Fatalf("export without the contract error = %v, want an internal error", err)
	}
	if _, err := f.service.RequestAccountDeletion(ctx, registered.Token, accountPass, Client{}); err == nil || errors.Is(err, ErrInvalidInput) {
		t.Fatalf("deletion without the contract error = %v, want an internal error", err)
	}
	// Отмена не требует данных: пользователь вправе снять назначение, даже если контракт отключили.
	if err := f.service.CancelAccountDeletion(ctx, registered.Token, Client{}); err != nil {
		t.Fatalf("cancel without the contract error = %v", err)
	}
}

func TestLoginRefusesAnAccountClosedWhileTheSessionWasBeingCreated(t *testing.T) {
	f := newAccountFixture()
	f.register(t, "owner@example.com")
	f.service = f.service.withRepository(closedDuringLoginRepository{memoryRepository: f.repo})

	if _, err := f.login(t, "owner@example.com", accountPass); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("login error = %v, want ErrInvalidCredentials", err)
	}
}

type closedDuringLoginRepository struct{ *memoryRepository }

func (closedDuringLoginRepository) CreateSession(context.Context, domain.Session) error {
	return domain.ErrNotFound
}

func TestDeletionMailsStateWhatWillHappen(t *testing.T) {
	due := time.Date(2026, 11, 13, 21, 30, 0, 0, time.FixedZone("UTC+3", 3*3600))
	_, requested := deletionRequestedMail("https://app.example.test/settings/account", "https://app.example.test/forgot-password", due)
	for _, want := range []string{"13.11.2026 18:30 UTC", "обезличены", "вход работает как обычно", "/settings/account", "/forgot-password", "не вы"} {
		if !strings.Contains(requested, want) {
			t.Errorf("requested mail lacks %q:\n%s", want, requested)
		}
	}
	subject, cancelled := deletionCancelledMail("https://app.example.test/forgot-password")
	if !strings.Contains(subject, "отменено") || !strings.Contains(cancelled, "/forgot-password") || !strings.Contains(cancelled, "не вы") {
		t.Errorf("cancelled mail = %q / %q", subject, cancelled)
	}
	for _, text := range []string{requested, cancelled} {
		if strings.Contains(text, "token") || strings.Contains(text, "#") {
			t.Errorf("a notice carries a token-like link: %s", text)
		}
	}
}

func TestNewDeletionScheduleValidatesTheDates(t *testing.T) {
	at := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	audit := domain.AuditRecord{ID: "audit-1", Operation: "ACCOUNT_DELETION_REQUESTED", At: at}
	if _, err := domain.NewDeletionSchedule("user-1", at, at.Add(time.Hour), audit); err != nil {
		t.Fatalf("valid schedule error = %v", err)
	}
	for name, build := range map[string]func() (domain.DeletionSchedule, error){
		"no user": func() (domain.DeletionSchedule, error) {
			return domain.NewDeletionSchedule("", at, at.Add(time.Hour), audit)
		},
		"zero request": func() (domain.DeletionSchedule, error) {
			return domain.NewDeletionSchedule("user-1", time.Time{}, at, audit)
		},
		"due equals at": func() (domain.DeletionSchedule, error) {
			return domain.NewDeletionSchedule("user-1", at, at, audit)
		},
		"due before": func() (domain.DeletionSchedule, error) {
			return domain.NewDeletionSchedule("user-1", at, at.Add(-time.Hour), audit)
		},
		"no audit": func() (domain.DeletionSchedule, error) {
			return domain.NewDeletionSchedule("user-1", at, at.Add(time.Hour), domain.AuditRecord{})
		},
	} {
		if _, err := build(); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("%s: error = %v, want ErrInvalid", name, err)
		}
	}
}
