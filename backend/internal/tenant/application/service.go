// Package application coordinates tenant setup and central permission checks.
package application

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	auditapplication "lidradar/backend/internal/audit/application"
	identityapplication "lidradar/backend/internal/identity/application"
	"lidradar/backend/internal/tenant/domain"
	"lidradar/backend/platform/tenantctx"
)

var (
	ErrInvalid   = errors.New("invalid tenant request")
	ErrForbidden = errors.New("tenant permission denied")
	ErrNotFound  = errors.New("tenant resource not found")
	ErrConflict  = errors.New("tenant resource conflict")
)

const (
	PermissionRiskRead           = "risks.read"
	PermissionRiskManage         = "risks.manage"
	PermissionConversationRead   = "conversation.read"
	PermissionOpportunityManage  = "opportunity.manage"
	PermissionActionManage       = "action.manage"
	PermissionOutcomeManage      = "outcome.manage"
	PermissionRevenueConfirm     = "revenue.confirm"
	PermissionRevenueRead        = "revenue.read"
	PermissionAnalyticsRead      = "analytics.read"
	PermissionIntegrationManage  = "integration.manage"
	PermissionOrganizationManage = "organization.manage"
	PermissionLocationManage     = "location.manage"
	PermissionServiceManage      = "service.manage"
	PermissionNotificationManage = "notification.manage"
	PermissionMemberManage       = "member.manage"
)

var ownerPermissions = map[string]struct{}{
	PermissionRiskRead: {}, PermissionRiskManage: {}, PermissionConversationRead: {},
	PermissionOpportunityManage: {}, PermissionActionManage: {}, PermissionOutcomeManage: {},
	PermissionRevenueConfirm: {}, PermissionRevenueRead: {}, PermissionAnalyticsRead: {},
	PermissionIntegrationManage: {}, PermissionOrganizationManage: {}, PermissionLocationManage: {},
	PermissionServiceManage: {}, PermissionNotificationManage: {}, PermissionMemberManage: {},
}

var managerPermissions = map[string]struct{}{
	PermissionRiskRead: {}, PermissionRiskManage: {}, PermissionConversationRead: {},
	PermissionOpportunityManage: {}, PermissionActionManage: {}, PermissionOutcomeManage: {},
	PermissionRevenueConfirm: {},
}

type IDs interface{ NewID() (string, error) }

type PermissionService struct{ repository domain.Repository }

func NewPermissionService(repository domain.Repository) PermissionService {
	return PermissionService{repository: repository}
}

func (service PermissionService) Allowed(ctx context.Context, actorID, tenantID, permission string) (bool, error) {
	if service.repository == nil || actorID == "" || tenantID == "" || permission == "" {
		return false, nil
	}
	membership, found, err := service.repository.Membership(ctx, tenantID, actorID)
	if err != nil || !found || membership.Status != domain.MembershipActive {
		return false, err
	}
	permissions := managerPermissions
	if membership.Role == domain.RoleOwner {
		permissions = ownerPermissions
	}
	_, allowed := permissions[permission]
	return allowed, nil
}

func (service PermissionService) ActiveMember(ctx context.Context, actorID, tenantID string) (bool, error) {
	if service.repository == nil || actorID == "" || tenantID == "" {
		return false, nil
	}
	membership, found, err := service.repository.Membership(ctx, tenantID, actorID)
	return found && membership.Status == domain.MembershipActive, err
}

type Service struct {
	repository  domain.Repository
	permissions PermissionService
	ids         IDs
	now         func() time.Time
	auditor     auditapplication.Recorder
}

// WithAuditor включает аудит настроек организации и изменений состава (ТЗ §65).
func (service Service) WithAuditor(auditor auditapplication.Recorder) Service {
	service.auditor = auditor
	return service
}

func (service Service) audit(ctx context.Context, tenantID, actorID, operation, entityType, entityID string) error {
	if service.auditor == nil {
		return nil
	}
	return service.auditor.Tenant(ctx, auditapplication.TenantEntry(tenantID, actorID, operation, entityType, entityID, service.now()))
}

func NewService(repository domain.Repository, permissions PermissionService, ids IDs, now func() time.Time) Service {
	return Service{repository: repository, permissions: permissions, ids: ids, now: now}
}

