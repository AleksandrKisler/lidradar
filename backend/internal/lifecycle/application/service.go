// Package application coordinates data export and account closure (ADR 0059): the personal export, the
// organization export, the executor that anonymizes closed accounts and the operator erasure.
package application

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	auditapplication "lidradar/backend/internal/audit/application"
	identityapplication "lidradar/backend/internal/identity/application"
	"lidradar/backend/internal/lifecycle/domain"
)

var (
	// ErrForbidden: у пользователя нет права выгрузки организации (оно только у владельца).
	ErrForbidden = errors.New("organization export is not allowed")
	// ErrBusy: в процессе уже идёт предельное число выгрузок организаций.
	ErrBusy = errors.New("too many organization exports are running")
	// ErrRateLimited: предел выгрузок организации исчерпан; причину и срок несёт RateLimitError.
	ErrRateLimited = errors.New("organization export rate limited")
	// ErrUserNotFound: операторская команда не нашла пользователя.
	ErrUserNotFound = errors.New("user not found")
	// ErrNotConfigured: службе не хватает зависимости для запрошенной операции.
	ErrNotConfigured = errors.New("lifecycle service is not configured")
)

// RateLimitError несёт срок, через который выгрузку можно повторить.
type RateLimitError struct{ RetryAfter time.Duration }

func (err RateLimitError) Error() string { return ErrRateLimited.Error() }
func (err RateLimitError) Unwrap() error { return ErrRateLimited }

// RateLimitRetryAfter возвращает срок повтора из ошибки предела (не меньше секунды).
func RateLimitRetryAfter(err error) time.Duration {
	var limited RateLimitError
	if errors.As(err, &limited) && limited.RetryAfter > time.Second {
		return limited.RetryAfter
	}
	return time.Second
}

const (
	// PermissionOrganizationExport — право на выгрузку данных организации; у владельца (ADR 0059).
	PermissionOrganizationExport = "organization.export"

	// ScopeOrganizationExportTenant — предел выгрузок организации; список допустимых значений ограничен CHECK
	// таблицы auth_rate_limits (миграция 000026).
	ScopeOrganizationExportTenant = "ORGANIZATION_EXPORT_TENANT"

	organizationExportLimit  = 3
	organizationExportWindow = time.Hour

	operationOrganizationExported = "ORGANIZATION_DATA_EXPORTED"
	entityOrganization            = "ORGANIZATION"

	// deletionBatch — сколько назначенных удалений исполнитель берёт за один проход.
	deletionBatch = 20
	// noticeTTL — сколько письмо об исходе остаётся достойным отправки.
	noticeTTL = 24 * time.Hour
)

type IDs interface{ NewID() (string, error) }

// Passwords нужен исполнителю только для хэша случайного секрета.
type Passwords interface{ Hash(string) (string, error) }

// Permissions реализует tenant.PermissionService.
type Permissions interface {
	Allowed(ctx context.Context, actorID, tenantID, permission string) (bool, error)
}

// Mailer ставит письмо в очередь; контракт реализует модуль mail (ADR 0058).
type Mailer interface {
	Enqueue(ctx context.Context, kind, to, subject, text string, expiresAt time.Time) error
}

// RateLimiter должен быть общим для всех экземпляров API; в работе его реализует PostgreSQL (таблица
// auth_rate_limits, модуль identity), поэтому форма ответа — identity.RateLimitDecision.
type RateLimiter interface {
	Take(ctx context.Context, scope, subject string, limit int, window time.Duration, now time.Time) (identityapplication.RateLimitDecision, error)
}

// Reporter получает события исполнителя без адресов и текстов; *slog.Logger его реализует.
type Reporter interface {
	Info(message string, arguments ...any)
	Warn(message string, arguments ...any)
	Error(message string, arguments ...any)
}

// Snapshot — согласованный по времени снимок организации для выгрузки.
type Snapshot interface {
	// At — момент снимка; он же момент создания выгрузки.
	At() time.Time
	// Organization возвращает organization.json.
	Organization(ctx context.Context) ([]byte, error)
	// Rows вызывает emit для каждой строки набора (JSON-объект без перевода строки), в порядке набора.
	Rows(ctx context.Context, dataset domain.Dataset, emit func(line []byte) error) error
	Close(ctx context.Context) error
}

// DeletionStats — состояние назначенных удалений для периодической записи в журнал воркера.
type DeletionStats struct {
	// Scheduled — назначено и ещё не выполнено; Due — срок наступил; Overdue — срок наступил больше часа назад.
	Scheduled, Due, Overdue int
}

// Store — хранилище модуля. Читает и пишет таблицы других модулей по реестру домена (ADR 0059).
type Store interface {
	DeletionBlockers(ctx context.Context, userID string) ([]domain.Blocker, error)
	PersonalExport(ctx context.Context, userID string) (domain.PersonalExport, error)
	OpenSnapshot(ctx context.Context, tenantID string) (Snapshot, error)
	DueDeletions(ctx context.Context, now time.Time, limit int) ([]string, error)
	Erase(ctx context.Context, command domain.Erasure) (domain.ErasureResult, error)
	UserIDByEmail(ctx context.Context, email string) (string, bool, error)
	DeletionStats(ctx context.Context, now time.Time) (DeletionStats, error)
}

type Service struct {
	store       Store
	passwords   Passwords
	ids         IDs
	now         func() time.Time
	permissions Permissions
	auditor     auditapplication.Recorder
	limiter     RateLimiter
	mailer      Mailer
	baseURL     string
	reporter    Reporter
	// slots ограничивает одновременные выгрузки организаций в процессе; канал общий для копий службы.
	slots chan struct{}
}

