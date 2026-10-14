package application

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"lidradar/backend/internal/lifecycle/domain"
)

// Исполнитель обезличивания и операторская команда на подставном хранилище (ADR 0059). Что делает база, проверяют
// тесты хранилища; здесь — как служба готовит команду, что делает с исходом и что рассылает.

func done(email string) domain.ErasureResult {
	return domain.ErasureResult{Outcome: domain.ErasureDone, Email: email, Counts: map[string]int64{"sessions": 2, "users": 1}}
}

func TestProcessDueDeletionsAnonymizesEachDueUserAndNotifiesOnce(t *testing.T) {
	f := newFixture()
	f.store.due = []string{"user-1", "user-2"}
	f.store.results["user-1"] = done("first@example.test")
	f.store.results["user-2"] = done("second@example.test")

	processed, err := f.service.ProcessDueDeletions(context.Background())
	if err != nil || processed != 2 {
		t.Fatalf("ProcessDueDeletions() = %d, %v", processed, err)
	}
	if f.store.dueLimit != 20 {
		t.Errorf("пачка = %d, ожидалось 20", f.store.dueLimit)
	}
	if len(f.store.commands) != 2 {
		t.Fatalf("команд: %d", len(f.store.commands))
	}
	for index, command := range f.store.commands {
		if command.Mode != domain.ModeDue || command.DryRun || command.UserID != f.store.due[index] || !command.At.Equal(f.now) ||
			command.AuditID == "" || command.AdminAuditID != "" || command.Note != "" {
			t.Errorf("команда %d = %+v", index, command)
		}
		if command.Validate() != nil {
			t.Errorf("служба собрала недопустимую команду: %+v", command)
		}
	}
	if f.store.commands[0].AuditID == f.store.commands[1].AuditID {
		t.Error("записи аудита двух пользователей разделяют идентификатор")
	}
	if len(f.mailer.mails) != 2 || f.mailer.mails[0].Kind != "ACCOUNT_DELETED" || f.mailer.mails[0].To != "first@example.test" ||
		f.mailer.mails[1].To != "second@example.test" {
		t.Fatalf("письма: %+v", f.mailer.mails)
	}
	if got := f.mailer.mails[0].ExpiresAt; !got.Equal(f.now.Add(24 * time.Hour)) {
		t.Errorf("срок письма = %s", got)
	}
	text := f.mailer.mails[0].Text
	for _, want := range []string{"обезличены", "войти в него больше нельзя", "один раз", "зарегистрируйтесь заново"} {
		if !strings.Contains(text, want) {
			t.Errorf("письмо не содержит %q:\n%s", want, text)
		}
	}
	// Журнал знает идентификатор и режим, но не адрес.
	for _, line := range f.reporter.lines {
		if strings.Contains(line, "example.test") {
			t.Errorf("в журнале адрес: %s", line)
		}
	}
	if len(f.reporter.events) != 2 || f.reporter.events[0] != "account.deletion.anonymized" {
		t.Errorf("события журнала: %v", f.reporter.events)
	}
}

func TestProcessDueDeletionsHashesAFreshRandomUnstoredSecretForEachUser(t *testing.T) {
	f := newFixture()
	f.store.due = []string{"user-1", "user-2"}
	f.store.results["user-1"] = done("first@example.test")
	f.store.results["user-2"] = done("second@example.test")
	if _, err := f.service.ProcessDueDeletions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.passwords.secrets) != 2 || f.passwords.secrets[0] == f.passwords.secrets[1] {
		t.Fatalf("секреты = %v: у каждого пользователя свой", f.passwords.secrets)
	}
	for index, secret := range f.passwords.secrets {
		if len(secret) != 64 || strings.Trim(secret, "0123456789abcdef") != "" {
			t.Errorf("секрет %d = %q: ожидалось 256 бит в hex", index, secret)
		}
		// В команду попадает только хэш; открытый секрет нигде дальше не живёт.
		if f.store.commands[index].PasswordHash != "hash-of-"+secret[:8] {
			t.Errorf("команда %d несёт %q", index, f.store.commands[index].PasswordHash)
		}
		for _, line := range f.reporter.lines {
			if strings.Contains(line, secret) {
				t.Errorf("секрет попал в журнал: %s", line)
			}
		}
	}
}