func (service Service) CreateOrganization(ctx context.Context, actorID, name, timezone, currency string) (domain.Organization, error) {
	if !service.ready() || actorID == "" {
		return domain.Organization{}, ErrInvalid
	}
	organizationID, err := service.ids.NewID()
	if err != nil {
		return domain.Organization{}, err
	}
	membershipID, err := service.ids.NewID()
	if err != nil {
		return domain.Organization{}, err
	}
	now := service.now().UTC()
	organization, err := domain.NewOrganization(organizationID, name, timezone, currency, now)
	if err != nil {
		return domain.Organization{}, ErrInvalid
	}
	membership, err := domain.NewMembership(membershipID, organization.ID, actorID, domain.RoleOwner, now)
	if err != nil {
		return domain.Organization{}, ErrInvalid
	}
	// Новая организация ещё не в заголовке запроса: контекст RLS задаётся из её идентификатора.
	if err := service.repository.CreateOrganizationWithOwner(tenantctx.WithTenant(ctx, organization.ID), organization, membership); err != nil {
		if errors.Is(err, domain.ErrConflict) {
			return domain.Organization{}, ErrConflict
		}
		return domain.Organization{}, err
	}
	return organization, nil
}

func (service Service) GetOrganization(ctx context.Context, actorID, tenantID string) (domain.Organization, error) {
	if err := service.requireMember(ctx, actorID, tenantID); err != nil {
		return domain.Organization{}, err
	}
	organization, found, err := service.repository.Organization(ctx, tenantID)
	if err != nil {
		return domain.Organization{}, err
	}
	if !found {
		return domain.Organization{}, ErrNotFound
	}
	return organization, nil
}

type OrganizationUpdate struct {
	Name            *string
	DefaultTimezone *string
	DefaultCurrency *string
}

func (service Service) UpdateOrganization(ctx context.Context, actorID, tenantID string, update OrganizationUpdate) (domain.Organization, error) {
	if err := service.requirePermission(ctx, actorID, tenantID, PermissionOrganizationManage); err != nil {
		return domain.Organization{}, err
	}
	if update.Name == nil && update.DefaultTimezone == nil && update.DefaultCurrency == nil {
		return domain.Organization{}, ErrInvalid
	}
	organization, found, err := service.repository.Organization(ctx, tenantID)
	if err != nil {
		return domain.Organization{}, err
	}
	if !found {
		return domain.Organization{}, ErrNotFound
	}
	if update.Name != nil {
		organization.Name = strings.TrimSpace(*update.Name)
	}
	if update.DefaultTimezone != nil {
		organization.DefaultTimezone = strings.TrimSpace(*update.DefaultTimezone)
	}
	if update.DefaultCurrency != nil {
		organization.DefaultCurrency = strings.ToUpper(strings.TrimSpace(*update.DefaultCurrency))
	}
	organization.UpdatedAt = service.now().UTC()
	if organization.Validate() != nil {
		return domain.Organization{}, ErrInvalid
	}
	organization, found, err = service.repository.UpdateOrganization(ctx, tenantID, organization)
	if err != nil {
		return domain.Organization{}, err
	}
	if !found {
		return domain.Organization{}, ErrNotFound
	}
	if err := service.audit(ctx, tenantID, actorID, "ORGANIZATION_UPDATED", "ORGANIZATION", tenantID); err != nil {
		return domain.Organization{}, err
	}
	return organization, nil
}

func (service Service) AddMember(ctx context.Context, actorID, tenantID, userID string, role domain.Role) (domain.Membership, error) {
	if err := service.requirePermission(ctx, actorID, tenantID, PermissionMemberManage); err != nil {
		return domain.Membership{}, err
	}
	id, err := service.ids.NewID()
	if err != nil {
		return domain.Membership{}, err
	}
	membership, err := domain.NewMembership(id, tenantID, userID, role, service.now().UTC())
	if err != nil {
		return domain.Membership{}, ErrInvalid
	}
	if err := service.repository.CreateMembership(ctx, tenantID, membership); err != nil {
		if errors.Is(err, domain.ErrConflict) {
			return domain.Membership{}, ErrConflict
		}
		return domain.Membership{}, err
	}
	if err := service.audit(ctx, tenantID, actorID, "MEMBER_ADDED", "MEMBERSHIP", membership.ID); err != nil {
		return domain.Membership{}, err
	}
	return membership, nil
}

