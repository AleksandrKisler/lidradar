package infrastructure_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"lidradar/backend/internal/lifecycle/domain"
	"lidradar/backend/internal/lifecycle/infrastructure"
)

// Блокировки закрытия, очередь исполнителя и вспомогательные чтения (ADR 0059).

func blockerCodes(blockers []domain.Blocker) []string {
	codes := []string{}
	for _, blocker := range blockers {
		codes = append(codes, blocker.Code)
	}
	return codes
}

func TestDeletionBlockersListEverySoleOwnerOrganizationAndPlatformRight(t *testing.T) {
	store, pools := newStore(t)
	ctx := context.Background()

	// Каждый случай получает свою организацию: состояния не накапливаются между случаями.
	type scenario struct {
		name    string
		prepare func(a org)
		user    func(a org) string
		want    func(a org) []domain.Blocker
	}
	owner := func(a org) string { return a.Owner }
	manager := func(a org) string { return a.Manager }
	soleOwner := func(others bool) func(a org) []domain.Blocker {
		return func(a org) []domain.Blocker {
			return []domain.Blocker{{Code: "SOLE_OWNER", OrganizationName: a.Name, HasOtherMembers: others}}
		}
	}
	none := func(org) []domain.Blocker { return []domain.Blocker{} }
	scenarios := []scenario{
		{"рядовой менеджер свободен", func(org) {}, manager, none},
		{"единственный владелец при других участниках", func(org) {}, owner, soleOwner(true)},
		{"единственный участник организации", func(a org) {
			must(t, pools.Owner, `UPDATE memberships SET status = 'DISABLED', revoked_at = $2 WHERE id = $1`, a.ManagerMembership, seedAt)
		}, owner, soleOwner(false)},
		{"второй активный владелец снимает блокировку", func(a org) {
			must(t, pools.Owner, `UPDATE memberships SET role = 'OWNER' WHERE id = $1`, a.ManagerMembership)
		}, owner, none},
		{"отозванный второй владелец не считается", func(a org) {
			must(t, pools.Owner, `UPDATE memberships SET role = 'OWNER', status = 'DISABLED', revoked_at = $2 WHERE id = $1`, a.ManagerMembership, seedAt)
		}, owner, soleOwner(false)},
		{"приостановленная организация блокирует так же", func(a org) {
			must(t, pools.Owner, `UPDATE organizations SET status = 'SUSPENDED' WHERE id = $1`, a.Tenant)
		}, owner, soleOwner(true)},
		{"архивная организация не блокирует", func(a org) {
			must(t, pools.Owner, `UPDATE organizations SET status = 'ARCHIVED' WHERE id = $1`, a.Tenant)
		}, owner, none},
		{"отключённое членство владельца не блокирует", func(a org) {
			must(t, pools.Owner, `UPDATE memberships SET status = 'DISABLED', revoked_at = $2 WHERE id = $1`, a.OwnerMembership, seedAt)
		}, owner, none},
		{"действующий платформенный администратор", func(a org) {
			must(t, pools.Owner, `INSERT INTO platform_admins(id, user_id, granted_at) VALUES ($1, $2, $3)`, newID(t), a.Manager, seedAt)
		}, manager, func(org) []domain.Blocker { return []domain.Blocker{{Code: "PLATFORM_ADMIN"}} }},
		{"отозванное право не блокирует", func(a org) {
			must(t, pools.Owner, `INSERT INTO platform_admins(id, user_id, granted_at, revoked_at) VALUES ($1, $2, $3, $4)`, newID(t), a.Manager, seedAt, seedAt.Add(time.Hour))
		}, manager, none},
	}
	for index, tc := range scenarios {
		a := seedOrganization(t, pools.Owner, string(rune('a'+index)))
		tc.prepare(a)
		got, err := store.DeletionBlockers(ctx, tc.user(a))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		want := tc.want(a)
		if len(got) != len(want) {
			t.Errorf("%s: блокировки %+v, ожидались %+v", tc.name, got, want)
			continue
		}
		for position := range got {
			if got[position] != want[position] {
				t.Errorf("%s: блокировки %+v, ожидались %+v", tc.name, got, want)
			}
		}
	}
}

func TestDeletionBlockersAccumulateAcrossOrganizations(t *testing.T) {
	store, pools := newStore(t)
	a := seedOrganization(t, pools.Owner, "a")
	b := seedOrganization(t, pools.Owner, "b")
	// Владелец a становится единственным владельцем ещё и организации b и администратором платформы.
	must(t, pools.Owner, `INSERT INTO memberships(id, tenant_id, user_id, role, status, created_at, updated_at) VALUES ($1, $2, $3, 'OWNER', 'ACTIVE', $4, $4)`,
		newID(t), b.Tenant, a.Owner, seedAt.Add(time.Hour))
	must(t, pools.Owner, `UPDATE memberships SET status = 'DISABLED', revoked_at = $2 WHERE id = $1`, b.OwnerMembership, seedAt)
	must(t, pools.Owner, `INSERT INTO platform_admins(id, user_id, granted_at) VALUES ($1, $2, $3)`, newID(t), a.Owner, seedAt)

	got, err := store.DeletionBlockers(context.Background(), a.Owner)
	if err != nil {
		t.Fatal(err)
	}
	if codes := blockerCodes(got); len(codes) != 3 || codes[0] != "SOLE_OWNER" || codes[1] != "SOLE_OWNER" || codes[2] != "PLATFORM_ADMIN" {
		t.Fatalf("blockers = %+v", got)
	}
	if got[0].OrganizationName != a.Name || got[1].OrganizationName != b.Name {
		t.Errorf("организации в порядке создания: %+v", got)
	}
}

