package infrastructure_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/internal/lifecycle/domain"
	"lidradar/backend/internal/lifecycle/infrastructure"
)

// Обезличивание на настоящей PostgreSQL (ADR 0059): что стирается, что остаётся, что не затронуто, атомарность и
// поведение при гонках с командами организации и входом.

const erasedPasswordHash = "$argon2id$unusable-for-tests"

// scheduleDue назначает пользователю удаление, срок которого уже наступил к seedAt.
func scheduleDue(t *testing.T, pool *pgxpool.Pool, userID string) {
	t.Helper()
	must(t, pool, `UPDATE users SET deletion_requested_at = $2::timestamptz - interval '31 days', deletion_due_at = $2::timestamptz - interval '1 day' WHERE id = $1`, userID, seedAt)
}

func dueCommand(t *testing.T, userID string) domain.Erasure {
	t.Helper()
	return domain.Erasure{
		UserID: userID, Mode: domain.ModeDue, At: seedAt, PasswordHash: erasedPasswordHash, AuditID: newID(t),
	}
}

func operatorCommand(t *testing.T, userID string, dryRun bool) domain.Erasure {
	t.Helper()
	return domain.Erasure{
		UserID: userID, Mode: domain.ModeImmediate, At: seedAt, PasswordHash: erasedPasswordHash, AuditID: newID(t),
		AdminAuditID: newID(t), Note: "обращение №17", DryRun: dryRun,
	}
}

