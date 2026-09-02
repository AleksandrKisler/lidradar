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
	ListLocations(context.Context, string) ([]Location, error)
	Location(context.Context, string, string) (Location, bool, error)
	CreateLocation(context.Context, string, Location) error
	UpdateLocation(context.Context, string, string, Location) (Location, bool, error)
	ReplaceBusinessHours(context.Context, string, string, string, []BusinessHour, time.Time) (Location, bool, error)
}
