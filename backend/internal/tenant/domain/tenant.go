// Package domain owns the Organization tenant boundary, memberships and business locations.
package domain

import (
	"context"
	"errors"
	"strings"
	"time"
)

var (
	ErrInvalid  = errors.New("invalid tenant state")
	ErrNotFound = errors.New("tenant resource not found")
	ErrConflict = errors.New("tenant resource conflict")
	// ErrLastOwner защищает организацию от потери последнего активного владельца.
	ErrLastOwner = errors.New("the last active owner cannot be demoted or revoked")
	// ErrMemberDisabled — команда над отозванным членством: его роль не меняется.
	ErrMemberDisabled = errors.New("membership is disabled")
	// Состояния приглашения, при которых код нельзя использовать.
	ErrInvitationExpired = errors.New("invitation has expired")
	ErrInvitationRevoked = errors.New("invitation was revoked")
	ErrInvitationUsed    = errors.New("invitation was already accepted")
	// ErrAlreadyMember — пользователь уже активный участник организации.
	ErrAlreadyMember = errors.New("user is already an active member")
)

type OrganizationStatus string

const (
	OrganizationActive    OrganizationStatus = "ACTIVE"
	OrganizationSuspended OrganizationStatus = "SUSPENDED"
	OrganizationArchived  OrganizationStatus = "ARCHIVED"
)

type Organization struct {
	ID              string             `json:"id"`
	Name            string             `json:"name"`
	DefaultTimezone string             `json:"defaultTimezone"`
	DefaultCurrency string             `json:"defaultCurrency"`
	Status          OrganizationStatus `json:"status"`
	CreatedAt       time.Time          `json:"createdAt"`
	UpdatedAt       time.Time          `json:"updatedAt"`
}

func NewOrganization(id, name, timezone, currency string, at time.Time) (Organization, error) {
	organization := Organization{
		ID: id, Name: strings.TrimSpace(name), DefaultTimezone: strings.TrimSpace(timezone),
		DefaultCurrency: strings.ToUpper(strings.TrimSpace(currency)), Status: OrganizationActive,
		CreatedAt: at.UTC(), UpdatedAt: at.UTC(),
	}
	if organization.DefaultCurrency == "" {
		organization.DefaultCurrency = "RUB"
	}
	if organization.Validate() != nil || at.IsZero() {
		return Organization{}, ErrInvalid
	}
	return organization, nil
}

func (organization Organization) Validate() error {
	if organization.ID == "" || organization.Name == "" || len(organization.Name) > 200 ||
		!validTimezone(organization.DefaultTimezone) || len(organization.DefaultCurrency) != 3 ||
		organization.Status != OrganizationActive && organization.Status != OrganizationSuspended && organization.Status != OrganizationArchived {
		return ErrInvalid
	}
	for _, character := range organization.DefaultCurrency {
		if character < 'A' || character > 'Z' {
			return ErrInvalid
		}
	}
	return nil
}

type Role string

const (
	RoleOwner   Role = "OWNER"
	RoleManager Role = "MANAGER"
)

type MembershipStatus string

const (
	MembershipActive   MembershipStatus = "ACTIVE"
	MembershipInvited  MembershipStatus = "INVITED"
	MembershipDisabled MembershipStatus = "DISABLED"
)

// Membership не удаляется физически: на него ссылаются неизменяемые факты
// (выручка, действия, исходы, аудит). Отзыв доступа — статус DISABLED и
// RevokedAt; членство с любым другим статусом RevokedAt не имеет.
type Membership struct {
	ID        string           `json:"id"`
	TenantID  string           `json:"tenantId"`
	UserID    string           `json:"userId"`
	Role      Role             `json:"role"`
	Status    MembershipStatus `json:"status"`
	RevokedAt *time.Time       `json:"revokedAt,omitempty"`
	CreatedAt time.Time        `json:"createdAt"`
	UpdatedAt time.Time        `json:"updatedAt"`
}

