package application

import (
	"context"
	"strings"
	"time"

	auditapplication "lidradar/backend/internal/audit/application"
	"lidradar/backend/internal/notification/domain"
)

type PreferenceStore interface {
	Preferences(context.Context, string, string) ([]domain.Preference, error)
	// SavePreference создаёт или заменяет настройку типа риска, сохраняя
	// идентификатор и время создания существующей строки.
	SavePreference(context.Context, domain.Preference) (domain.Preference, error)
	DeletePreference(context.Context, string, string, domain.RiskType) (bool, error)
	Timezone(context.Context, string) (string, bool, error)
}

// PreferenceInput — полная замена настройки: частичных обновлений нет, чтобы
// клиент всегда видел и подтверждал согласованный набор полей.
type PreferenceInput struct {
	MinimumSeverity   string
	DeliveryMode      string
	InAppEnabled      bool
	TelegramEnabled   bool
	QuietHoursEnabled bool
	QuietHoursStart   *string
	QuietHoursEnd     *string
	DigestTime        string
}

// PreferenceView дополняет настройку часовым поясом организации, в котором
// считаются тихие часы и время сводки (ТЗ §3.7).
type PreferenceView struct {
	Preference domain.Preference
	Timezone   string
}

// PreferenceService управляет личными настройками активного участника.
type PreferenceService struct {
	store      PreferenceStore
	authorizer MembershipAuthorizer
	ids        IDs
	now        func() time.Time
	auditor    auditapplication.Recorder
}

// WithAuditor включает аудит изменения политики уведомлений (ТЗ §65).
func (service PreferenceService) WithAuditor(auditor auditapplication.Recorder) PreferenceService {
	service.auditor = auditor
	return service
}

func NewPreferenceService(store PreferenceStore, authorizer MembershipAuthorizer, ids IDs, now func() time.Time) PreferenceService {
	return PreferenceService{store: store, authorizer: authorizer, ids: ids, now: now}
}

// List возвращает настройку для каждого типа риска: сохранённую либо неявную.
func (service PreferenceService) List(ctx context.Context, actorID, tenantID string) ([]PreferenceView, error) {
	timezone, err := service.authorize(ctx, actorID, tenantID)
	if err != nil {
		return nil, err
	}
	stored, err := service.store.Preferences(ctx, tenantID, actorID)
	if err != nil {
		return nil, err
	}
	byType := make(map[domain.RiskType]domain.Preference, len(stored))
	for _, preference := range stored {
		byType[preference.RiskType] = preference
	}
	views := make([]PreferenceView, 0, len(domain.RiskTypes()))
	for _, riskType := range domain.RiskTypes() {
		preference, found := byType[riskType]
		if !found {
			preference = domain.DefaultPreference(tenantID, actorID, riskType)
		}
		views = append(views, PreferenceView{Preference: preference, Timezone: timezone})
	}
	return views, nil
}

func (service PreferenceService) Put(
	ctx context.Context,
	actorID, tenantID, riskType string,
	input PreferenceInput,
) (PreferenceView, error) {
	timezone, err := service.authorize(ctx, actorID, tenantID)
	if err != nil {
		return PreferenceView{}, err
	}
	preference, err := service.build(tenantID, actorID, domain.RiskType(strings.TrimSpace(riskType)), input)
	if err != nil {
		return PreferenceView{}, err
	}
	saved, err := service.store.SavePreference(ctx, preference)
	if err != nil {
		return PreferenceView{}, err
	}
	if service.auditor != nil {
		if err := service.auditor.Tenant(ctx, auditapplication.TenantEntry(
			tenantID, actorID, "NOTIFICATION_POLICY_CHANGED", "NOTIFICATION_PREFERENCE", saved.ID, service.now(),
		)); err != nil {
			return PreferenceView{}, err
		}
	}
	return PreferenceView{Preference: saved, Timezone: timezone}, nil
}

// Reset удаляет сохранённую настройку; тип снова действует по умолчанию.
func (service PreferenceService) Reset(ctx context.Context, actorID, tenantID, riskType string) error {
	if _, err := service.authorize(ctx, actorID, tenantID); err != nil {
		return err
	}
	parsed := domain.RiskType(strings.TrimSpace(riskType))
	if !domain.ValidRiskType(parsed) {
		return domain.ErrInvalidPreference
	}
	stored, err := service.store.Preferences(ctx, tenantID, actorID)
	if err != nil {
		return err
	}
	var storedID string
	for _, preference := range stored {
		if preference.RiskType == parsed {
			storedID = preference.ID
		}
	}
	deleted, err := service.store.DeletePreference(ctx, tenantID, actorID, parsed)
	if err != nil || !deleted || service.auditor == nil || storedID == "" {
		return err
	}
	return service.auditor.Tenant(ctx, auditapplication.TenantEntry(
		tenantID, actorID, "NOTIFICATION_POLICY_RESET", "NOTIFICATION_PREFERENCE", storedID, service.now(),
	))
}

func (service PreferenceService) authorize(ctx context.Context, actorID, tenantID string) (string, error) {
	if service.store == nil || service.authorizer == nil || service.ids == nil || service.now == nil || actorID == "" || tenantID == "" {
		return "", domain.ErrInvalidPreference
	}
	allowed, err := service.authorizer.ActiveMember(ctx, actorID, tenantID)
	if err != nil {
		return "", err
	}
	if !allowed {
		return "", ErrForbidden
	}
	timezone, found, err := service.store.Timezone(ctx, tenantID)
	if err != nil {
		return "", err
	}
	if !found {
		return "", ErrNotFound
	}
	return timezone, nil
}

func (service PreferenceService) build(tenantID, userID string, riskType domain.RiskType, input PreferenceInput) (domain.Preference, error) {
	if !domain.ValidRiskType(riskType) {
		return domain.Preference{}, domain.ErrInvalidPreference
	}
	id, err := service.ids.NewID()
	if err != nil {
		return domain.Preference{}, err
	}
	now := service.now().UTC()
	preference := domain.Preference{
		ID: id, TenantID: tenantID, UserID: userID, RiskType: riskType,
		MinimumSeverity: domain.Severity(strings.TrimSpace(input.MinimumSeverity)),
		DeliveryMode:    domain.DeliveryMode(strings.TrimSpace(input.DeliveryMode)),
		InAppEnabled:    input.InAppEnabled, TelegramEnabled: input.TelegramEnabled,
		QuietHoursEnabled: input.QuietHoursEnabled, CreatedAt: now, UpdatedAt: now,
	}
	if preference.DigestTime, err = domain.ParseClockTime(input.DigestTime); err != nil {
		return domain.Preference{}, err
	}
	if (input.QuietHoursStart == nil) != (input.QuietHoursEnd == nil) {
		return domain.Preference{}, domain.ErrInvalidPreference
	}
	if input.QuietHoursStart != nil {
		start, err := domain.ParseClockTime(*input.QuietHoursStart)
		if err != nil {
			return domain.Preference{}, err
		}
		end, err := domain.ParseClockTime(*input.QuietHoursEnd)
		if err != nil {
			return domain.Preference{}, err
		}
		preference.QuietHoursStart, preference.QuietHoursEnd = &start, &end
	}
	if err := preference.Validate(); err != nil {
		return domain.Preference{}, err
	}
	return preference, nil
}