func TestDeletionBlockersRejectEmptyInput(t *testing.T) {
	store, _ := newStore(t)
	if _, err := store.DeletionBlockers(context.Background(), ""); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("error = %v, want ErrInvalid", err)
	}
	if _, err := (*infrastructure.PostgresStore)(nil).DeletionBlockers(context.Background(), "x"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("nil store error = %v, want ErrInvalid", err)
	}
}

func TestDueDeletionsComeOldestFirstLimitedAndSkipDoneOnes(t *testing.T) {
	store, pools := newStore(t)
	ctx := context.Background()
	var due []string
	for index, label := range []string{"c", "a", "b", "d"} {
		o := seedOrganization(t, pools.Owner, label)
		// Сроки идут в обратном порядке создания: порядок очереди задаёт срок, а не идентификатор.
		must(t, pools.Owner, `UPDATE users SET deletion_requested_at = $2::timestamptz - interval '40 days', deletion_due_at = $2::timestamptz - make_interval(days => $3) WHERE id = $1`,
			o.Manager, seedAt, index+1)
		due = append([]string{o.Manager}, due...)
	}
	// Один не наступил, один уже обезличен, у одного ничего не назначено.
	future := seedUser(t, pools.Owner, "future@example.test", "Будущий")
	must(t, pools.Owner, `UPDATE users SET deletion_requested_at = $2, deletion_due_at = $3 WHERE id = $1`, future, seedAt, seedAt.Add(time.Hour))
	done := seedUser(t, pools.Owner, "done@example.test", "Готов")
	must(t, pools.Owner, `UPDATE users SET deletion_requested_at = $2::timestamptz - interval '40 days', deletion_due_at = $2::timestamptz - interval '10 days', status = 'DISABLED', deleted_at = $2 WHERE id = $1`, done, seedAt)
	seedUser(t, pools.Owner, "plain@example.test", "Обычный")

	got, err := store.DueDeletions(ctx, seedAt, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(due) {
		t.Fatalf("due = %v, ожидались %v", got, due)
	}
	for index := range got {
		if got[index] != due[index] {
			t.Errorf("порядок очереди: %v, ожидался %v", got, due)
			break
		}
	}
	limited, err := store.DueDeletions(ctx, seedAt, 2)
	if err != nil || len(limited) != 2 || limited[0] != due[0] || limited[1] != due[1] {
		t.Errorf("limit 2 = %v, %v", limited, err)
	}
	// Срок наступает ровно в свою секунду.
	exact, err := store.DueDeletions(ctx, seedAt.Add(-4*24*time.Hour), 100)
	if err != nil || len(exact) != 1 || exact[0] != due[0] {
		t.Errorf("граница срока: %v, %v", exact, err)
	}
	if _, err := store.DueDeletions(ctx, seedAt, 0); !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("limit 0 error = %v, want ErrInvalid", err)
	}
}

func TestDeletionStatsCountScheduledDueAndOverdue(t *testing.T) {
	store, pools := newStore(t)
	ctx := context.Background()
	for index, hours := range []int{-3, -1, 0, 5} { // срок: 3 часа назад, час назад, сейчас, через 5 часов
		user := seedUser(t, pools.Owner, "u"+string(rune('a'+index))+"@example.test", "Пользователь")
		must(t, pools.Owner, `UPDATE users SET deletion_requested_at = $2::timestamptz - interval '31 days', deletion_due_at = $2::timestamptz + make_interval(hours => $3) WHERE id = $1`,
			user, seedAt, hours)
	}
	done := seedUser(t, pools.Owner, "done@example.test", "Готов")
	must(t, pools.Owner, `UPDATE users SET deletion_requested_at = $2::timestamptz - interval '40 days', deletion_due_at = $2::timestamptz - interval '10 days', status = 'DISABLED', deleted_at = $2 WHERE id = $1`, done, seedAt)

	stats, err := store.DeletionStats(ctx, seedAt)
	if err != nil {
		t.Fatal(err)
	}
	// Назначено 4; срок наступил у трёх (включая «сейчас»); просрочено больше часа у двух (-3 и -1: граница включительно).
	if stats.Scheduled != 4 || stats.Due != 3 || stats.Overdue != 2 {
		t.Errorf("stats = %+v, want scheduled 4, due 3, overdue 2", stats)
	}
}

func TestUserIDByEmailFindsExactlyTheNormalizedAddress(t *testing.T) {
	store, pools := newStore(t)
	a := seedOrganization(t, pools.Owner, "a")
	id, found, err := store.UserIDByEmail(context.Background(), a.OwnerEmail)
	if err != nil || !found || id != a.Owner {
		t.Fatalf("UserIDByEmail() = %q, %v, %v", id, found, err)
	}
	if _, found, err := store.UserIDByEmail(context.Background(), "nobody@example.test"); err != nil || found {
		t.Errorf("неизвестный адрес: %v, %v", found, err)
	}
	// Начало и подстрока адреса не находят пользователя: команда необратима, совпадение должно быть полным.
	for _, partial := range []string{a.OwnerEmail[:len(a.OwnerEmail)-3], a.OwnerEmail[1:], "%", "_"} {
		if _, found, err := store.UserIDByEmail(context.Background(), partial); err != nil || found {
			t.Errorf("часть адреса %q нашла пользователя: %v, %v", partial, found, err)
		}
	}
	if _, _, err := store.UserIDByEmail(context.Background(), ""); !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("пустой адрес error = %v", err)
	}
}