func NewMembership(id, tenantID, userID string, role Role, at time.Time) (Membership, error) {
	if id == "" || tenantID == "" || userID == "" || (role != RoleOwner && role != RoleManager) || at.IsZero() {
		return Membership{}, ErrInvalid
	}
	at = at.UTC()
	return Membership{ID: id, TenantID: tenantID, UserID: userID, Role: role, Status: MembershipActive, CreatedAt: at, UpdatedAt: at}, nil
}

type AccountMembership struct {
	Membership   Membership
	Organization Organization
}

type Location struct {
	ID                       string         `json:"id"`
	TenantID                 string         `json:"-"`
	Name                     string         `json:"name"`
	Timezone                 string         `json:"timezone"`
	ResponseThresholdMinutes int            `json:"responseThresholdMinutes"`
	Active                   bool           `json:"active"`
	BusinessHours            []BusinessHour `json:"businessHours"`
	CreatedAt                time.Time      `json:"createdAt"`
	UpdatedAt                time.Time      `json:"updatedAt"`
}

func NewLocation(id, tenantID, name, timezone string, responseThresholdMinutes int, at time.Time) (Location, error) {
	if responseThresholdMinutes == 0 {
		responseThresholdMinutes = 45
	}
	location := Location{
		ID: id, TenantID: tenantID, Name: strings.TrimSpace(name), Timezone: strings.TrimSpace(timezone),
		ResponseThresholdMinutes: responseThresholdMinutes, Active: true,
		BusinessHours: []BusinessHour{}, CreatedAt: at.UTC(), UpdatedAt: at.UTC(),
	}
	if location.Validate() != nil || at.IsZero() {
		return Location{}, ErrInvalid
	}
	return location, nil
}

func (location Location) Validate() error {
	if location.ID == "" || location.TenantID == "" || location.Name == "" || len(location.Name) > 200 ||
		!validTimezone(location.Timezone) || location.ResponseThresholdMinutes < 1 || location.ResponseThresholdMinutes > 1440 {
		return ErrInvalid
	}
	return nil
}

type BusinessHour struct {
	ID         string `json:"-"`
	TenantID   string `json:"-"`
	LocationID string `json:"-"`
	Weekday    int    `json:"weekday"`
	Closed     bool   `json:"closed"`
	OpensAt    string `json:"opensAt,omitempty"`
	ClosesAt   string `json:"closesAt,omitempty"`
}

func NewBusinessHour(id, tenantID, locationID string, weekday int, closed bool, opensAt, closesAt string) (BusinessHour, error) {
	hour := BusinessHour{ID: id, TenantID: tenantID, LocationID: locationID, Weekday: weekday, Closed: closed, OpensAt: strings.TrimSpace(opensAt), ClosesAt: strings.TrimSpace(closesAt)}
	if id == "" || tenantID == "" || locationID == "" || weekday < 1 || weekday > 7 {
		return BusinessHour{}, ErrInvalid
	}
	if closed {
		if hour.OpensAt != "" || hour.ClosesAt != "" {
			return BusinessHour{}, ErrInvalid
		}
		return hour, nil
	}
	opens, openErr := time.Parse("15:04", hour.OpensAt)
	closes, closeErr := time.Parse("15:04", hour.ClosesAt)
	if openErr != nil || closeErr != nil || !opens.Before(closes) {
		return BusinessHour{}, ErrInvalid
	}
	return hour, nil
}

func validTimezone(value string) bool {
	if strings.TrimSpace(value) == "" {
		return false
	}
	_, err := time.LoadLocation(value)
	return err == nil
}

