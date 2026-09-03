package application

import (
	"context"
	"testing"
	"time"

	"lidradar/backend/internal/tenant/domain"
)

type permissionRepository struct{ membership domain.Membership }

func (*permissionRepository) CreateOrganizationWithOwner(context.Context, domain.Organization, domain.Membership) error {
	return nil
}
func (*permissionRepository) Organization(context.Context, string) (domain.Organization, bool, error) {
	return domain.Organization{}, false, nil
}
func (*permissionRepository) UpdateOrganization(context.Context, string, domain.Organization) (domain.Organization, bool, error) {
	return domain.Organization{}, false, nil
}
func (r *permissionRepository) Membership(_ context.Context, tenantID, userID string) (domain.Membership, bool, error) {
	if r.membership.TenantID == tenantID && r.membership.UserID == userID {
		return r.membership, true, nil
	}
	return domain.Membership{}, false, nil
}
func (*permissionRepository) MembershipsForUser(context.Context, string) ([]domain.AccountMembership, error) {
	return nil, nil
}
func (*permissionRepository) CreateMembership(context.Context, string, domain.Membership) error {
	return nil
}
func (*permissionRepository) RevokeMembership(context.Context, string, string, time.Time) (bool, error) {
	return false, nil
}
func (*permissionRepository) ListLocations(context.Context, string) ([]domain.Location, error) {
	return nil, nil
}
func (*permissionRepository) Location(context.Context, string, string) (domain.Location, bool, error) {
	return domain.Location{}, false, nil
}
func (*permissionRepository) CreateLocation(context.Context, string, domain.Location) error {
	return nil
}
func (*permissionRepository) UpdateLocation(context.Context, string, string, domain.Location) (domain.Location, bool, error) {
	return domain.Location{}, false, nil
}
func (*permissionRepository) ReplaceBusinessHours(context.Context, string, string, string, []domain.BusinessHour, time.Time) (domain.Location, bool, error) {
	return domain.Location{}, false, nil
}
func (*permissionRepository) ActiveMLConsent(context.Context, string) (domain.MLConsent, bool, error) {
	return domain.MLConsent{}, false, nil
}
func (*permissionRepository) GrantMLConsent(context.Context, domain.MLConsent, domain.AuditEntry) (domain.MLConsent, bool, error) {
	return domain.MLConsent{}, false, nil
}
func (*permissionRepository) RevokeMLConsent(context.Context, string, string, time.Time, domain.AuditEntry) (domain.MLConsent, bool, error) {
	return domain.MLConsent{}, false, nil
}

func TestPermissionServiceUsesMembershipRoleMapping(t *testing.T) {
	repository := &permissionRepository{membership: domain.Membership{TenantID: "tenant", UserID: "user", Role: domain.RoleOwner, Status: domain.MembershipActive}}
	service := NewPermissionService(repository)
	for permission := range ownerPermissions {
		allowed, err := service.Allowed(context.Background(), "user", "tenant", permission)
		if err != nil || !allowed {
			t.Fatalf("OWNER permission %q = %v, %v", permission, allowed, err)
		}
	}

	repository.membership.Role = domain.RoleManager
	for permission := range managerPermissions {
		allowed, err := service.Allowed(context.Background(), "user", "tenant", permission)
		if err != nil || !allowed {
			t.Fatalf("MANAGER permission %q = %v, %v", permission, allowed, err)
		}
	}
	for _, permission := range []string{
		PermissionOrganizationManage, PermissionLocationManage, PermissionMemberManage,
		PermissionRevenueRead, PermissionAnalyticsRead,
	} {
		allowed, err := service.Allowed(context.Background(), "user", "tenant", permission)
		if err != nil || allowed {
			t.Fatalf("MANAGER forbidden permission %q = %v, %v", permission, allowed, err)
		}
	}

	allowed, err := service.Allowed(context.Background(), "user", "other-tenant", PermissionRiskRead)
	if err != nil || allowed {
		t.Fatalf("cross-tenant permission = %v, %v", allowed, err)
	}
}
