package application

import (
	"context"
	"errors"
	"time"

	"lidradar/backend/internal/identity/domain"
)

// Команды выгрузки личных данных и закрытия учётной записи (ADR 0059). Сами данные, блокировки закрытия и
// исполнение обезличивания принадлежат модулю lifecycle и приходят через AccountData; здесь — кто просит,
// подтверждение паролем, пределы, аудит и письма.

var (
	// ErrDeletionBlocked: закрытие невозможно, пока пользователь единственный владелец организации или
	// платформенный администратор. Причины несёт DeletionBlockedError.
	ErrDeletionBlocked = errors.New("account deletion is blocked")

	errAccountDataNotConfigured = errors.New("account data is not configured")
)

const (
	// AccountDeletionGrace — срок между запросом и обезличиванием: строка 19 матрицы ADR 0056, политика
	// retention-v1. От окружения не зависит.
	AccountDeletionGrace = 30 * 24 * time.Hour

	// Причины, по которым закрыть учётную запись нельзя.
	BlockerSoleOwner     = "SOLE_OWNER"
	BlockerPlatformAdmin = "PLATFORM_ADMIN"

	operationAccountDataExported = "ACCOUNT_DATA_EXPORTED"
	operationDeletionRequested   = "ACCOUNT_DELETION_REQUESTED"
	operationDeletionCancelled   = "ACCOUNT_DELETION_CANCELLED"

	// accountSettingsPath — страница, где закрытие отменяют; ссылка в письмах не несёт токена.
	accountSettingsPath = "/settings/account"
	forgotPasswordPath  = "/forgot-password"
)

// DeletionBlocker — одна причина, мешающая закрыть учётную запись.
type DeletionBlocker struct {
	Code string `json:"code"`
	// OrganizationName и HasOtherMembers заполнены для SOLE_OWNER: есть ли в организации другие участники
	// (тогда достаточно передать владение, иначе организацию нужно закрывать через оператора).
	OrganizationName string `json:"organizationName,omitempty"`
	HasOtherMembers  bool   `json:"hasOtherMembers,omitempty"`
}

// DeletionBlockedError возвращается, когда закрытие невозможно; errors.Is(err, ErrDeletionBlocked) истинно.
type DeletionBlockedError struct{ Blockers []DeletionBlocker }

func (err DeletionBlockedError) Error() string { return ErrDeletionBlocked.Error() }
func (err DeletionBlockedError) Unwrap() error { return ErrDeletionBlocked }

// AccountData — узкий контракт данных учётной записи, который реализует модуль lifecycle (ADR 0059); так же
// устроена связка с tenant в MemberInvitations (ADR 0057).
type AccountData interface {
	// DeletionBlockers перечисляет причины, по которым пользователя сейчас нельзя обезличить.
	DeletionBlockers(ctx context.Context, userID string) ([]DeletionBlocker, error)
	// PersonalExport собирает документ личной выгрузки пользователя (JSON, формат lidradar.account-export/1).
	PersonalExport(ctx context.Context, userID string) ([]byte, error)
}

// WithAccountData подключает данные учётной записи. Без вызова выгрузка и закрытие недоступны.
func (s Service) WithAccountData(data AccountData) Service {
	s.data = data
	return s
}

// confirmPassword подтверждает опасное действие текущим паролем. Предел общий со сменой пароля: краденая сессия
// не получает лишних попыток подбора через другие команды. Счётчик расходуется до проверки, сбрасывается при успехе.
func (s Service) confirmPassword(ctx context.Context, user domain.User, password string) error {
	if password == "" || len(password) > maximumPasswordLength {
		return ErrInvalidInput
	}
	subject := rateSubject(user.ID)
	if err := s.takeSubject(ctx, ScopePasswordChangeAccount, subject, passwordChangeAccountLimit, passwordChangeWindow); err != nil {
		return err
	}
	valid, err := s.passwords.Verify(password, user.PasswordHash)
	if err != nil {
		return err
	}
	if !valid {
		return ErrInvalidCurrentPassword
	}
	return s.limiter.Reset(ctx, ScopePasswordChangeAccount, subject)
}