// Repository keeps every tenant-owned lookup explicitly scoped by tenant ID.
type Repository interface {
	CreateOrganizationWithOwner(context.Context, Organization, Membership) error
	Organization(context.Context, string) (Organization, bool, error)
	UpdateOrganization(context.Context, string, Organization) (Organization, bool, error)
	Membership(context.Context, string, string) (Membership, bool, error)
	MembershipsForUser(context.Context, string) ([]AccountMembership, error)
	CreateMembership(context.Context, string, Membership) error
	// RevokeMembership отзывает доступ, не удаляя строку. Повтор для уже
	// отозванного членства возвращает false без ошибки.
	RevokeMembership(context.Context, string, string, time.Time) (bool, error)
	// ListMembers перечисляет участников организации с учётными данными
	// пользователей, включая отозванных.
	ListMembers(context.Context, string) ([]Member, error)
	// ChangeMemberRole меняет роль активного участника; понижение последнего
	// активного владельца отклоняется ErrLastOwner, отозванное членство —
	// ErrMemberDisabled.
	ChangeMemberRole(context.Context, string, string, Role, time.Time) (Membership, error)
	// RevokeMember отзывает участника с защитой последнего владельца; повтор
	// для уже отозванного возвращает false без ошибки.
	RevokeMember(context.Context, string, string, time.Time) (Membership, bool, error)
	CreateInvitation(context.Context, Invitation, AuditEntry) error
	ListInvitations(context.Context, string) ([]Invitation, error)
	// RevokeInvitation отзывает ожидающее приглашение; принятое отклоняется
	// ErrInvitationUsed, уже отозванное возвращается без изменений.
	RevokeInvitation(context.Context, string, string, string, time.Time, AuditEntry) (Invitation, error)
	// AcceptInvitation находит приглашение по хешу кода, создаёт или
	// восстанавливает членство и помечает приглашение принятым одной
	// транзакцией; аудит записывается от имени нового участника.
	AcceptInvitation(context.Context, AcceptInvitationCommand) (AccountMembership, error)
	// OnboardingFacts читает факты настройки организации для расчёта статуса
	// онбординга (ADR 0045).
	OnboardingFacts(context.Context, string, string) (OnboardingFacts, error)
	ListLocations(context.Context, string) ([]Location, error)
	Location(context.Context, string, string) (Location, bool, error)
	CreateLocation(context.Context, string, Location) error
	UpdateLocation(context.Context, string, string, Location) (Location, bool, error)
	ReplaceBusinessHours(context.Context, string, string, string, []BusinessHour, time.Time) (Location, bool, error)
	ActiveMLConsent(context.Context, string) (MLConsent, bool, error)
	// GrantMLConsent сохраняет согласие и аудит; при действующем согласии
	// возвращает его без изменений и false.
	GrantMLConsent(context.Context, MLConsent, AuditEntry) (MLConsent, bool, error)
	// RevokeMLConsent отзывает действующее согласие, не удаляя строку;
	// без действующего согласия возвращает false.
	RevokeMLConsent(context.Context, string, string, time.Time, AuditEntry) (MLConsent, bool, error)
}

// MLConsentScopeDatasets — единственная область согласия MVP: наборы для
// обучения, настройки промптов и оценки за пределами оказания услуги (ТЗ §70).
const MLConsentScopeDatasets = "DATASETS"

// MLConsent — явное, активное и отзываемое согласие организации на
// использование реальных переписок и обратной связи в наборах данных
// (LR-BE-2107). Без действующего согласия данные служат только сервису.
type MLConsent struct {
	ID        string     `json:"id"`
	TenantID  string     `json:"-"`
	Scope     string     `json:"scope"`
	GrantedBy string     `json:"grantedBy"`
	GrantedAt time.Time  `json:"grantedAt"`
	RevokedBy *string    `json:"revokedBy,omitempty"`
	RevokedAt *time.Time `json:"revokedAt,omitempty"`
}

func NewMLConsent(id, tenantID, grantedBy string, at time.Time) (MLConsent, error) {
	consent := MLConsent{ID: id, TenantID: tenantID, Scope: MLConsentScopeDatasets, GrantedBy: grantedBy, GrantedAt: at.UTC()}
	if consent.Validate() != nil {
		return MLConsent{}, ErrInvalid
	}
	return consent, nil
}