func (service Service) MembershipsForUser(ctx context.Context, userID string) ([]identityapplication.MembershipSummary, error) {
	if service.repository == nil || userID == "" {
		return nil, ErrInvalid
	}
	memberships, err := service.repository.MembershipsForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	result := make([]identityapplication.MembershipSummary, 0, len(memberships))
	for _, item := range memberships {
		if item.Membership.Status == domain.MembershipActive && item.Organization.Status == domain.OrganizationActive {
			result = append(result, identityapplication.MembershipSummary{TenantID: item.Organization.ID, OrganizationName: item.Organization.Name, Role: string(item.Membership.Role)})
		}
	}
	return result, nil
}

func (service Service) ListLocations(ctx context.Context, actorID, tenantID string) ([]domain.Location, error) {
	if err := service.requireMember(ctx, actorID, tenantID); err != nil {
		return nil, err
	}
	return service.repository.ListLocations(ctx, tenantID)
}

func (service Service) CreateLocation(ctx context.Context, actorID, tenantID, name, timezone string, threshold int) (domain.Location, error) {
	if err := service.requirePermission(ctx, actorID, tenantID, PermissionLocationManage); err != nil {
		return domain.Location{}, err
	}
	id, err := service.ids.NewID()
	if err != nil {
		return domain.Location{}, err
	}
	location, err := domain.NewLocation(id, tenantID, name, timezone, threshold, service.now().UTC())
	if err != nil {
		return domain.Location{}, ErrInvalid
	}
	if err := service.repository.CreateLocation(ctx, tenantID, location); err != nil {
		return domain.Location{}, err
	}
	return location, nil
}

type LocationUpdate struct {
	Name                     *string
	Timezone                 *string
	ResponseThresholdMinutes *int
	Active                   *bool
}

func (service Service) UpdateLocation(ctx context.Context, actorID, tenantID, locationID string, update LocationUpdate) (domain.Location, error) {
	if err := service.requirePermission(ctx, actorID, tenantID, PermissionLocationManage); err != nil {
		return domain.Location{}, err
	}
	if locationID == "" || update.Name == nil && update.Timezone == nil && update.ResponseThresholdMinutes == nil && update.Active == nil {
		return domain.Location{}, ErrInvalid
	}
	location, found, err := service.repository.Location(ctx, tenantID, locationID)
	if err != nil {
		return domain.Location{}, err
	}
	if !found {
		return domain.Location{}, ErrNotFound
	}
	if update.Name != nil {
		location.Name = strings.TrimSpace(*update.Name)
	}
	if update.Timezone != nil {
		location.Timezone = strings.TrimSpace(*update.Timezone)
	}
	if update.ResponseThresholdMinutes != nil {
		location.ResponseThresholdMinutes = *update.ResponseThresholdMinutes
	}
	if update.Active != nil {
		location.Active = *update.Active
	}
	location.UpdatedAt = service.now().UTC()
	if location.Validate() != nil {
		return domain.Location{}, ErrInvalid
	}
	location, found, err = service.repository.UpdateLocation(ctx, tenantID, locationID, location)
	if err != nil {
		return domain.Location{}, err
	}
	if !found {
		return domain.Location{}, ErrNotFound
	}
	return location, nil
}

type BusinessHourInput struct {
	Weekday  int
	Closed   bool
	OpensAt  string
	ClosesAt string
}

func (service Service) ReplaceBusinessHours(ctx context.Context, actorID, tenantID, locationID, timezone string, inputs []BusinessHourInput) (domain.Location, error) {
	if err := service.requirePermission(ctx, actorID, tenantID, PermissionLocationManage); err != nil {
		return domain.Location{}, err
	}
	timezone = strings.TrimSpace(timezone)
	if _, err := time.LoadLocation(timezone); err != nil || locationID == "" || len(inputs) != 7 {
		return domain.Location{}, ErrInvalid
	}
	seen := make(map[int]struct{}, 7)
	hours := make([]domain.BusinessHour, 0, 7)
	for _, input := range inputs {
		if _, duplicate := seen[input.Weekday]; duplicate {
			return domain.Location{}, ErrInvalid
		}
		seen[input.Weekday] = struct{}{}
		id, err := service.ids.NewID()
		if err != nil {
			return domain.Location{}, err
		}
		hour, err := domain.NewBusinessHour(id, tenantID, locationID, input.Weekday, input.Closed, input.OpensAt, input.ClosesAt)
		if err != nil {
			return domain.Location{}, ErrInvalid
		}
		hours = append(hours, hour)
	}
	location, found, err := service.repository.ReplaceBusinessHours(ctx, tenantID, locationID, timezone, hours, service.now().UTC())
	if err != nil {
		if errors.Is(err, domain.ErrInvalid) {
			return domain.Location{}, ErrInvalid
		}
		return domain.Location{}, err
	}
	if !found {
		return domain.Location{}, ErrNotFound
	}
	return location, nil
}

