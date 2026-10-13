package infrastructure

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/internal/tenant/domain"
	"lidradar/backend/internal/testsupport"
	"lidradar/backend/platform/tenantctx"
)

// Право на создание организации (ADR 0057) тратится условным UPDATE в транзакции создания:
// «одно право — одна организация» должно выдерживать параллельные запросы, а отказ не оставлять следов.

var rightsNow = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

func codeHashOf(code string) string {
	digest := sha256.Sum256([]byte(code))
	return hex.EncodeToString(digest[:])
}

func insertUser(t *testing.T, pool *pgxpool.Pool, email string, rights int) string {
	t.Helper()
	id := newTeamID(t)
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO users(id, email, password_hash, display_name, status, organization_rights)
		VALUES ($1, $2, '$integration-test$', 'Создатель', 'ACTIVE', $3)`, id, email, rights); err != nil {
		t.Fatal(err)
	}
	return id
}

func organizationFor(t *testing.T, userID, name string) (domain.Organization, domain.Membership) {
	t.Helper()
	organization, err := domain.NewOrganization(newTeamID(t), name, "Europe/Moscow", "RUB", rightsNow)
	if err != nil {
		t.Fatal(err)
	}
	membership, err := domain.NewMembership(newTeamID(t), organization.ID, userID, domain.RoleOwner, rightsNow)
	if err != nil {
		t.Fatal(err)
	}
	return organization, membership
}

func rightsOf(t *testing.T, pool *pgxpool.Pool, userID string) int {
	t.Helper()
	var rights int
	if err := pool.QueryRow(context.Background(), `SELECT organization_rights FROM users WHERE id = $1`, userID).Scan(&rights); err != nil {
		t.Fatal(err)
	}
	return rights
}

func countOrganizationsOf(t *testing.T, pool *pgxpool.Pool, userID string) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM memberships WHERE user_id = $1`, userID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestCreateOrganizationSpendsTheCreatorsRightAtomically(t *testing.T) {
	pools := testsupport.PostgresRoles(t)
	repository := NewPostgresRepository(pools.App)
	ctx := context.Background()
	userID := insertUser(t, pools.Owner, "creator@example.com", 1)

	first, firstMembership := organizationFor(t, userID, "Первая")
	if err := repository.CreateOrganizationWithOwner(tenantctx.WithTenant(ctx, first.ID), first, firstMembership, domain.CreationNeedsRight); err != nil {
		t.Fatalf("первая организация по праву: %v", err)
	}
	if got := rightsOf(t, pools.Owner, userID); got != 0 {
		t.Fatalf("после создания осталось прав %d, ожидался ноль", got)
	}

	second, secondMembership := organizationFor(t, userID, "Вторая")
	if err := repository.CreateOrganizationWithOwner(tenantctx.WithTenant(ctx, second.ID), second, secondMembership, domain.CreationNeedsRight); !errors.Is(err, domain.ErrOrganizationRightMissing) {
		t.Fatalf("вторая организация без права = %v, want ErrOrganizationRightMissing", err)
	}
	var exists bool
	if err := pools.Owner.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM organizations WHERE id = $1)`, second.ID).Scan(&exists); err != nil || exists {
		t.Fatalf("отказ оставил организацию: exists=%v, %v", exists, err)
	}
	if got := countOrganizationsOf(t, pools.Owner, userID); got != 1 {
		t.Fatalf("членств %d, ожидалось одно", got)
	}

	// В открытом режиме право не проверяется и не тратится.
	third, thirdMembership := organizationFor(t, userID, "Третья")
	if err := repository.CreateOrganizationWithOwner(tenantctx.WithTenant(ctx, third.ID), third, thirdMembership, domain.CreationUnrestricted); err != nil {
		t.Fatalf("создание без ограничения: %v", err)
	}
	if got := rightsOf(t, pools.Owner, userID); got != 0 {
		t.Fatalf("прав %d: открытое создание не должно их менять", got)
	}
}

func TestParallelCreationsSpendASingleRightOnce(t *testing.T) {
	pools := testsupport.PostgresRoles(t)
	repository := NewPostgresRepository(pools.App)
	userID := insertUser(t, pools.Owner, "racer@example.com", 1)

	const attempts = 8
	results := make(chan error, attempts)
	var group sync.WaitGroup
	for i := 0; i < attempts; i++ {
		organization, membership := organizationFor(t, userID, fmt.Sprintf("Гонка %d", i))
		group.Add(1)
		go func() {
			defer group.Done()
			results <- repository.CreateOrganizationWithOwner(tenantctx.WithTenant(context.Background(), organization.ID), organization, membership, domain.CreationNeedsRight)
		}()
	}
	group.Wait()
	close(results)
	succeeded := 0
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case !errors.Is(err, domain.ErrOrganizationRightMissing):
			t.Errorf("проигравший получил %v, want ErrOrganizationRightMissing", err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("создано организаций %d при одном праве, ожидалась одна", succeeded)
	}
	if got := countOrganizationsOf(t, pools.Owner, userID); got != 1 {
		t.Fatalf("членств %d, ожидалось одно", got)
	}
	if got := rightsOf(t, pools.Owner, userID); got != 0 {
		t.Fatalf("прав %d, ожидался ноль", got)
	}
}

// Регистрация по коду сотрудника проверяет код до создания пользователя, то есть без сессии и организации в
// контексте: строка находится только по хешу, заданному на время транзакции, и ничего не меняется.
func TestInvitationByCodeHashFindsOnlyTheInvitationWithThatHash(t *testing.T) {
	pools := testsupport.PostgresRoles(t)
	ctx := context.Background()
	pair := testsupport.TwoTenants(t, ctx, pools.Owner)
	repository := NewPostgresRepository(pools.App)

	issue := func(tenantID, ownerID, code string) domain.Invitation {
		invitation, err := domain.NewInvitation(newTeamID(t), tenantID, domain.RoleManager, codeHashOf(code), nil, ownerID, rightsNow)
		if err != nil {
			t.Fatal(err)
		}
		audit := domain.AuditEntry{ID: newTeamID(t), ActorID: ownerID, Operation: "MEMBER_INVITED", EntityType: "INVITATION", At: rightsNow}
		if err := repository.CreateInvitation(tenantctx.WithTenant(ctx, tenantID), invitation, audit); err != nil {
			t.Fatal(err)
		}
		return invitation
	}
	first := issue(pair.A.TenantID, pair.A.UserID, "code-of-tenant-a")
	second := issue(pair.B.TenantID, pair.B.UserID, "code-of-tenant-b")

	for name, want := range map[string]domain.Invitation{"code-of-tenant-a": first, "code-of-tenant-b": second} {
		found, err := repository.InvitationByCodeHash(ctx, codeHashOf(name)) // без организации в контексте
		if err != nil || found.ID != want.ID || found.TenantID != want.TenantID || found.Status(rightsNow) != domain.InvitationPending {
			t.Errorf("InvitationByCodeHash(%s) = %+v, %v; want %s", name, found, err, want.ID)
		}
	}
	if _, err := repository.InvitationByCodeHash(ctx, codeHashOf("unknown")); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("неизвестный хеш = %v, want ErrNotFound", err)
	}
	if _, err := repository.InvitationByCodeHash(ctx, "short"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("хеш неверной длины = %v, want ErrInvalid", err)
	}
	// Проверка ничего не расходует: приглашение остаётся ожидающим.
	if again, err := repository.InvitationByCodeHash(ctx, codeHashOf("code-of-tenant-a")); err != nil || again.AcceptedAt != nil {
		t.Fatalf("после проверки приглашение изменилось: %+v, %v", again, err)
	}
}
