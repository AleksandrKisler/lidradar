package application

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	identityapplication "lidradar/backend/internal/identity/application"
	"lidradar/backend/internal/lifecycle/domain"
)

// Исполнитель обезличивания (цепочка воркера) и операторская команда немедленного обезличивания (ADR 0059).

const (
	mailKindAccountDeleted         = "ACCOUNT_DELETED"
	mailKindAccountDeletionBlocked = "ACCOUNT_DELETION_BLOCKED"

	// maxNoteRunes — предел заметки оператора (основание стирания) в журнале.
	maxNoteRunes = 500

	accountSettingsPath = "/settings/account"
)

// ProcessDueDeletions берёт пачку пользователей, чей срок наступил, и обезличивает каждого отдельной транзакцией.
// Сбой одного не мешает остальным; первая ошибка возвращается после обработки всей пачки, а пользователь остаётся
// в очереди до следующего прохода. Возвращает число пользователей, с которыми что-то произошло.
func (s Service) ProcessDueDeletions(ctx context.Context) (int, error) {
	if s.store == nil || s.passwords == nil || s.ids == nil || s.now == nil {
		return 0, ErrNotConfigured
	}
	users, err := s.store.DueDeletions(ctx, s.now().UTC(), deletionBatch)
	if err != nil {
		return 0, err
	}
	processed := 0
	var first error
	for _, userID := range users {
		if ctx.Err() != nil {
			break
		}
		result, err := s.erase(ctx, userID, domain.ModeDue, "", false)
		if err != nil {
			s.report("error", "Не удалось обезличить учётную запись", "event", "account.deletion.failed", "user_id", userID, "error", err)
			if first == nil {
				first = err
			}
			continue
		}
		if result.Outcome != domain.ErasureSkipped {
			processed++
		}
	}
	return processed, first
}

// ErasureReport — итог операторской команды: что произошло или произошло бы при сухом запуске.
type ErasureReport struct {
	UserID   string
	Outcome  domain.ErasureOutcome
	Blockers []domain.Blocker
	Counts   map[string]int64
	DryRun   bool
}

// EraseUser немедленно обезличивает пользователя по адресу почты, минуя отсрочку. Блокировки соблюдаются. Заметка
// с основанием обязательна и попадает в журнал оператора; dryRun показывает затрагиваемые строки без изменений.
func (s Service) EraseUser(ctx context.Context, email, note string, dryRun bool) (ErasureReport, error) {
	if s.store == nil || s.passwords == nil || s.ids == nil || s.now == nil {
		return ErasureReport{}, ErrNotConfigured
	}
	email = strings.ToLower(strings.TrimSpace(email))
	note = strings.TrimSpace(note)
	if email == "" || note == "" || utf8.RuneCountInString(note) > maxNoteRunes {
		return ErasureReport{}, domain.ErrInvalid
	}
	userID, found, err := s.store.UserIDByEmail(ctx, email)
	if err != nil {
		return ErasureReport{}, err
	}
	if !found {
		return ErasureReport{}, ErrUserNotFound
	}
	result, err := s.erase(ctx, userID, domain.ModeImmediate, note, dryRun)
	if err != nil {
		return ErasureReport{}, err
	}
	return ErasureReport{UserID: userID, Outcome: result.Outcome, Blockers: result.Blockers, Counts: result.Counts, DryRun: dryRun}, nil
}