// MLConsent возвращает действующее согласие организации на наборы данных.
func (service Service) MLConsent(ctx context.Context, actorID, tenantID string) (domain.MLConsent, bool, error) {
	if !service.ready() {
		return domain.MLConsent{}, false, ErrInvalid
	}
	if err := service.requireMember(ctx, actorID, tenantID); err != nil {
		return domain.MLConsent{}, false, err
	}
	return service.repository.ActiveMLConsent(ctx, tenantID)
}

// GrantMLConsent выдаёт согласие от имени владельца; повтор возвращает
// действующее согласие без новой записи.
func (service Service) GrantMLConsent(ctx context.Context, actorID, tenantID string) (domain.MLConsent, bool, error) {
	if !service.ready() {
		return domain.MLConsent{}, false, ErrInvalid
	}
	if err := service.requirePermission(ctx, actorID, tenantID, PermissionOrganizationManage); err != nil {
		return domain.MLConsent{}, false, err
	}
	consentID, err := service.ids.NewID()
	if err != nil {
		return domain.MLConsent{}, false, err
	}
	auditID, err := service.ids.NewID()
	if err != nil {
		return domain.MLConsent{}, false, err
	}
	now := service.now().UTC()
	consent, err := domain.NewMLConsent(consentID, tenantID, actorID, now)
	if err != nil {
		return domain.MLConsent{}, false, ErrInvalid
	}
	return service.repository.GrantMLConsent(ctx, consent, domain.AuditEntry{
		ID: auditID, ActorID: actorID, Operation: "ML_CONSENT_GRANTED", EntityType: "ML_CONSENT", EntityID: consent.ID, At: now,
	})
}

// RevokeMLConsent отзывает действующее согласие; повтор без согласия
// возвращает false без ошибки.
func (service Service) RevokeMLConsent(ctx context.Context, actorID, tenantID string) (domain.MLConsent, bool, error) {
	if !service.ready() {
		return domain.MLConsent{}, false, ErrInvalid
	}
	if err := service.requirePermission(ctx, actorID, tenantID, PermissionOrganizationManage); err != nil {
		return domain.MLConsent{}, false, err
	}
	auditID, err := service.ids.NewID()
	if err != nil {
		return domain.MLConsent{}, false, err
	}
	now := service.now().UTC()
	return service.repository.RevokeMLConsent(ctx, tenantID, actorID, now, domain.AuditEntry{
		ID: auditID, ActorID: actorID, Operation: "ML_CONSENT_REVOKED", EntityType: "ML_CONSENT", At: now,
	})
}

func (service Service) ready() bool {
	return service.repository != nil && service.ids != nil && service.now != nil
}

func (service Service) requireMember(ctx context.Context, actorID, tenantID string) error {
	allowed, err := service.permissions.ActiveMember(ctx, actorID, tenantID)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrForbidden
	}
	return nil
}

func (service Service) requirePermission(ctx context.Context, actorID, tenantID, permission string) error {
	allowed, err := service.permissions.Allowed(ctx, actorID, tenantID, permission)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrForbidden
	}
	return nil
}

// Команда организации и приглашения (ADR 0045).

var (
	ErrLastOwner         = errors.New("the last active owner cannot be demoted or revoked")
	ErrMemberDisabled    = errors.New("membership is disabled")
	ErrInvitationExpired = errors.New("invitation has expired")
	ErrInvitationRevoked = errors.New("invitation was revoked")
	ErrInvitationUsed    = errors.New("invitation was already accepted")
	ErrAlreadyMember     = errors.New("user is already an active member")
)

// invitationCodeBytes задаёт энтропию кода приглашения: 256 бит, base64url
// без дополнения — 43 символа из [A-Za-z0-9_-].
const invitationCodeBytes = 32

var invitationCodePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// InvitationView — приглашение вместе с вычисленным состоянием.
type InvitationView struct {
	domain.Invitation
	Status domain.InvitationStatus `json:"status"`
}