func NewService(store Store, passwords Passwords, ids IDs, now func() time.Time) Service {
	return Service{
		store: store, passwords: passwords, ids: ids, now: now,
		slots: make(chan struct{}, domain.MaxConcurrentExports),
	}
}

// WithOrganizationExport подключает всё, что нужно выгрузке организации: проверку права, аудит и предел.
func (s Service) WithOrganizationExport(permissions Permissions, auditor auditapplication.Recorder, limiter RateLimiter) Service {
	s.permissions, s.auditor, s.limiter = permissions, auditor, limiter
	return s
}

// WithMail включает письма об исходе обезличивания; webBaseURL нужен для ссылок в письмах.
func (s Service) WithMail(mailer Mailer, webBaseURL string) Service {
	s.mailer, s.baseURL = mailer, strings.TrimRight(strings.TrimSpace(webBaseURL), "/")
	return s
}

// WithReporter включает запись исходов исполнителя в журнал.
func (s Service) WithReporter(reporter Reporter) Service {
	s.reporter = reporter
	return s
}

// WithExportSlots задаёт число одновременных выгрузок организаций (в тестах).
func (s Service) WithExportSlots(slots int) Service {
	if slots < 1 {
		slots = 1
	}
	s.slots = make(chan struct{}, slots)
	return s
}

// DeletionBlockers реализует контракт identity.AccountData: причины, по которым пользователя нельзя закрыть.
func (s Service) DeletionBlockers(ctx context.Context, userID string) ([]identityapplication.DeletionBlocker, error) {
	if s.store == nil {
		return nil, ErrNotConfigured
	}
	if userID == "" {
		return nil, domain.ErrInvalid
	}
	blockers, err := s.store.DeletionBlockers(ctx, userID)
	if err != nil {
		return nil, err
	}
	return toIdentityBlockers(blockers), nil
}

func toIdentityBlockers(blockers []domain.Blocker) []identityapplication.DeletionBlocker {
	result := make([]identityapplication.DeletionBlocker, 0, len(blockers))
	for _, blocker := range blockers {
		result = append(result, identityapplication.DeletionBlocker{
			Code: blocker.Code, OrganizationName: blocker.OrganizationName, HasOtherMembers: blocker.HasOtherMembers,
		})
	}
	return result
}

// PersonalExport реализует контракт identity.AccountData: документ личной выгрузки в виде JSON.
func (s Service) PersonalExport(ctx context.Context, userID string) ([]byte, error) {
	if s.store == nil || s.now == nil {
		return nil, ErrNotConfigured
	}
	if userID == "" {
		return nil, domain.ErrInvalid
	}
	document, err := s.store.PersonalExport(ctx, userID)
	if err != nil {
		return nil, err
	}
	document.Format = domain.PersonalExportFormat
	document.GeneratedAt = s.now().UTC()
	document.Note = domain.PersonalExportNote
	if document.AuthoredRecords == nil {
		document.AuthoredRecords = []domain.ExportAuthored{}
	}
	body, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode personal export: %w", err)
	}
	return append(body, '\n'), nil
}

// ExportOrganization пишет архив выгрузки организации в out. Всё, что может отказать (право, слот, предел, аудит,
// снимок), выполняется до первого записанного байта: транспорт отвечает обычной ошибкой, пока ничего не отправлено.
// Ошибка после первого байта означает оборванный архив без манифеста.
func (s Service) ExportOrganization(ctx context.Context, actorID, tenantID string, out io.Writer) error {
	if s.store == nil || s.permissions == nil || s.auditor == nil || s.limiter == nil || s.now == nil || out == nil {
		return ErrNotConfigured
	}
	if actorID == "" || tenantID == "" {
		return domain.ErrInvalid
	}
	allowed, err := s.permissions.Allowed(ctx, actorID, tenantID, PermissionOrganizationExport)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrForbidden
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		return ErrBusy
	}
	now := s.now().UTC()
	digest := sha256.Sum256([]byte(tenantID))
	decision, err := s.limiter.Take(ctx, ScopeOrganizationExportTenant, hex.EncodeToString(digest[:]), organizationExportLimit, organizationExportWindow, now)
	if err != nil {
		return err
	}
	if !decision.Allowed {
		return RateLimitError{RetryAfter: decision.ExpiresAt.Sub(now)}
	}
	// Запись аудита раньше первого байта: выгрузки без следа не бывает. Она фиксирует попытку, а не успех.
	if err := s.auditor.Tenant(ctx, auditapplication.TenantEntry(tenantID, actorID, operationOrganizationExported, entityOrganization, tenantID, now)); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, domain.ExportTimeout)
	defer cancel()
	snapshot, err := s.store.OpenSnapshot(ctx, tenantID)
	if err != nil {
		return err
	}
	defer func() { _ = snapshot.Close(context.WithoutCancel(ctx)) }()
	return writeArchive(ctx, out, snapshot)
}

// randomSecret возвращает 256 бит случайности; секрет существует только в памяти на время хэширования.
func randomSecret() (string, error) {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("generate secret: %w", err)
	}
	return hex.EncodeToString(buffer), nil
}

func (s Service) report(level, message string, arguments ...any) {
	if s.reporter == nil {
		return
	}
	switch level {
	case "warn":
		s.reporter.Warn(message, arguments...)
	case "error":
		s.reporter.Error(message, arguments...)
	default:
		s.reporter.Info(message, arguments...)
	}
}