func TestProcessDueDeletionsTellsAboutABlockerAndDoesNotClaimDeletion(t *testing.T) {
	f := newFixture()
	f.store.due = []string{"user-1", "user-2"}
	f.store.results["user-1"] = domain.ErasureResult{
		Outcome: domain.ErasureBlocked, Email: "owner@example.test",
		Blockers: []domain.Blocker{{Code: "SOLE_OWNER", OrganizationName: "Салон Лотос", HasOtherMembers: true}, {Code: "PLATFORM_ADMIN"}},
	}
	f.store.results["user-2"] = domain.ErasureResult{
		Outcome: domain.ErasureBlocked, Email: "alone@example.test",
		Blockers: []domain.Blocker{{Code: "SOLE_OWNER", OrganizationName: "Один", HasOtherMembers: false}},
	}
	processed, err := f.service.ProcessDueDeletions(context.Background())
	if err != nil || processed != 2 {
		t.Fatalf("ProcessDueDeletions() = %d, %v", processed, err)
	}
	if len(f.mailer.mails) != 2 {
		t.Fatalf("письма: %+v", f.mailer.mails)
	}
	first, second := f.mailer.mails[0], f.mailer.mails[1]
	if first.Kind != "ACCOUNT_DELETION_BLOCKED" || first.To != "owner@example.test" || !strings.Contains(first.Subject, "не выполнено") {
		t.Errorf("письмо = %+v", first)
	}
	for _, want := range []string{"«Салон Лотос»", "«Настройки → Команда»", "администратор платформы", "запросите удаление заново", "https://app.example.test/settings/account", "работает как обычно"} {
		if !strings.Contains(first.Text, want) {
			t.Errorf("письмо о блокировке не содержит %q:\n%s", want, first.Text)
		}
	}
	if !strings.Contains(second.Text, "в ней нет других участников") || !strings.Contains(second.Text, "поддержка") {
		t.Errorf("письмо для одноместной организации:\n%s", second.Text)
	}
	for _, mail := range f.mailer.mails {
		if mail.Kind == "ACCOUNT_DELETED" || strings.Contains(mail.Text, "обезличены") {
			t.Errorf("письмо о блокировке сообщает об удалении: %+v", mail)
		}
	}
	if f.reporter.events[0] != "account.deletion.blocked" || !strings.Contains(f.reporter.lines[0], "SOLE_OWNER") {
		t.Errorf("журнал: %v", f.reporter.lines)
	}
}

func TestProcessDueDeletionsIgnoresSkippedUsers(t *testing.T) {
	f := newFixture()
	f.store.due = []string{"user-1", "user-2"} // у обоих исход по умолчанию Skipped
	processed, err := f.service.ProcessDueDeletions(context.Background())
	if err != nil || processed != 0 {
		t.Fatalf("ProcessDueDeletions() = %d, %v", processed, err)
	}
	if len(f.mailer.mails) != 0 || len(f.reporter.events) != 0 {
		t.Errorf("пропущенные пользователи оставили следы: %v %v", f.mailer.mails, f.reporter.events)
	}
}

func TestProcessDueDeletionsOneFailureDoesNotStopTheBatch(t *testing.T) {
	f := newFixture()
	f.store.due = []string{"user-1", "user-2", "user-3"}
	boom := errors.New("lock timeout for first@example.test")
	f.store.failures["user-1"] = boom
	f.store.results["user-2"] = done("second@example.test")
	f.store.results["user-3"] = done("third@example.test")

	processed, err := f.service.ProcessDueDeletions(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want the first failure", err)
	}
	if processed != 2 || len(f.store.commands) != 3 || len(f.mailer.mails) != 2 {
		t.Errorf("обработано %d, команд %d, писем %d", processed, len(f.store.commands), len(f.mailer.mails))
	}
	if f.reporter.events[0] != "account.deletion.failed" {
		t.Errorf("события: %v", f.reporter.events)
	}
	// Сбой логируется по идентификатору пользователя; текст ошибки хранилища может нести адрес, но журнал его
	// получает как есть от хранилища, поэтому хранилище обязано не включать адреса в ошибки. Служба же не добавляет своих.
	if !strings.Contains(f.reporter.lines[0], "user-1") {
		t.Errorf("журнал сбоя не называет пользователя: %s", f.reporter.lines[0])
	}
}

