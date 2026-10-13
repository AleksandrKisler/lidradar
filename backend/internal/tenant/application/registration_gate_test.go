package application

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	identityapplication "lidradar/backend/internal/identity/application"
	"lidradar/backend/internal/tenant/domain"
)

// Регистрация по приглашению (ADR 0057) опирается на два узких места модуля tenant: право на создание
// организации и контракт кодов сотрудников. Здесь проверяется то, что решает служба; транзакционная
// часть проверяется на PostgreSQL в тестах хранилища.

type gateRepository struct {
	*permissionRepository
	creations    []domain.OrganizationCreation
	createErr    error
	invitation   domain.Invitation
	invitationEr error
	acceptErr    error
}

func (r *gateRepository) CreateOrganizationWithOwner(_ context.Context, _ domain.Organization, _ domain.Membership, creation domain.OrganizationCreation) error {
	r.creations = append(r.creations, creation)
	return r.createErr
}
func (r *gateRepository) InvitationByCodeHash(context.Context, string) (domain.Invitation, error) {
	return r.invitation, r.invitationEr
}
func (r *gateRepository) AcceptInvitation(context.Context, domain.AcceptInvitationCommand) (domain.AccountMembership, error) {
	if r.acceptErr != nil {
		return domain.AccountMembership{}, r.acceptErr
	}
	return domain.AccountMembership{
		Membership:   domain.Membership{TenantID: "tenant", Role: domain.RoleManager},
		Organization: domain.Organization{ID: "tenant", Name: "Студия"},
	}, nil
}

type sequenceIDs struct{ next int }

func (s *sequenceIDs) NewID() (string, error) {
	s.next++
	return fmt.Sprintf("id-%d", s.next), nil
}

var gateNow = time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)

func newGateService(repository *gateRepository) Service {
	return NewService(repository, NewPermissionService(repository), &sequenceIDs{}, func() time.Time { return gateNow })
}

const gateCode = "G0000000000000000000000000000000000000000AA"

func TestOrganizationCreationAsksForARightOnlyWhenRequired(t *testing.T) {
	for name, tc := range map[string]struct {
		required bool
		want     domain.OrganizationCreation
	}{
		"открытый режим": {false, domain.CreationUnrestricted},
		"invite_only":    {true, domain.CreationNeedsRight},
	} {
		repository := &gateRepository{permissionRepository: &permissionRepository{}}
		service := newGateService(repository).WithOrganizationRights(tc.required)
		if _, err := service.CreateOrganization(context.Background(), "user-1", "Студия", "Europe/Moscow", "RUB"); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(repository.creations) != 1 || repository.creations[0] != tc.want {
			t.Errorf("%s: правило создания = %v, want %v", name, repository.creations, tc.want)
		}
	}
}

func TestMissingOrganizationRightBecomesAForbiddenCreation(t *testing.T) {
	repository := &gateRepository{permissionRepository: &permissionRepository{}, createErr: domain.ErrOrganizationRightMissing}
	service := newGateService(repository).WithOrganizationRights(true)
	if _, err := service.CreateOrganization(context.Background(), "user-1", "Студия", "Europe/Moscow", "RUB"); !errors.Is(err, ErrOrganizationCreationNotAllowed) {
		t.Fatalf("CreateOrganization() = %v, want ErrOrganizationCreationNotAllowed", err)
	}
}

func TestCheckMemberInvitationAcceptsOnlyAPendingCode(t *testing.T) {
	pending := domain.Invitation{ExpiresAt: gateNow.Add(time.Hour)}
	accepted := gateNow.Add(-time.Minute)
	revoked := gateNow.Add(-time.Minute)
	for name, tc := range map[string]struct {
		code       string
		invitation domain.Invitation
		lookupErr  error
		want       error // nil — код можно принять
		passErr    bool  // ошибка не должна превращаться в «недействителен»
	}{
		"ожидает":         {code: gateCode, invitation: pending},
		"неверный формат": {code: "short", invitation: pending, want: identityapplication.ErrInvitationInvalid},
		"нет такого":      {code: gateCode, lookupErr: domain.ErrNotFound, want: identityapplication.ErrInvitationInvalid},
		"принято":         {code: gateCode, invitation: domain.Invitation{ExpiresAt: gateNow.Add(time.Hour), AcceptedAt: &accepted}, want: identityapplication.ErrInvitationInvalid},
		"отозвано":        {code: gateCode, invitation: domain.Invitation{ExpiresAt: gateNow.Add(time.Hour), RevokedAt: &revoked}, want: identityapplication.ErrInvitationInvalid},
		"просрочено":      {code: gateCode, invitation: domain.Invitation{ExpiresAt: gateNow.Add(-time.Second)}, want: identityapplication.ErrInvitationInvalid},
		"сбой хранилища":  {code: gateCode, lookupErr: errors.New("база недоступна"), passErr: true},
	} {
		repository := &gateRepository{permissionRepository: &permissionRepository{}, invitation: tc.invitation, invitationEr: tc.lookupErr}
		err := newGateService(repository).CheckMemberInvitation(context.Background(), tc.code)
		switch {
		case tc.passErr:
			if err == nil || errors.Is(err, identityapplication.ErrInvitationInvalid) {
				t.Errorf("%s: %v, ожидалась обычная ошибка хранилища", name, err)
			}
		case !errors.Is(err, tc.want) || (tc.want == nil && err != nil):
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
}

func TestAcceptMemberInvitationSeparatesDefiniteRefusalsFromAmbiguousFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		err         error
		wantInvalid bool
	}{
		"принят":      {nil, false},
		"нет такого":  {domain.ErrNotFound, true},
		"просрочен":   {domain.ErrInvitationExpired, true},
		"отозван":     {domain.ErrInvitationRevoked, true},
		"использован": {domain.ErrInvitationUsed, true},
		"уже в орг.":  {domain.ErrAlreadyMember, true},
		"сбой":        {errors.New("соединение разорвано"), false},
	} {
		repository := &gateRepository{permissionRepository: &permissionRepository{}, acceptErr: tc.err}
		err := newGateService(repository).AcceptMemberInvitation(context.Background(), "user-2", gateCode)
		switch {
		case tc.err == nil && err != nil:
			t.Errorf("%s: %v, want nil", name, err)
		case tc.err != nil && errors.Is(err, identityapplication.ErrInvitationInvalid) != tc.wantInvalid:
			t.Errorf("%s: %v, wantInvalid=%v", name, err, tc.wantInvalid)
		case tc.err != nil && err == nil:
			t.Errorf("%s: ошибка потеряна", name)
		}
	}
}