// fingerprint — отпечаток всех таблиц схемы: число строк и свёртка содержимого. Нужен, чтобы показать, что
// операция не тронула ничего лишнего.
func fingerprint(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT table_name FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_type = 'BASE TABLE' ORDER BY table_name`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, table)
	}
	rows.Close()
	result := map[string]string{}
	for _, table := range tables {
		if table == "schema_migrations" {
			continue
		}
		var count int
		var checksum string
		if err := pool.QueryRow(context.Background(),
			fmt.Sprintf(`SELECT count(*), COALESCE(md5(string_agg(x::text, '|' ORDER BY x::text)), '') FROM %s x`, table)).Scan(&count, &checksum); err != nil {
			t.Fatalf("fingerprint %s: %v", table, err)
		}
		result[table] = fmt.Sprintf("%d:%s", count, checksum)
	}
	return result
}

func diff(before, after map[string]string) []string {
	var changed []string
	for table, value := range after {
		if before[table] != value {
			changed = append(changed, table)
		}
	}
	sort.Strings(changed)
	return changed
}

func TestEraseAnonymizesTheUserAndTouchesNobodyElse(t *testing.T) {
	store, pools := newStore(t)
	a := seedOrganization(t, pools.Owner, "a")
	seedOrganization(t, pools.Owner, "b")
	scheduleDue(t, pools.Owner, a.Manager)
	// Право создать организацию личное и не должно пережить обезличивание; у остальных оно остаётся.
	must(t, pools.Owner, `UPDATE users SET organization_rights = 2 WHERE id IN ($1, $2)`, a.Manager, a.Owner)
	before := fingerprint(t, pools.Owner)

	result, err := store.Erase(context.Background(), dueCommand(t, a.Manager))
	if err != nil {
		t.Fatalf("Erase() error = %v", err)
	}
	if got := countOf(t, pools.Owner, `SELECT organization_rights FROM users WHERE id = $1`, a.Owner); got != 2 {
		t.Errorf("право владельца организации: %d, ожидалось 2", got)
	}
	if result.Outcome != domain.ErasureDone || result.Email != a.ManagerEmail {
		t.Fatalf("result = %+v", result)
	}
	wantCounts := map[string]int64{
		"telegram_callback_commands": 1, "telegram_link_tokens": 1, "telegram_user_links": 1, "notification_digest_items": 1,
		"notification_preferences": 1, "notifications": 1, "sessions": 2, "account_tokens": 1, "membership_invitations": 0,
		"memberships": 1, "users": 1,
	}
	for table, want := range wantCounts {
		if result.Counts[table] != want {
			t.Errorf("затронуто строк в %s: %d, ожидалось %d", table, result.Counts[table], want)
		}
	}

	// Ровно эти таблицы изменились; остальные, включая организацию b и владельца a, не тронуты.
	changed := diff(before, fingerprint(t, pools.Owner))
	wantChanged := []string{
		"account_tokens", "auth_audit_log", "memberships", "notification_deliveries", "notification_digest_items",
		"notification_preferences", "notifications", "sessions", "telegram_callback_commands", "telegram_link_tokens",
		"telegram_user_links", "users",
	}
	if strings.Join(changed, ",") != strings.Join(wantChanged, ",") {
		t.Errorf("изменились таблицы %v, ожидались %v", changed, wantChanged)
	}

	// Пользователь обезличен на месте.
	var email, name, hash, status string
	var verified, deleted, requested *time.Time
	var rights int
	if err := pools.Owner.QueryRow(context.Background(), `
		SELECT email, display_name, password_hash, status, email_verified_at, deleted_at, deletion_requested_at, organization_rights FROM users WHERE id = $1`,
		a.Manager).Scan(&email, &name, &hash, &status, &verified, &deleted, &requested, &rights); err != nil {
		t.Fatal(err)
	}
	if email != a.Manager+"@deleted.invalid" || name != "Удалён" || hash != erasedPasswordHash || status != "DISABLED" ||
		verified != nil || deleted == nil || !deleted.Equal(seedAt) || requested == nil || rights != 0 {
		t.Errorf("users = %q %q %q %q verified=%v deleted=%v requested=%v rights=%d", email, name, hash, status, verified, deleted, requested, rights)
	}
	// Стёртое стёрто.
	for _, table := range []string{"sessions", "account_tokens", "notifications", "notification_preferences", "notification_digest_items",
		"telegram_user_links", "telegram_link_tokens", "telegram_callback_commands"} {
		if got := countOf(t, pools.Owner, fmt.Sprintf(`SELECT count(*) FROM %s WHERE user_id = $1`, table), a.Manager); got != 0 {
			t.Errorf("в %s осталось %d строк пользователя", table, got)
		}
	}
	if got := countOf(t, pools.Owner, `SELECT count(*) FROM notification_deliveries WHERE notification_id = $1`, a.Notification); got != 0 {
		t.Errorf("доставки удалённых уведомлений остались: %d", got)
	}
	// Остаётся то, на что ссылаются неизменяемые факты, но без личных данных.
	for name, query := range map[string]string{
		"вердикт":              `SELECT count(*) FROM risk_feedback WHERE actor_user_id = $1`,
		"смена стадии":         `SELECT count(*) FROM opportunity_stage_history WHERE actor_user_id = $1`,
		"отзыв согласия":       `SELECT count(*) FROM ml_consents WHERE revoked_by = $1`,
		"принятое приглашение": `SELECT count(*) FROM membership_invitations WHERE accepted_by_user_id = $1`,
		"журнал входов":        `SELECT count(*) FROM auth_audit_log WHERE user_id = $1 AND operation IN ('USER_LOGGED_IN', 'PASSWORD_CHANGED')`,
	} {
		if got := countOf(t, pools.Owner, query, a.Manager); got == 0 {
			t.Errorf("запись %q исчезла: на неё ссылаются неизменяемые таблицы", name)
		}
	}
	var membershipStatus string
	var revoked *time.Time
	if err := pools.Owner.QueryRow(context.Background(), `SELECT status, revoked_at FROM memberships WHERE id = $1`, a.ManagerMembership).Scan(&membershipStatus, &revoked); err != nil {
		t.Fatal(err)
	}
	if membershipStatus != "DISABLED" || revoked == nil || !revoked.Equal(seedAt) {
		t.Errorf("членство: %s, revoked_at=%v", membershipStatus, revoked)
	}
	// След исполнения: запись журнала без адреса.
	var operations, addresses int
	if err := pools.Owner.QueryRow(context.Background(), `
		SELECT count(*), count(ip_address) FROM auth_audit_log WHERE user_id = $1 AND operation = 'ACCOUNT_ANONYMIZED'`, a.Manager).Scan(&operations, &addresses); err != nil {
		t.Fatal(err)
	}
	if operations != 1 || addresses != 0 {
		t.Errorf("ACCOUNT_ANONYMIZED: записей %d, с адресом %d", operations, addresses)
	}
	// Организация продолжает работать: владелец активен.
	if got := countOf(t, pools.Owner, `SELECT count(*) FROM memberships WHERE tenant_id = $1 AND role = 'OWNER' AND status = 'ACTIVE'`, a.Tenant); got != 1 {
		t.Errorf("активных владельцев: %d", got)
	}
}

func TestEraseDoesNotTouchTheOwnersOrOtherOrganizations(t *testing.T) {
	store, pools := newStore(t)
	a := seedOrganization(t, pools.Owner, "a")
	b := seedOrganization(t, pools.Owner, "b")
	// Тот же менеджер состоит и в организации b: его членство там тоже закрывается, чужие записи нет.
	must(t, pools.Owner, `INSERT INTO memberships(id, tenant_id, user_id, role, status, created_at, updated_at) VALUES ($1, $2, $3, 'MANAGER', 'ACTIVE', $4, $4)`,
		newID(t), b.Tenant, a.Manager, seedAt)
	scheduleDue(t, pools.Owner, a.Manager)
	if _, err := store.Erase(context.Background(), dueCommand(t, a.Manager)); err != nil {
		t.Fatal(err)
	}
	if got := countOf(t, pools.Owner, `SELECT count(*) FROM memberships WHERE user_id = $1 AND status = 'ACTIVE'`, a.Manager); got != 0 {
		t.Errorf("у обезличенного остались активные членства: %d", got)
	}
	for _, o := range []org{a, b} {
		for name, query := range map[string]string{
			"владелец": `SELECT count(*) FROM users WHERE id = $1 AND status = 'ACTIVE' AND email = $2`,
		} {
			if got := countOf(t, pools.Owner, query, o.Owner, o.OwnerEmail); got != 1 {
				t.Errorf("%s %s изменён", name, describe(o))
			}
		}
		if got := countOf(t, pools.Owner, `SELECT count(*) FROM sessions WHERE user_id = $1`, o.Owner); got != 2 {
			t.Errorf("сессии владельца %s: %d", describe(o), got)
		}
		if got := countOf(t, pools.Owner, `SELECT count(*) FROM notification_preferences WHERE user_id = $1`, o.Owner); got != 1 {
			t.Errorf("настройки владельца %s: %d", describe(o), got)
		}
	}
	// Настройки менеджера b не существовали, чужие настройки b сохранены у его владельца.
	if got := countOf(t, pools.Owner, `SELECT count(*) FROM notification_preferences WHERE tenant_id = $1`, b.Tenant); got != 2 {
		t.Errorf("настройки организации b: %d, ожидалось 2 (владелец и собственный менеджер b)", got)
	}
}

func TestEraseRevokesPendingInvitationsCreatedByTheUser(t *testing.T) {
	store, pools := newStore(t)
	a := seedOrganization(t, pools.Owner, "a")
	pending, accepted := newID(t), newID(t)
	must(t, pools.Owner, `INSERT INTO membership_invitations(id, tenant_id, role, code_hash, created_by_user_id, created_at, expires_at) VALUES ($1, $2, 'MANAGER', $3, $4, $5, $6)`,
		pending, a.Tenant, digest("pending-by-manager"), a.Manager, seedAt, seedAt.Add(7*24*time.Hour))
	must(t, pools.Owner, `INSERT INTO membership_invitations(id, tenant_id, role, code_hash, created_by_user_id, created_at, expires_at, accepted_at, accepted_by_user_id) VALUES ($1, $2, 'MANAGER', $3, $4, $5, $6, $5, $7)`,
		accepted, a.Tenant, digest("accepted-by-manager"), a.Manager, seedAt, seedAt.Add(7*24*time.Hour), a.Owner)
	scheduleDue(t, pools.Owner, a.Manager)

	result, err := store.Erase(context.Background(), dueCommand(t, a.Manager))
	if err != nil || result.Counts["membership_invitations"] != 1 {
		t.Fatalf("Erase() = %+v, %v", result, err)
	}
	var revokedAt *time.Time
	var revokedBy *string
	if err := pools.Owner.QueryRow(context.Background(), `SELECT revoked_at, revoked_by_user_id::text FROM membership_invitations WHERE id = $1`, pending).Scan(&revokedAt, &revokedBy); err != nil {
		t.Fatal(err)
	}
	if revokedAt == nil || revokedBy == nil || *revokedBy != a.Manager {
		t.Errorf("неиспользованное приглашение не отозвано: %v %v", revokedAt, revokedBy)
	}
	if err := pools.Owner.QueryRow(context.Background(), `SELECT revoked_at, revoked_by_user_id::text FROM membership_invitations WHERE id = $1`, accepted).Scan(&revokedAt, &revokedBy); err != nil {
		t.Fatal(err)
	}
	if revokedAt != nil || revokedBy != nil {
		t.Errorf("принятое приглашение изменено: %v %v", revokedAt, revokedBy)
	}
	// Приглашения владельца не затронуты.
	if got := countOf(t, pools.Owner, `SELECT count(*) FROM membership_invitations WHERE id = $1 AND revoked_at IS NULL AND accepted_at IS NULL`, a.Invitation); got != 1 {
		t.Error("приглашение владельца отозвано")
	}
}

func TestEraseOfASoleOwnerCancelsTheRequestAndLeavesTheAccountActive(t *testing.T) {
	store, pools := newStore(t)
	a := seedOrganization(t, pools.Owner, "a")
	scheduleDue(t, pools.Owner, a.Owner)
	before := fingerprint(t, pools.Owner)

	result, err := store.Erase(context.Background(), dueCommand(t, a.Owner))
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != domain.ErasureBlocked || result.Email != a.OwnerEmail || len(result.Blockers) != 1 ||
		result.Blockers[0] != (domain.Blocker{Code: "SOLE_OWNER", OrganizationName: a.Name, HasOtherMembers: true}) {
		t.Fatalf("result = %+v", result)
	}
	if changed := diff(before, fingerprint(t, pools.Owner)); strings.Join(changed, ",") != "auth_audit_log,users" {
		t.Errorf("изменились таблицы %v, ожидались только users и auth_audit_log", changed)
	}
	var status string
	var requested, due, deleted *time.Time
	if err := pools.Owner.QueryRow(context.Background(), `SELECT status, deletion_requested_at, deletion_due_at, deleted_at FROM users WHERE id = $1`, a.Owner).Scan(&status, &requested, &due, &deleted); err != nil {
		t.Fatal(err)
	}
	if status != "ACTIVE" || requested != nil || due != nil || deleted != nil {
		t.Errorf("запрос не снят: %s %v %v %v", status, requested, due, deleted)
	}
	if got := countOf(t, pools.Owner, `SELECT count(*) FROM auth_audit_log WHERE user_id = $1 AND operation = 'ACCOUNT_DELETION_BLOCKED'`, a.Owner); got != 1 {
		t.Errorf("записей ACCOUNT_DELETION_BLOCKED: %d", got)
	}
	if got := countOf(t, pools.Owner, `SELECT count(*) FROM sessions WHERE user_id = $1`, a.Owner); got != 2 {
		t.Errorf("сессии владельца тронуты: %d", got)
	}
}

func TestEraseInImmediateModeChangesNothingWhenBlocked(t *testing.T) {
	store, pools := newStore(t)
	a := seedOrganization(t, pools.Owner, "a")
	before := fingerprint(t, pools.Owner)
	result, err := store.Erase(context.Background(), operatorCommand(t, a.Owner, false))
	if err != nil || result.Outcome != domain.ErasureBlocked || len(result.Blockers) != 1 {
		t.Fatalf("Erase() = %+v, %v", result, err)
	}
	if changed := diff(before, fingerprint(t, pools.Owner)); len(changed) != 0 {
		t.Errorf("операторская команда с блокировкой изменила %v", changed)
	}
}

func TestEraseIsAllowedWhenAnotherActiveOwnerRemains(t *testing.T) {
	store, pools := newStore(t)
	a := seedOrganization(t, pools.Owner, "a")
	must(t, pools.Owner, `UPDATE memberships SET role = 'OWNER' WHERE id = $1`, a.ManagerMembership)
	scheduleDue(t, pools.Owner, a.Owner)
	result, err := store.Erase(context.Background(), dueCommand(t, a.Owner))
	if err != nil || result.Outcome != domain.ErasureDone {
		t.Fatalf("Erase() = %+v, %v", result, err)
	}
	// Второй владелец остаётся единственным активным.
	if got := countOf(t, pools.Owner, `SELECT count(*) FROM memberships WHERE tenant_id = $1 AND role = 'OWNER' AND status = 'ACTIVE'`, a.Tenant); got != 1 {
		t.Errorf("активных владельцев: %d", got)
	}
}

func TestEraseIsBlockedForAnActivePlatformAdminUntilTheRightIsRevoked(t *testing.T) {
	store, pools := newStore(t)
	a := seedOrganization(t, pools.Owner, "a")
	must(t, pools.Owner, `INSERT INTO platform_admins(id, user_id, granted_at) VALUES ($1, $2, $3)`, newID(t), a.Manager, seedAt)
	scheduleDue(t, pools.Owner, a.Manager)
	result, err := store.Erase(context.Background(), dueCommand(t, a.Manager))
	if err != nil || result.Outcome != domain.ErasureBlocked || len(result.Blockers) != 1 || result.Blockers[0].Code != "PLATFORM_ADMIN" {
		t.Fatalf("Erase() = %+v, %v", result, err)
	}
	// Право отозвано: после нового запроса пользователь обезличивается, а история выдач остаётся.
	must(t, pools.Owner, `UPDATE platform_admins SET revoked_at = $2 WHERE user_id = $1`, a.Manager, seedAt)
	scheduleDue(t, pools.Owner, a.Manager)
	result, err = store.Erase(context.Background(), dueCommand(t, a.Manager))
	if err != nil || result.Outcome != domain.ErasureDone {
		t.Fatalf("Erase() after the revocation = %+v, %v", result, err)
	}
	if got := countOf(t, pools.Owner, `SELECT count(*) FROM platform_admins WHERE user_id = $1`, a.Manager); got != 1 {
		t.Errorf("история выдач права потеряна: %d", got)
	}
}

func TestEraseSkipsWhatIsNotDueYetAndNeverRepeats(t *testing.T) {
	store, pools := newStore(t)
	a := seedOrganization(t, pools.Owner, "a")
	ctx := context.Background()

	// Удаление не назначено, срок в будущем, пользователя нет.
	for name, userID := range map[string]string{"не назначено": a.Manager, "нет пользователя": newID(t)} {
		result, err := store.Erase(ctx, dueCommand(t, userID))
		if err != nil || result.Outcome != domain.ErasureSkipped {
			t.Errorf("%s: %+v, %v", name, result, err)
		}
	}
	must(t, pools.Owner, `UPDATE users SET deletion_requested_at = $2, deletion_due_at = $3 WHERE id = $1`, a.Manager, seedAt, seedAt.Add(time.Hour))
	if result, err := store.Erase(ctx, dueCommand(t, a.Manager)); err != nil || result.Outcome != domain.ErasureSkipped {
		t.Errorf("срок в будущем: %+v, %v", result, err)
	}
	if got := countOf(t, pools.Owner, `SELECT count(*) FROM sessions WHERE user_id = $1`, a.Manager); got != 2 {
		t.Fatalf("пропущенный пользователь затронут: сессий %d", got)
	}

	// Срок наступил ровно сейчас: граница включительно.
	must(t, pools.Owner, `UPDATE users SET deletion_requested_at = $2::timestamptz - interval '30 days', deletion_due_at = $2 WHERE id = $1`, a.Manager, seedAt)
	result, err := store.Erase(ctx, dueCommand(t, a.Manager))
	if err != nil || result.Outcome != domain.ErasureDone {
		t.Fatalf("срок наступил: %+v, %v", result, err)
	}
	// Повтор безопасен: второй раз пользователь исполнителю не попадается и не пишется второй след.
	after := fingerprint(t, pools.Owner)
	again, err := store.Erase(ctx, dueCommand(t, a.Manager))
	if err != nil || again.Outcome != domain.ErasureSkipped {
		t.Fatalf("повтор: %+v, %v", again, err)
	}
	if changed := diff(after, fingerprint(t, pools.Owner)); len(changed) != 0 {
		t.Errorf("повтор изменил %v", changed)
	}
}

func TestEraseByTheOperatorIsImmediateAndLeavesTheOperatorAudit(t *testing.T) {
	store, pools := newStore(t)
	a := seedOrganization(t, pools.Owner, "a")
	command := operatorCommand(t, a.Manager, false)
	result, err := store.Erase(context.Background(), command)
	if err != nil || result.Outcome != domain.ErasureDone || result.Email != a.ManagerEmail {
		t.Fatalf("Erase() = %+v, %v", result, err)
	}
	var source, operation, entityType, note string
	var actor *string
	var entityID string
	if err := pools.Owner.QueryRow(context.Background(), `
		SELECT source, operation, entity_type, entity_id::text, actor_user_id::text, details ->> 'note' FROM admin_audit_log WHERE id = $1`,
		command.AdminAuditID).Scan(&source, &operation, &entityType, &entityID, &actor, &note); err != nil {
		t.Fatalf("запись журнала оператора: %v", err)
	}
	if source != "CLI" || operation != "USER_ERASED" || entityType != "USER" || entityID != a.Manager || actor != nil || note != "обращение №17" {
		t.Errorf("admin_audit_log = %s %s %s %s %v %q", source, operation, entityType, entityID, actor, note)
	}
	if got := countOf(t, pools.Owner, `SELECT count(*) FROM auth_audit_log WHERE user_id = $1 AND operation = 'ACCOUNT_ANONYMIZED'`, a.Manager); got != 1 {
		t.Errorf("ACCOUNT_ANONYMIZED: %d", got)
	}
	// Самообслуживание не оставляет записи оператора.
	b := seedOrganization(t, pools.Owner, "b")
	scheduleDue(t, pools.Owner, b.Manager)
	if _, err := store.Erase(context.Background(), dueCommand(t, b.Manager)); err != nil {
		t.Fatal(err)
	}
	if got := countOf(t, pools.Owner, `SELECT count(*) FROM admin_audit_log WHERE entity_id = $1`, b.Manager); got != 0 {
		t.Errorf("исполнитель по сроку записал журнал оператора: %d", got)
	}
}

func TestEraseDryRunReportsTheRealCountsAndChangesNothing(t *testing.T) {
	store, pools := newStore(t)
	a := seedOrganization(t, pools.Owner, "a")
	before := fingerprint(t, pools.Owner)

	dry, err := store.Erase(context.Background(), operatorCommand(t, a.Manager, true))
	if err != nil || dry.Outcome != domain.ErasureDone {
		t.Fatalf("сухой запуск: %+v, %v", dry, err)
	}
	if changed := diff(before, fingerprint(t, pools.Owner)); len(changed) != 0 {
		t.Fatalf("сухой запуск изменил %v", changed)
	}
	real, err := store.Erase(context.Background(), operatorCommand(t, a.Manager, false))
	if err != nil || real.Outcome != domain.ErasureDone {
		t.Fatalf("боевой запуск: %+v, %v", real, err)
	}
	if fmt.Sprint(dry.Counts) != fmt.Sprint(real.Counts) {
		t.Errorf("сухой запуск насчитал %v, боевой %v", dry.Counts, real.Counts)
	}
}

func TestEraseIsAtomicWhenTheLastStepFails(t *testing.T) {
	store, pools := newStore(t)
	a := seedOrganization(t, pools.Owner, "a")
	scheduleDue(t, pools.Owner, a.Manager)
	// Адрес-заместитель уже занят: последний шаг, обновление пользователя, нарушит уникальность.
	must(t, pools.Owner, `INSERT INTO users(id, email, password_hash, display_name, status, created_at, updated_at) VALUES ($1, $2, 'x', 'Занято', 'ACTIVE', $3, $3)`,
		newID(t), a.Manager+"@deleted.invalid", seedAt)
	before := fingerprint(t, pools.Owner)

	if _, err := store.Erase(context.Background(), dueCommand(t, a.Manager)); err == nil {
		t.Fatal("Erase() succeeded although the last step must fail")
	}
	if changed := diff(before, fingerprint(t, pools.Owner)); len(changed) != 0 {
		t.Errorf("неудачное обезличивание оставило изменения в %v", changed)
	}
}

func TestEraseByParallelExecutorsAnonymizesOnce(t *testing.T) {
	store, pools := newStore(t)
	a := seedOrganization(t, pools.Owner, "a")
	scheduleDue(t, pools.Owner, a.Manager)

	const executors = 8
	outcomes := make(chan domain.ErasureOutcome, executors)
	failures := make(chan error, executors)
	var group sync.WaitGroup
	for range executors {
		group.Add(1)
		go func() {
			defer group.Done()
			result, err := store.Erase(context.Background(), dueCommand(t, a.Manager))
			if err != nil {
				failures <- err
				return
			}
			outcomes <- result.Outcome
		}()
	}
	group.Wait()
	close(outcomes)
	close(failures)
	for err := range failures {
		t.Fatalf("исполнитель завершился ошибкой: %v", err)
	}
	done := 0
	for outcome := range outcomes {
		if outcome == domain.ErasureDone {
			done++
		}
	}
	if done != 1 {
		t.Fatalf("обезличиваний выполнено %d, ожидалось ровно одно", done)
	}
	if got := countOf(t, pools.Owner, `SELECT count(*) FROM auth_audit_log WHERE user_id = $1 AND operation = 'ACCOUNT_ANONYMIZED'`, a.Manager); got != 1 {
		t.Errorf("записей ACCOUNT_ANONYMIZED: %d", got)
	}
}

// Исполнитель ждёт блокировку членств организации и перепроверяет блокировки уже после неё: пока команда владельца
// понижала второго владельца, пользователь не мог считаться «не единственным».
func TestEraseWaitsForTheOrganizationLockAndRechecksTheBlockers(t *testing.T) {
	store, pools := newStore(t)
	a := seedOrganization(t, pools.Owner, "a")
	must(t, pools.Owner, `UPDATE memberships SET role = 'OWNER' WHERE id = $1`, a.ManagerMembership) // два владельца
	scheduleDue(t, pools.Owner, a.Owner)
	ctx := context.Background()

	holder, err := pools.Owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	// Так же, как lockMemberships модуля tenant: блокировка всех членств организации и понижение второго владельца.
	if _, err := holder.Exec(ctx, `SELECT id FROM memberships WHERE tenant_id = $1 ORDER BY id FOR UPDATE`, a.Tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, `UPDATE memberships SET role = 'MANAGER' WHERE id = $1`, a.ManagerMembership); err != nil {
		t.Fatal(err)
	}

	finished := make(chan domain.ErasureResult, 1)
	failed := make(chan error, 1)
	go func() {
		result, err := store.Erase(ctx, dueCommand(t, a.Owner))
		if err != nil {
			failed <- err
			return
		}
		finished <- result
	}()
	select {
	case result := <-finished:
		t.Fatalf("исполнитель не дождался блокировки организации: %+v", result)
	case err := <-failed:
		t.Fatalf("исполнитель завершился ошибкой раньше блокировки: %v", err)
	case <-time.After(400 * time.Millisecond):
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-finished:
		if result.Outcome != domain.ErasureBlocked || len(result.Blockers) != 1 || result.Blockers[0].Code != "SOLE_OWNER" {
			t.Fatalf("после понижения второго владельца ожидалась блокировка: %+v", result)
		}
	case err := <-failed:
		t.Fatalf("Erase() error = %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("исполнитель не завершился после снятия блокировки")
	}
	if got := countOf(t, pools.Owner, `SELECT count(*) FROM users WHERE id = $1 AND status = 'ACTIVE' AND deleted_at IS NULL`, a.Owner); got != 1 {
		t.Error("владелец обезличен вопреки блокировке")
	}
}

// Приём приглашения держит приглашение, а затем членство принимающего. Исполнитель берёт неиспользованные приглашения
// пользователя раньше членств организации, поэтому, пока он ждёт приглашение, членства свободны и приём доходит до
// конца без взаимного ожидания; завершённый приём не отзывается.
func TestEraseTakesTheInvitationsOfTheUserBeforeTheMemberships(t *testing.T) {
	store, pools := newStore(t)
	a := seedOrganization(t, pools.Owner, "a")
	pending := newID(t)
	must(t, pools.Owner, `INSERT INTO membership_invitations(id, tenant_id, role, code_hash, created_by_user_id, created_at, expires_at) VALUES ($1, $2, 'MANAGER', $3, $4, $5, $6)`,
		pending, a.Tenant, digest("pending-by-manager"), a.Manager, seedAt, seedAt.Add(7*24*time.Hour))
	scheduleDue(t, pools.Owner, a.Manager)
	ctx := context.Background()

	acceptor, err := pools.Owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = acceptor.Rollback(ctx) }()
	if _, err := acceptor.Exec(ctx, `SELECT id FROM membership_invitations WHERE id = $1 FOR UPDATE`, pending); err != nil {
		t.Fatal(err)
	}
	finished := make(chan domain.ErasureResult, 1)
	failed := make(chan error, 1)
	go func() {
		result, err := store.Erase(ctx, dueCommand(t, a.Manager))
		if err != nil {
			failed <- err
			return
		}
		finished <- result
	}()
	select {
	case result := <-finished:
		t.Fatalf("исполнитель не дождался приглашения: %+v", result)
	case err := <-failed:
		t.Fatalf("исполнитель завершился ошибкой раньше приглашения: %v", err)
	case <-time.After(400 * time.Millisecond):
	}
	// Исполнитель ждёт приглашение и не должен держать членства: NOWAIT отвечает ошибкой на любую занятую строку.
	if _, err := acceptor.Exec(ctx, `SELECT id FROM memberships WHERE tenant_id = $1 ORDER BY id FOR UPDATE NOWAIT`, a.Tenant); err != nil {
		t.Fatalf("исполнитель держит членства, ожидая приглашение: %v", err)
	}
	if _, err := acceptor.Exec(ctx, `UPDATE membership_invitations SET accepted_at = $2, accepted_by_user_id = $3 WHERE id = $1`, pending, seedAt, a.Owner); err != nil {
		t.Fatal(err)
	}
	if err := acceptor.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-finished:
		if result.Outcome != domain.ErasureDone || result.Counts["membership_invitations"] != 0 {
			t.Fatalf("Erase() = %+v, приём завершился раньше и не должен отзываться", result)
		}
	case err := <-failed:
		t.Fatalf("Erase() error = %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("исполнитель не завершился после снятия блокировки")
	}
	if got := countOf(t, pools.Owner, `SELECT count(*) FROM membership_invitations WHERE id = $1 AND accepted_at IS NOT NULL AND revoked_at IS NULL`, pending); got != 1 {
		t.Error("принятое приглашение изменено исполнителем")
	}
}

// Приём приглашения и вход держат строку пользователя FOR SHARE. Исполнитель не ждёт такого пользователя, а
// пропускает его (SKIP LOCKED) и возвращается на следующем проходе: транзакция входа не растягивается на очередь.
func TestEraseSkipsAUserWhoseRowIsHeldByAnotherTransaction(t *testing.T) {
	store, pools := newStore(t)
	a := seedOrganization(t, pools.Owner, "a")
	scheduleDue(t, pools.Owner, a.Manager)
	ctx := context.Background()
	holder, err := pools.Owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	var locked string
	if err := holder.QueryRow(ctx, `SELECT id::text FROM users WHERE id = $1 FOR SHARE`, a.Manager).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	result, err := store.Erase(ctx, dueCommand(t, a.Manager))
	if err != nil || result.Outcome != domain.ErasureSkipped {
		t.Fatalf("Erase() пока строка занята = %+v, %v; ожидался пропуск", result, err)
	}
	if waited := time.Since(started); waited > 3*time.Second {
		t.Errorf("исполнитель ждал занятого пользователя %s вместо пропуска", waited)
	}
	if got := countOf(t, pools.Owner, `SELECT count(*) FROM users WHERE id = $1 AND deleted_at IS NULL`, a.Manager); got != 1 {
		t.Error("пропущенный пользователь изменён")
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	result, err = store.Erase(ctx, dueCommand(t, a.Manager))
	if err != nil || result.Outcome != domain.ErasureDone {
		t.Fatalf("следующий проход: %+v, %v", result, err)
	}
}

// Блокировка на строке пользователя не держит исполнителя дольше lock_timeout: он возвращает ошибку и остаётся в очереди.
func TestEraseGivesUpWhenTheOrganizationStaysLocked(t *testing.T) {
	t.Parallel()
	store, pools := newStore(t)
	a := seedOrganization(t, pools.Owner, "a")
	scheduleDue(t, pools.Owner, a.Manager)
	ctx := context.Background()
	holder, err := pools.Owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, `SELECT id FROM memberships WHERE tenant_id = $1 FOR UPDATE`, a.Tenant); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = store.Erase(ctx, dueCommand(t, a.Manager))
	if err == nil {
		t.Fatal("Erase() succeeded although the organization was locked")
	}
	if waited := time.Since(started); waited < 4*time.Second || waited > 15*time.Second {
		t.Errorf("исполнитель ждал %s, ожидалось около 5 секунд", waited)
	}
	_ = holder.Rollback(ctx)
	// Пользователь остался в очереди и обезличивается при следующем проходе.
	result, err := store.Erase(ctx, dueCommand(t, a.Manager))
	if err != nil || result.Outcome != domain.ErasureDone {
		t.Fatalf("повторный проход: %+v, %v", result, err)
	}
}

// Вход, начатый до обезличивания, не оставляет сессию с адресом после него: исполнитель держит строку пользователя.
func TestEraseSerializesWithSessionCreation(t *testing.T) {
	store, pools := newStore(t)
	a := seedOrganization(t, pools.Owner, "a")
	scheduleDue(t, pools.Owner, a.Manager)
	ctx := context.Background()
	if _, err := store.Erase(ctx, dueCommand(t, a.Manager)); err != nil {
		t.Fatal(err)
	}
	// Поздняя вставка сессии для обезличенного пользователя отвергается условием защиты (ADR 0059): она не должна
	// появиться, поэтому сессий пользователя по-прежнему нет.
	tag, err := pools.Owner.Exec(ctx, `
		INSERT INTO sessions(id, user_id, token_hash, expires_at, ip, user_agent, created_at)
		SELECT $1::uuid, $2::uuid, $3, $4::timestamptz, '198.51.100.1'::inet, 'late', $4::timestamptz
		WHERE EXISTS (SELECT 1 FROM users WHERE id = $2::uuid AND status = 'ACTIVE' FOR SHARE)`,
		newID(t), a.Manager, digest("late"), seedAt)
	if err != nil || tag.RowsAffected() != 0 {
		t.Fatalf("сессия для обезличенного пользователя: %v, строк %d", err, tag.RowsAffected())
	}
}

func TestEraseValidatesTheCommandBeforeTouchingTheDatabase(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()
	good := domain.Erasure{UserID: "u", Mode: domain.ModeDue, At: seedAt, PasswordHash: "h", AuditID: "a"}
	for name, mutate := range map[string]func(*domain.Erasure){
		"без пользователя":      func(c *domain.Erasure) { c.UserID = "" },
		"без времени":           func(c *domain.Erasure) { c.At = time.Time{} },
		"без хэша":              func(c *domain.Erasure) { c.PasswordHash = "" },
		"без записи аудита":     func(c *domain.Erasure) { c.AuditID = "" },
		"неизвестный режим":     func(c *domain.Erasure) { c.Mode = "WHENEVER" },
		"сухой запуск по сроку": func(c *domain.Erasure) { c.DryRun = true },
		"оператор без заметки":  func(c *domain.Erasure) { c.Mode, c.AdminAuditID = domain.ModeImmediate, "x" },
	} {
		command := good
		mutate(&command)
		if _, err := store.Erase(ctx, command); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("%s: error = %v, want ErrInvalid", name, err)
		}
	}
	if _, err := (*infrastructure.PostgresStore)(nil).Erase(ctx, good); !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("nil store error = %v", err)
	}
}