func TestProcessDueDeletionsKeepsTheResultWhenTheNoticeCannotBeQueued(t *testing.T) {
	f := newFixture()
	f.store.due = []string{"user-1"}
	f.store.results["user-1"] = done("first@example.test")
	f.mailer.err = errors.New("queue unavailable")
	processed, err := f.service.ProcessDueDeletions(context.Background())
	if err != nil || processed != 1 {
		t.Fatalf("сбой постановки письма стал сбоем исполнителя: %d, %v", processed, err)
	}
	if got := f.reporter.events; len(got) != 2 || got[1] != "account.deletion.notice_failed" {
		t.Errorf("события: %v", got)
	}
	for _, line := range f.reporter.lines {
		if strings.Contains(line, "example.test") {
			t.Errorf("в журнале адрес: %s", line)
		}
	}
}

func TestProcessDueDeletionsWorksWithoutMail(t *testing.T) {
	f := newFixture()
	f.service = NewService(f.store, f.passwords, f.ids, func() time.Time { return f.now }).WithReporter(f.reporter)
	f.store.due = []string{"user-1"}
	f.store.results["user-1"] = done("first@example.test")
	if processed, err := f.service.ProcessDueDeletions(context.Background()); err != nil || processed != 1 {
		t.Fatalf("ProcessDueDeletions() = %d, %v", processed, err)
	}
}

func TestProcessDueDeletionsStopsOnACancelledContext(t *testing.T) {
	f := newFixture()
	f.store.due = []string{"user-1", "user-2"}
	f.store.results["user-1"] = done("first@example.test")
	f.store.results["user-2"] = done("second@example.test")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	processed, err := f.service.ProcessDueDeletions(ctx)
	if err != nil || processed != 0 || len(f.store.commands) != 0 {
		t.Errorf("отменённый контекст: обработано %d, команд %d, error %v", processed, len(f.store.commands), err)
	}
}

func TestProcessDueDeletionsReportsQueueAndHashFailures(t *testing.T) {
	f := newFixture()
	f.store.dueErr = errors.New("database unavailable")
	if _, err := f.service.ProcessDueDeletions(context.Background()); err == nil {
		t.Error("сбой чтения очереди потерян")
	}
	f.store.dueErr = nil
	f.store.due = []string{"user-1"}
	f.passwords.err = errors.New("hashing unavailable")
	_, err := f.service.ProcessDueDeletions(context.Background())
	if err == nil || len(f.store.commands) != 0 {
		t.Errorf("сбой хэширования: error %v, команд %d", err, len(f.store.commands))
	}
	if _, err := (Service{}).ProcessDueDeletions(context.Background()); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("пустая служба error = %v", err)
	}
}

func TestEraseUserRunsTheOperatorErasureByAddress(t *testing.T) {
	f := newFixture()
	f.store.users["user@example.test"] = "user-1"
	f.store.results["user-1"] = done("user@example.test")

	report, err := f.service.EraseUser(context.Background(), "  User@Example.test ", "  обращение №17 ", false)
	if err != nil {
		t.Fatalf("EraseUser() error = %v", err)
	}
	if report.UserID != "user-1" || report.Outcome != domain.ErasureDone || report.DryRun || report.Counts["sessions"] != 2 {
		t.Errorf("report = %+v", report)
	}
	command := f.store.commands[0]
	if command.Mode != domain.ModeImmediate || command.Note != "обращение №17" || command.AdminAuditID == "" || command.AuditID == "" ||
		command.AdminAuditID == command.AuditID || command.DryRun || command.Validate() != nil {
		t.Errorf("команда = %+v", command)
	}
	if len(f.mailer.mails) != 1 || f.mailer.mails[0].Kind != "ACCOUNT_DELETED" || f.mailer.mails[0].To != "user@example.test" {
		t.Errorf("письма: %+v", f.mailer.mails)
	}
}