// erase готовит команду (хэш случайного секрета, идентификаторы записей аудита), исполняет её и рассылает письма об
// исходе. Письма уходят после фиксации и не влияют на результат.
func (s Service) erase(ctx context.Context, userID string, mode domain.ErasureMode, note string, dryRun bool) (domain.ErasureResult, error) {
	secret, err := randomSecret()
	if err != nil {
		return domain.ErasureResult{}, err
	}
	hash, err := s.passwords.Hash(secret)
	if err != nil {
		return domain.ErasureResult{}, fmt.Errorf("hash unusable password: %w", err)
	}
	auditID, err := s.ids.NewID()
	if err != nil {
		return domain.ErasureResult{}, err
	}
	command := domain.Erasure{UserID: userID, Mode: mode, At: s.now().UTC(), PasswordHash: hash, AuditID: auditID, DryRun: dryRun}
	if mode == domain.ModeImmediate {
		if command.AdminAuditID, err = s.ids.NewID(); err != nil {
			return domain.ErasureResult{}, err
		}
		command.Note = note
	}
	if err := command.Validate(); err != nil {
		return domain.ErasureResult{}, err
	}
	result, err := s.store.Erase(ctx, command)
	if err != nil {
		return domain.ErasureResult{}, err
	}
	switch {
	case result.Outcome == domain.ErasureDone && !dryRun:
		s.report("info", "Учётная запись обезличена", "event", "account.deletion.anonymized", "user_id", userID, "mode", string(mode))
		subject, text := accountDeletedMail()
		s.notify(ctx, mailKindAccountDeleted, result.Email, subject, text)
	case result.Outcome == domain.ErasureBlocked && mode == domain.ModeDue:
		s.report("warn", "Удаление учётной записи отменено: появилась блокировка", "event", "account.deletion.blocked", "user_id", userID, "blockers", blockerCodes(result.Blockers))
		subject, text := deletionBlockedMail(result.Blockers, s.baseURL+accountSettingsPath)
		s.notify(ctx, mailKindAccountDeletionBlocked, result.Email, subject, text)
	}
	return result, nil
}

// notify ставит письмо в очередь; сбой не отменяет уже зафиксированное и попадает в журнал без адреса.
func (s Service) notify(ctx context.Context, kind, email, subject, text string) {
	if s.mailer == nil || email == "" {
		return
	}
	if err := s.mailer.Enqueue(ctx, kind, email, subject, text, s.now().UTC().Add(noticeTTL)); err != nil {
		s.report("warn", "Письмо об исходе закрытия учётной записи не поставлено", "event", "account.deletion.notice_failed", "kind", kind, "error", err)
	}
}

func blockerCodes(blockers []domain.Blocker) []string {
	codes := make([]string, 0, len(blockers))
	for _, blocker := range blockers {
		codes = append(codes, blocker.Code)
	}
	return codes
}

// Stats возвращает состояние назначенных удалений.
func (s Service) Stats(ctx context.Context) (DeletionStats, error) {
	if s.store == nil || s.now == nil {
		return DeletionStats{}, ErrNotConfigured
	}
	return s.store.DeletionStats(ctx, s.now().UTC())
}

// deletionBlockedMail объясняет, почему запрос снят, и что сделать. Названия организаций принадлежат получателю.
func deletionBlockedMail(blockers []domain.Blocker, settingsLink string) (subject, text string) {
	var reasons strings.Builder
	for _, blocker := range blockers {
		switch blocker.Code {
		case identityapplication.BlockerSoleOwner:
			reasons.WriteString("— вы единственный владелец организации «" + blocker.OrganizationName + "»: ")
			if blocker.HasOtherMembers {
				reasons.WriteString("передайте владение другому участнику в «Настройки → Команда».\n")
			} else {
				reasons.WriteString("в ней нет других участников, поэтому закрыть её может только поддержка LidRadar.\n")
			}
		case identityapplication.BlockerPlatformAdmin:
			reasons.WriteString("— вы действующий администратор платформы: право должен отозвать оператор.\n")
		}
	}
	return "Удаление аккаунта LidRadar не выполнено",
		"Здравствуйте!\n\n" +
			"Запланированное удаление вашего аккаунта LidRadar не выполнено, потому что на дату обезличивания его нельзя закрыть:\n\n" +
			reasons.String() + "\n" +
			"Запрос отменён, аккаунт работает как обычно. Когда причина будет устранена, запросите удаление заново:\n\n" +
			settingsLink + "\n"
}

// accountDeletedMail сообщает об исполнении. Письмо уходит один раз: адреса больше нет в системе.
func accountDeletedMail() (subject, text string) {
	return "Аккаунт LidRadar удалён",
		"Здравствуйте!\n\n" +
			"Данные вашего аккаунта LidRadar обезличены, войти в него больше нельзя. Это письмо отправлено один раз: " +
			"адрес не сохранён, и других писем на него не будет.\n\n" +
			"Записи, которые вы вносили в организациях, остаются в них без привязки к вашему имени и адресу. " +
			"Если захотите вернуться, зарегистрируйтесь заново.\n"
}