func (service Service) invitationView(invitation domain.Invitation) InvitationView {
	return InvitationView{Invitation: invitation, Status: invitation.Status(service.now().UTC())}
}

// ListMembers перечисляет участников организации, включая отозванных; право
// member.manage, так как список раскрывает адреса почты.
func (service Service) ListMembers(ctx context.Context, actorID, tenantID string) ([]domain.Member, error) {
	if !service.ready() {
		return nil, ErrInvalid
	}
	if err := service.requirePermission(ctx, actorID, tenantID, PermissionMemberManage); err != nil {
		return nil, err
	}
	members, err := service.repository.ListMembers(ctx, tenantID)
	if err != nil {
		return nil, mapTeamError(err)
	}
	if members == nil {
		members = []domain.Member{}
	}
	return members, nil
}

// ChangeMemberRole меняет роль активного участника. Последнего активного
// владельца понизить нельзя; смена собственной роли подчиняется тому же правилу.
func (service Service) ChangeMemberRole(ctx context.Context, actorID, tenantID, userID string, role domain.Role) (domain.Membership, error) {
	if !service.ready() {
		return domain.Membership{}, ErrInvalid
	}
	if err := service.requirePermission(ctx, actorID, tenantID, PermissionMemberManage); err != nil {
		return domain.Membership{}, err
	}
	if userID == "" || (role != domain.RoleOwner && role != domain.RoleManager) {
		return domain.Membership{}, ErrInvalid
	}
	membership, err := service.repository.ChangeMemberRole(ctx, tenantID, userID, role, service.now().UTC())
	if err != nil {
		return domain.Membership{}, mapTeamError(err)
	}
	if err := service.audit(ctx, tenantID, actorID, "MEMBER_ROLE_CHANGED", "MEMBERSHIP", membership.ID); err != nil {
		return domain.Membership{}, err
	}
	return membership, nil
}

// RevokeMember отзывает доступ участника (статус DISABLED, строка остаётся).
// Повтор идемпотентен; последний активный владелец защищён.
func (service Service) RevokeMember(ctx context.Context, actorID, tenantID, userID string) error {
	if !service.ready() {
		return ErrInvalid
	}
	if err := service.requirePermission(ctx, actorID, tenantID, PermissionMemberManage); err != nil {
		return err
	}
	if userID == "" {
		return ErrInvalid
	}
	membership, changed, err := service.repository.RevokeMember(ctx, tenantID, userID, service.now().UTC())
	if err != nil {
		return mapTeamError(err)
	}
	if !changed {
		return nil
	}
	return service.audit(ctx, tenantID, actorID, "MEMBER_REVOKED", "MEMBERSHIP", membership.ID)
}

// CreateInvitation выпускает одноразовый код с ролью. Открытый код
// возвращается один раз и нигде не сохраняется; в базе остаётся SHA-256.
func (service Service) CreateInvitation(ctx context.Context, actorID, tenantID string, role domain.Role, note *string) (InvitationView, string, error) {
	if !service.ready() {
		return InvitationView{}, "", ErrInvalid
	}
	if err := service.requirePermission(ctx, actorID, tenantID, PermissionMemberManage); err != nil {
		return InvitationView{}, "", err
	}
	code, err := newInvitationCode()
	if err != nil {
		return InvitationView{}, "", err
	}
	invitationID, err := service.ids.NewID()
	if err != nil {
		return InvitationView{}, "", err
	}
	auditID, err := service.ids.NewID()
	if err != nil {
		return InvitationView{}, "", err
	}
	now := service.now().UTC()
	invitation, err := domain.NewInvitation(invitationID, tenantID, role, hashInvitationCode(code), note, actorID, now)
	if err != nil {
		return InvitationView{}, "", ErrInvalid
	}
	if err := service.repository.CreateInvitation(ctx, invitation, domain.AuditEntry{
		ID: auditID, ActorID: actorID, Operation: "MEMBER_INVITED", EntityType: "INVITATION", EntityID: invitation.ID, At: now,
	}); err != nil {
		return InvitationView{}, "", mapTeamError(err)
	}
	return service.invitationView(invitation), code, nil
}