func TestEraseUserDryRunSendsNothingAndLogsNothing(t *testing.T) {
	f := newFixture()
	f.store.users["user@example.test"] = "user-1"
	f.store.results["user-1"] = done("user@example.test")
	report, err := f.service.EraseUser(context.Background(), "user@example.test", "проверка", true)
	if err != nil || !report.DryRun || report.Outcome != domain.ErasureDone {
		t.Fatalf("EraseUser() = %+v, %v", report, err)
	}
	if !f.store.commands[0].DryRun {
		t.Error("сухой запуск не дошёл до хранилища")
	}
	if len(f.mailer.mails) != 0 || len(f.reporter.events) != 0 {
		t.Errorf("сухой запуск оставил следы: %v %v", f.mailer.mails, f.reporter.events)
	}
}

func TestEraseUserWithABlockerSendsNothingAndReportsTheReasons(t *testing.T) {
	f := newFixture()
	f.store.users["owner@example.test"] = "user-1"
	f.store.results["user-1"] = domain.ErasureResult{
		Outcome: domain.ErasureBlocked, Email: "owner@example.test", Blockers: []domain.Blocker{{Code: "SOLE_OWNER", OrganizationName: "Салон"}},
	}
	report, err := f.service.EraseUser(context.Background(), "owner@example.test", "обращение", false)
	if err != nil || report.Outcome != domain.ErasureBlocked || len(report.Blockers) != 1 {
		t.Fatalf("EraseUser() = %+v, %v", report, err)
	}
	if len(f.mailer.mails) != 0 {
		t.Errorf("отказ оператору разослал письма: %+v", f.mailer.mails)
	}
}

func TestEraseUserValidatesItsInput(t *testing.T) {
	f := newFixture()
	f.store.users["user@example.test"] = "user-1"
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"без адреса":    func() error { _, err := f.service.EraseUser(ctx, " ", "основание", false); return err },
		"без основания": func() error { _, err := f.service.EraseUser(ctx, "user@example.test", "  ", false); return err },
		// Проверка входа раньше поиска: запрос без основания не должен выдавать, есть ли такой пользователь.
		"без основания и без пользователя": func() error {
			_, err := f.service.EraseUser(ctx, "nobody@example.test", "  ", false)
			return err
		},
		"длинное основание": func() error {
			_, err := f.service.EraseUser(ctx, "user@example.test", strings.Repeat("я", 501), false)
			return err
		},
	} {
		if err := call(); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("%s: error = %v, want ErrInvalid", name, err)
		}
	}
	if _, err := f.service.EraseUser(ctx, "nobody@example.test", "основание", false); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("неизвестный адрес error = %v, want ErrUserNotFound", err)
	}
	// Ровно предельная длина проходит (считаются символы, а не байты).
	f.store.results["user-1"] = done("user@example.test")
	if _, err := f.service.EraseUser(ctx, "user@example.test", strings.Repeat("я", 500), true); err != nil {
		t.Errorf("основание в 500 символов отклонено: %v", err)
	}
	if len(f.store.commands) != 1 {
		t.Errorf("при отказе валидации хранилище не должно вызываться; команд %d", len(f.store.commands))
	}
	if _, err := (Service{}).EraseUser(ctx, "user@example.test", "основание", false); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("пустая служба error = %v", err)
	}
}

func TestStatsPassThroughTheStore(t *testing.T) {
	f := newFixture()
	f.store.stats = DeletionStats{Scheduled: 4, Due: 3, Overdue: 2}
	stats, err := f.service.Stats(context.Background())
	if err != nil || stats != f.store.stats {
		t.Errorf("Stats() = %+v, %v", stats, err)
	}
	if _, err := (Service{}).Stats(context.Background()); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("пустая служба error = %v", err)
	}
}
