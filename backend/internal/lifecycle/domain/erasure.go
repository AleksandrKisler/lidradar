package domain

import (
	"fmt"
	"time"
)

const (
	// AnonymizedDisplayName — имя обезличенного пользователя (строка 19 матрицы ADR 0056).
	AnonymizedDisplayName = "Удалён"

	// anonymizedEmailDomain — зарезервированная зона .invalid (RFC 2606): письмо на такой адрес не уйдёт, а вход
	// по нему никто не подберёт.
	anonymizedEmailDomain = "deleted.invalid"
)

// AnonymizedEmail возвращает адрес обезличенного пользователя: по идентификатору, уникален и нигде не доставляется.
func AnonymizedEmail(userID string) string { return userID + "@" + anonymizedEmailDomain }

// ErasureMode отличает исполнение по сроку от немедленной операторской команды.
type ErasureMode string

const (
	// ModeDue — исполнитель берёт пользователя, чей срок наступил; блокировка снимает запрос.
	ModeDue ErasureMode = "DUE"
	// ModeImmediate — операторская команда: срок не нужен; блокировка отказывает без изменений.
	ModeImmediate ErasureMode = "IMMEDIATE"
)

// Erasure — команда обезличивания одного пользователя.
type Erasure struct {
	UserID string
	Mode   ErasureMode
	At     time.Time
	// PasswordHash — хэш случайного секрета, который нигде не сохраняется: пароль непригоден для входа, но
	// проверка пароля на нём не падает.
	PasswordHash string
	// AuditID — идентификатор записи auth_audit_log об исходе; AdminAuditID и Note задают запись admin_audit_log
	// операторской команды (в режиме ModeImmediate).
	AuditID      string
	AdminAuditID string
	Note         string
	// DryRun выполняет все изменения в транзакции и откатывает её: счётчики совпадают с боевым запуском.
	DryRun bool
}

// Validate проверяет команду до обращения к базе.
func (erasure Erasure) Validate() error {
	if erasure.UserID == "" || erasure.At.IsZero() || erasure.PasswordHash == "" || erasure.AuditID == "" {
		return ErrInvalid
	}
	switch erasure.Mode {
	case ModeDue:
		if erasure.DryRun {
			return fmt.Errorf("%w: dry run is for operator erasure", ErrInvalid)
		}
	case ModeImmediate:
		if erasure.AdminAuditID == "" || erasure.Note == "" {
			return fmt.Errorf("%w: operator erasure needs an audit id and a note", ErrInvalid)
		}
	default:
		return ErrInvalid
	}
	return nil
}

// ErasureOutcome — исход попытки обезличивания.
type ErasureOutcome string

const (
	// ErasureDone: пользователь обезличен (при DryRun — был бы обезличен).
	ErasureDone ErasureOutcome = "DONE"
	// ErasureBlocked: мешает блокировка; в ModeDue запрос снят, в ModeImmediate ничего не изменено.
	ErasureBlocked ErasureOutcome = "BLOCKED"
	// ErasureSkipped: пользователь не найден, уже обезличен, занят другой транзакцией или срок не наступил.
	ErasureSkipped ErasureOutcome = "SKIPPED"
)

// Blocker — причина, по которой пользователя нельзя обезличить (та же форма, что у identity.DeletionBlocker).
type Blocker struct {
	Code             string
	OrganizationName string
	HasOtherMembers  bool
}

// ErasureResult — результат обезличивания.
type ErasureResult struct {
	Outcome  ErasureOutcome
	Blockers []Blocker
	// Email — прежний адрес пользователя: нужен для письма после фиксации, дальше нигде не хранится.
	Email string
	// Counts — сколько строк затронуто по таблицам (без каскадных): для журнала и сухого запуска, без содержимого.
	Counts map[string]int64
}