// ExportAccountData возвращает документ личной выгрузки. Запись аудита делается до выдачи данных: выгрузки без
// следа не бывает.
func (s Service) ExportAccountData(ctx context.Context, sessionToken, password string, client Client) ([]byte, error) {
	if err := s.ready(); err != nil {
		return nil, ErrInvalidInput
	}
	if s.data == nil {
		return nil, errAccountDataNotConfigured
	}
	user, err := s.Authenticate(ctx, sessionToken)
	if err != nil {
		return nil, err
	}
	if err := s.confirmPassword(ctx, user, password); err != nil {
		return nil, err
	}
	if err := s.takeSubject(ctx, ScopeAccountExportAccount, rateSubject(user.ID), accountExportAccountLimit, accountLimitWindow); err != nil {
		return nil, err
	}
	if err := s.auditAuth(ctx, user.ID, operationAccountDataExported, client, s.now().UTC()); err != nil {
		return nil, err
	}
	return s.data.PersonalExport(ctx, user.ID)
}

// RequestAccountDeletion назначает обезличивание через AccountDeletionGrace и возвращает срок. Повторный запрос
// при уже назначенном удалении возвращает прежний срок: ничего не двигает и письма не шлёт. Блокировки
// проверяются здесь и ещё раз при исполнении.
func (s Service) RequestAccountDeletion(ctx context.Context, sessionToken, password string, client Client) (time.Time, error) {
	if err := s.ready(); err != nil {
		return time.Time{}, ErrInvalidInput
	}
	if s.data == nil {
		return time.Time{}, errAccountDataNotConfigured
	}
	user, err := s.Authenticate(ctx, sessionToken)
	if err != nil {
		return time.Time{}, err
	}
	if err := s.confirmPassword(ctx, user, password); err != nil {
		return time.Time{}, err
	}
	if err := s.takeSubject(ctx, ScopeAccountDeletionAccount, rateSubject(user.ID), accountDeletionAccountLimit, accountLimitWindow); err != nil {
		return time.Time{}, err
	}
	if user.DeletionDueAt != nil {
		return user.DeletionDueAt.UTC(), nil
	}
	blockers, err := s.data.DeletionBlockers(ctx, user.ID)
	if err != nil {
		return time.Time{}, err
	}
	if len(blockers) > 0 {
		return time.Time{}, DeletionBlockedError{Blockers: blockers}
	}
	now := s.now().UTC()
	audit, err := s.auditRecord(operationDeletionRequested, client, now)
	if err != nil {
		return time.Time{}, err
	}
	schedule, err := domain.NewDeletionSchedule(user.ID, now, now.Add(AccountDeletionGrace), audit)
	if err != nil {
		return time.Time{}, err
	}
	due, created, err := s.repository.ScheduleDeletion(ctx, schedule)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		// Пользователя отключили между проверкой сессии и назначением.
		return time.Time{}, ErrUnauthenticated
	case err != nil:
		return time.Time{}, err
	}
	if created {
		subject, text := deletionRequestedMail(s.mail.baseURL+accountSettingsPath, s.mail.baseURL+forgotPasswordPath, due)
		s.sendNotice(ctx, mailKindAccountDeletionRequested, user.Email, subject, text, now)
	}
	return due, nil
}

// CancelAccountDeletion снимает назначение. Пароль не нужен: отмена ничего не разрушает. Повтор без назначения
// успешен и ничего не пишет.
func (s Service) CancelAccountDeletion(ctx context.Context, sessionToken string, client Client) error {
	if err := s.ready(); err != nil {
		return ErrInvalidInput
	}
	user, err := s.Authenticate(ctx, sessionToken)
	if err != nil {
		return err
	}
	if err := s.takeSubject(ctx, ScopeAccountDeletionAccount, rateSubject(user.ID), accountDeletionAccountLimit, accountLimitWindow); err != nil {
		return err
	}
	now := s.now().UTC()
	audit, err := s.auditRecord(operationDeletionCancelled, client, now)
	if err != nil {
		return err
	}
	cancelled, err := s.repository.CancelDeletion(ctx, user.ID, now, audit)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return ErrUnauthenticated
	case err != nil:
		return err
	}
	if cancelled {
		subject, text := deletionCancelledMail(s.mail.baseURL + forgotPasswordPath)
		s.sendNotice(ctx, mailKindAccountDeletionCancelled, user.Email, subject, text, now)
	}
	return nil
}

// sendNotice ставит уведомление в очередь; сбой не отменяет уже выполненное действие.
func (s Service) sendNotice(ctx context.Context, kind, email, subject, text string, now time.Time) {
	if !s.mail.ready() {
		return
	}
	if err := s.mail.mailer.Enqueue(ctx, kind, email, subject, text, now.Add(noticeTTL)); err != nil {
		s.report("identity.account_notice_enqueue_failed", kind, err)
	}
}