func (service Service) ListInvitations(ctx context.Context, actorID, tenantID string) ([]InvitationView, error) {
	if !service.ready() {
		return nil, ErrInvalid
	}
	if err := service.requirePermission(ctx, actorID, tenantID, PermissionMemberManage); err != nil {
		return nil, err
	}
	invitations, err := service.repository.ListInvitations(ctx, tenantID)
	if err != nil {
		return nil, mapTeamError(err)
	}
	views := make([]InvitationView, 0, len(invitations))
	for _, invitation := range invitations {
		views = append(views, service.invitationView(invitation))
	}
	return views, nil
}

// RevokeInvitation отзывает ожидающее приглашение; принятое отозвать нельзя,
// повторный отзыв ничего не меняет.
func (service Service) RevokeInvitation(ctx context.Context, actorID, tenantID, invitationID string) error {
	if !service.ready() {
		return ErrInvalid
	}
	if err := service.requirePermission(ctx, actorID, tenantID, PermissionMemberManage); err != nil {
		return err
	}
	if invitationID == "" {
		return ErrInvalid
	}
	auditID, err := service.ids.NewID()
	if err != nil {
		return err
	}
	now := service.now().UTC()
	if _, err := service.repository.RevokeInvitation(ctx, tenantID, invitationID, actorID, now, domain.AuditEntry{
		ID: auditID, ActorID: actorID, Operation: "INVITATION_REVOKED", EntityType: "INVITATION", EntityID: invitationID, At: now,
	}); err != nil {
		return mapTeamError(err)
	}
	return nil
}

// AcceptInvitation принимает код от имени вошедшего пользователя без выбора
// организации: организация определяется приглашением. Отозванное ранее
// членство восстанавливается с ролью приглашения.
func (service Service) AcceptInvitation(ctx context.Context, userID, code string) (identityapplication.MembershipSummary, error) {
	if !service.ready() || userID == "" {
		return identityapplication.MembershipSummary{}, ErrInvalid
	}
	code = strings.TrimSpace(code)
	if !invitationCodePattern.MatchString(code) {
		return identityapplication.MembershipSummary{}, ErrInvalid
	}
	membershipID, err := service.ids.NewID()
	if err != nil {
		return identityapplication.MembershipSummary{}, err
	}
	auditID, err := service.ids.NewID()
	if err != nil {
		return identityapplication.MembershipSummary{}, err
	}
	account, err := service.repository.AcceptInvitation(ctx, domain.AcceptInvitationCommand{
		CodeHash: hashInvitationCode(code), UserID: userID, MembershipID: membershipID, At: service.now().UTC(), AuditID: auditID,
	})
	if err != nil {
		return identityapplication.MembershipSummary{}, mapTeamError(err)
	}
	return identityapplication.MembershipSummary{
		TenantID: account.Organization.ID, OrganizationName: account.Organization.Name, Role: string(account.Membership.Role),
	}, nil
}

// Onboarding выводит статус настройки организации из фактов; доступен любому
// активному участнику.
func (service Service) Onboarding(ctx context.Context, actorID, tenantID string) (domain.OnboardingStatus, error) {
	if !service.ready() {
		return domain.OnboardingStatus{}, ErrInvalid
	}
	if err := service.requireMember(ctx, actorID, tenantID); err != nil {
		return domain.OnboardingStatus{}, err
	}
	facts, err := service.repository.OnboardingFacts(ctx, tenantID, actorID)
	if err != nil {
		return domain.OnboardingStatus{}, mapTeamError(err)
	}
	return domain.OnboardingFrom(facts, service.now()), nil
}

func newInvitationCode() (string, error) {
	raw := make([]byte, invitationCodeBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate invitation code: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func hashInvitationCode(code string) string {
	digest := sha256.Sum256([]byte(code))
	return hex.EncodeToString(digest[:])
}

func mapTeamError(err error) error {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, domain.ErrConflict):
		return ErrConflict
	case errors.Is(err, domain.ErrInvalid):
		return ErrInvalid
	case errors.Is(err, domain.ErrLastOwner):
		return ErrLastOwner
	case errors.Is(err, domain.ErrMemberDisabled):
		return ErrMemberDisabled
	case errors.Is(err, domain.ErrInvitationExpired):
		return ErrInvitationExpired
	case errors.Is(err, domain.ErrInvitationRevoked):
		return ErrInvitationRevoked
	case errors.Is(err, domain.ErrInvitationUsed):
		return ErrInvitationUsed
	case errors.Is(err, domain.ErrAlreadyMember):
		return ErrAlreadyMember
	default:
		return err
	}
}