func (consent MLConsent) Validate() error {
	if consent.ID == "" || consent.TenantID == "" || consent.Scope != MLConsentScopeDatasets || consent.GrantedBy == "" ||
		consent.GrantedAt.IsZero() || (consent.RevokedAt == nil) != (consent.RevokedBy == nil) ||
		(consent.RevokedAt != nil && (consent.RevokedAt.Before(consent.GrantedAt) || *consent.RevokedBy == "")) {
		return ErrInvalid
	}
	return nil
}

func (consent MLConsent) Active() bool { return consent.RevokedAt == nil }

// AuditEntry — запись аудита, сохраняемая вместе с изменением согласия.
type AuditEntry struct {
	ID, ActorID, Operation, EntityType, EntityID string
	At                                           time.Time
}

// InvitationTTL — срок действия кода приглашения.
const InvitationTTL = 7 * 24 * time.Hour

// InvitationNoteLimit — предел длины пометки владельца о приглашении.
const InvitationNoteLimit = 500

type InvitationStatus string

const (
	InvitationPending  InvitationStatus = "PENDING"
	InvitationAccepted InvitationStatus = "ACCEPTED"
	InvitationRevoked  InvitationStatus = "REVOKED"
	InvitationExpired  InvitationStatus = "EXPIRED"
)

// Invitation — одноразовый код приглашения в организацию с ролью. Сам код
// показывается владельцу один раз и хранится только как SHA-256 (ADR 0045).
type Invitation struct {
	ID         string     `json:"id"`
	TenantID   string     `json:"-"`
	Role       Role       `json:"role"`
	CodeHash   string     `json:"-"`
	Note       *string    `json:"note"`
	CreatedBy  string     `json:"createdBy"`
	CreatedAt  time.Time  `json:"createdAt"`
	ExpiresAt  time.Time  `json:"expiresAt"`
	AcceptedAt *time.Time `json:"acceptedAt"`
	AcceptedBy *string    `json:"acceptedBy"`
	RevokedAt  *time.Time `json:"revokedAt"`
	RevokedBy  *string    `json:"revokedBy"`
}

func NewInvitation(id, tenantID string, role Role, codeHash string, note *string, createdBy string, at time.Time) (Invitation, error) {
	if note != nil {
		cleaned := strings.TrimSpace(*note)
		if cleaned == "" {
			note = nil
		} else {
			note = &cleaned
		}
	}
	invitation := Invitation{
		ID: id, TenantID: tenantID, Role: role, CodeHash: codeHash, Note: note, CreatedBy: createdBy,
		CreatedAt: at.UTC(), ExpiresAt: at.UTC().Add(InvitationTTL),
	}
	if invitation.Validate() != nil {
		return Invitation{}, ErrInvalid
	}
	return invitation, nil
}

func (invitation Invitation) Validate() error {
	if invitation.ID == "" || invitation.TenantID == "" || (invitation.Role != RoleOwner && invitation.Role != RoleManager) ||
		len(invitation.CodeHash) != 64 || invitation.CreatedBy == "" || invitation.CreatedAt.IsZero() ||
		!invitation.ExpiresAt.After(invitation.CreatedAt) {
		return ErrInvalid
	}
	if invitation.Note != nil && (*invitation.Note == "" || *invitation.Note != strings.TrimSpace(*invitation.Note) ||
		len([]rune(*invitation.Note)) > InvitationNoteLimit) {
		return ErrInvalid
	}
	if (invitation.AcceptedAt == nil) != (invitation.AcceptedBy == nil) || (invitation.RevokedAt == nil) != (invitation.RevokedBy == nil) ||
		(invitation.AcceptedAt != nil && invitation.RevokedAt != nil) {
		return ErrInvalid
	}
	return nil
}

// Status выводит состояние приглашения на момент времени: отметки принятия и
// отзыва имеют приоритет над сроком действия.
func (invitation Invitation) Status(now time.Time) InvitationStatus {
	switch {
	case invitation.AcceptedAt != nil:
		return InvitationAccepted
	case invitation.RevokedAt != nil:
		return InvitationRevoked
	case !now.Before(invitation.ExpiresAt):
		return InvitationExpired
	default:
		return InvitationPending
	}
}

// AcceptInvitationCommand — данные приёма приглашения одной транзакцией.
type AcceptInvitationCommand struct {
	CodeHash     string
	UserID       string
	MembershipID string
	At           time.Time
	AuditID      string
}

// Member — участник организации для экрана команды: членство вместе с
// учётными данными пользователя (ADR 0045).
type Member struct {
	MembershipID string           `json:"membershipId"`
	UserID       string           `json:"userId"`
	Email        string           `json:"email"`
	DisplayName  string           `json:"displayName"`
	Role         Role             `json:"role"`
	Status       MembershipStatus `json:"status"`
	RevokedAt    *time.Time       `json:"revokedAt"`
	CreatedAt    time.Time        `json:"createdAt"`
	UpdatedAt    time.Time        `json:"updatedAt"`
}

// OnboardingFacts — факты настройки, из которых детерминированно выводится
// статус онбординга; сервер ничего не хранит про «пройденные шаги».
type OnboardingFacts struct {
	ActiveLocations       int  `json:"activeLocations"`
	LocationsWithSchedule int  `json:"locationsWithSchedule"`
	ActiveServices        int  `json:"activeServices"`
	Connections           int  `json:"connections"`
	LiveConnections       int  `json:"liveConnections"`
	TelegramLinked        bool `json:"telegramLinked"`
}

// Шаги онбординга в порядке экрана настройки компании.
const (
	OnboardingStepOrganization = "ORGANIZATION"
	OnboardingStepLocation     = "LOCATION"
	OnboardingStepServices     = "SERVICES"
	OnboardingStepChannel      = "CHANNEL"
	OnboardingStepTelegramLink = "TELEGRAM_LINK"
)

type OnboardingStep struct {
	Key      string `json:"key"`
	Required bool   `json:"required"`
	Done     bool   `json:"done"`
}

// OnboardingStatus — авторитетный статус онбординга: обязательные шаги —
// организация, активная точка с полным недельным графиком, активная услуга и
// хотя бы один не отключённый канал; личная привязка Telegram необязательна.
type OnboardingStatus struct {
	Complete   bool             `json:"complete"`
	NextStep   *string          `json:"nextStep"`
	Steps      []OnboardingStep `json:"steps"`
	Facts      OnboardingFacts  `json:"facts"`
	ComputedAt time.Time        `json:"computedAt"`
}

// OnboardingFrom выводит статус из фактов. Следующий шаг — первый
// невыполненный обязательный, затем первый невыполненный необязательный.
func OnboardingFrom(facts OnboardingFacts, at time.Time) OnboardingStatus {
	steps := []OnboardingStep{
		{Key: OnboardingStepOrganization, Required: true, Done: true},
		{Key: OnboardingStepLocation, Required: true, Done: facts.LocationsWithSchedule > 0},
		{Key: OnboardingStepServices, Required: true, Done: facts.ActiveServices > 0},
		{Key: OnboardingStepChannel, Required: true, Done: facts.LiveConnections > 0},
		{Key: OnboardingStepTelegramLink, Required: false, Done: facts.TelegramLinked},
	}
	status := OnboardingStatus{Complete: true, Steps: steps, Facts: facts, ComputedAt: at.UTC()}
	for _, step := range steps {
		if step.Required && !step.Done {
			status.Complete = false
			if status.NextStep == nil {
				key := step.Key
				status.NextStep = &key
			}
		}
	}
	if status.NextStep == nil {
		for _, step := range steps {
			if !step.Done {
				key := step.Key
				status.NextStep = &key
				break
			}
		}
	}
	return status
}
