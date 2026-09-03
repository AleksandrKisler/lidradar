package devdata

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/internal/testsupport"
	"lidradar/backend/platform/config"
	cryptoplatform "lidradar/backend/platform/crypto"
	"lidradar/backend/platform/postgres"
	"lidradar/backend/platform/tenantctx"
)

// Это пароль только изолированных автоматических тестов, не реквизит стенда.
const testPassword = "only-for-isolated-tests-12345"

func TestValidateTarget(t *testing.T) {
	for _, tc := range []struct {
		env   config.Environment
		url   string
		valid bool
	}{
		{config.EnvironmentDevelopment, "postgres://local/lidradar_frontend", true},
		{config.EnvironmentTest, "postgres://local/lidradar_frontend", true},
		{config.EnvironmentProduction, "postgres://local/lidradar_frontend", false},
		{config.EnvironmentStaging, "postgres://local/lidradar_frontend", false},
		{config.EnvironmentDevelopment, "postgres://local/lidradar", false},
		{config.EnvironmentDevelopment, "", false},
		{config.EnvironmentDevelopment, "postgres://user:do-not-disclose@%/broken", false},
	} {
		err := ValidateTarget(tc.env, tc.url)
		if (err == nil) != tc.valid {
			t.Fatalf("unexpected validation for %s", tc.env)
		}
		if err != nil && strings.Contains(err.Error(), "do-not-disclose") {
			t.Fatal("secret exposed")
		}
	}
}

func frontendPool(t *testing.T) testsupport.Pools {
	t.Helper()
	// Обычный make test остаётся пригодным без базы. Полный прогон этого
	// набора: LIDRADAR_DATABASE_URL указывает на отдельный стенд фронтенда.
	if err := ValidateTarget(config.EnvironmentTest, testingDatabaseURL()); err != nil {
		t.Skip("нужна отдельная база lidradar_frontend; см. frontend-development.md")
	}
	return testsupport.PostgresRoles(t)
}

func TestMigrationRoundTripAndIsolation(t *testing.T) {
	pools := frontendPool(t)
	pool := pools.Owner
	ctx := context.Background()
	other := testsupport.TwoTenants(t, ctx, pool)
	before, err := postgres.NewSchemaReadiness(pool).Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Run(ctx, pool, config.EnvironmentTest, "status", "")
	if err != nil || result.Applied {
		t.Fatalf("initial status: %+v %v", result, err)
	}
	if _, err := Run(ctx, pool, config.EnvironmentProduction, "up", testPassword); err == nil {
		t.Fatal("production allowed")
	}
	if _, err := Run(ctx, pool, config.EnvironmentTest, "up", "short"); err == nil {
		t.Fatal("short password allowed")
	}
	result, err = Run(ctx, pool, config.EnvironmentTest, "up", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Applied || !result.Changed || len(result.Profiles) != 3 {
		t.Fatalf("up: %+v", result)
	}
	for i, p := range Profiles() {
		c := result.Profiles[i]
		if c.Conversations != p.Conversations || c.Messages != p.Conversations*6 || c.Opportunities != p.Conversations || c.Risks != p.Conversations*10/12 {
			t.Fatalf("counts: %+v", c)
		}
		var hash string
		if err := pool.QueryRow(ctx, `SELECT password_hash FROM users WHERE id=$1`, p.UserID).Scan(&hash); err != nil {
			t.Fatal(err)
		}
		valid, err := (cryptoplatform.PasswordHasher{}).Verify(testPassword, hash)
		if err != nil || !valid {
			t.Fatal("password is not accepted by production hasher")
		}
		var visible int
		if err := pools.App.QueryRow(tenantctx.WithTenant(ctx, p.TenantID), `SELECT count(*) FROM conversations`).Scan(&visible); err != nil || visible != p.Conversations {
			t.Fatalf("RLS: %d %v", visible, err)
		}
	}
	assertCount(t, pools.App, ctx, `SELECT count(*) FROM conversations`, 0)
	assertCount(t, pool, ctx, `SELECT count(DISTINCT type) FROM risk_signals`, 5)
	assertCount(t, pool, ctx, `SELECT count(DISTINCT status) FROM risk_signals`, 7)
	assertCount(t, pool, ctx, `SELECT count(*) FROM jobs`, 0)
	assertCount(t, pool, ctx, `SELECT count(*) FROM ai_jobs`, 0)
	assertCount(t, pool, ctx, `SELECT count(*) FROM outbox_events`, 0)
	assertCount(t, pool, ctx, `SELECT count(*) FROM telegram_user_links`, 0)
	assertCount(t, pool, ctx, `SELECT count(*) FROM platform_admins`, 0)
	assertCount(t, pool, ctx, `SELECT count(*) FROM revenue_attributions WHERE type='RECOVERED'`, 22)
	assertCount(t, pool, ctx, `SELECT count(*) FROM risk_feedback WHERE dataset_eligible`, 0)
	// Временные отметки находятся в прошлом, а агрегат согласован с сообщениями.
	assertCount(t, pool, ctx, `SELECT count(*) FROM conversations c WHERE revision NOT IN (6,7) OR first_message_at<>(SELECT min(sent_at) FROM messages WHERE conversation_id=c.id) OR last_message_at<>(SELECT max(sent_at) FROM messages WHERE conversation_id=c.id)`, 0)
	assertCount(t, pool, ctx, `SELECT count(*) FROM messages WHERE sent_at>now()`, 0)
	assertCount(t, pool, ctx, `SELECT count(*) FROM revenue_events WHERE confirmed_at>now()`, 0)
	repeated, err := Run(ctx, pool, config.EnvironmentTest, "up", "different-password-is-not-applied")
	if err != nil || repeated.Changed || !repeated.Applied || !repeated.AppliedAt.Equal(*result.AppliedAt) {
		t.Fatalf("repeat: %+v %v", repeated, err)
	}
	// Состояние фронтенда после ручного входа/настроек тоже должно откатываться.
	p := Profiles()[1]
	_, err = pool.Exec(ctx, `INSERT INTO sessions(id,user_id,token_hash,expires_at) VALUES(uuidv7(),$1,repeat('a',64),now()+interval '1 day');`, p.UserID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO auth_audit_log(id,user_id,operation,created_at) VALUES(uuidv7(),$1,'LOGIN',now())`, p.UserID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO ml_consents(id,tenant_id,scope,granted_by,granted_at) VALUES(uuidv7(),$1,'DATASETS',$2,now())`, p.TenantID, p.UserID)
	if err != nil {
		t.Fatal(err)
	}
	down, err := Run(ctx, pool, config.EnvironmentTest, "down", "")
	if err != nil || down.Applied || !down.Changed {
		t.Fatalf("down: %+v %v", down, err)
	}
	assertCount(t, pool, ctx, `SELECT count(*) FROM users`, 2)
	assertCount(t, pool, ctx, `SELECT count(*) FROM organizations`, 2)
	assertCount(t, pool, ctx, `SELECT count(*) FROM conversations`, 0)
	assertCount(t, pool, ctx, `SELECT count(*) FROM auth_audit_log`, 0)
	assertCount(t, pool, ctx, `SELECT count(*) FROM sessions`, 0)
	assertCount(t, pool, ctx, `SELECT count(*) FROM ml_consents`, 0)
	assertCount(t, pool, ctx, `SELECT count(*) FROM pg_trigger WHERE NOT tgisinternal AND tgrelid IN (SELECT oid FROM pg_class WHERE relnamespace=current_schema()::regnamespace) AND tgenabled<>'O'`, 0)
	if _, err := pool.Exec(ctx, `DELETE FROM memberships WHERE tenant_id=$1`, other.A.TenantID); err == nil {
		t.Fatal("membership protection lost")
	}
	after, err := postgres.NewSchemaReadiness(pool).Check(ctx)
	if err != nil || after.Latest != before.Latest {
		t.Fatalf("schema changed: %+v %v", after, err)
	}
	down, err = Run(ctx, pool, config.EnvironmentTest, "down", "")
	if err != nil || down.Changed {
		t.Fatalf("repeat down: %+v %v", down, err)
	}
	if _, err := Run(ctx, pool, config.EnvironmentTest, "up", testPassword); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationCollisionAndRollbackBoundaries(t *testing.T) {
	pool := frontendPool(t).Owner
	ctx := context.Background()
	p := Profiles()[0]
	// Коллизия почты не должна менять существующий пароль или создавать часть набора.
	_, err := pool.Exec(ctx, `INSERT INTO users(id,email,password_hash,display_name,status) VALUES(uuidv7(),$1,'unchanged','Посторонний пользователь','ACTIVE')`, p.Email)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Run(ctx, pool, config.EnvironmentTest, "up", testPassword); err == nil {
		t.Fatal("collision ignored")
	}
	assertCount(t, pool, ctx, `SELECT count(*) FROM organizations`, 0)
	assertCount(t, pool, ctx, `SELECT count(*) FROM users WHERE password_hash='unchanged'`, 1)
	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE email=$1`, p.Email); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(ctx, pool, config.EnvironmentTest, "up", testPassword); err != nil {
		t.Fatal(err)
	}
	other := testsupport.TwoTenants(t, ctx, pool)
	_, err = pool.Exec(ctx, `INSERT INTO memberships(id,tenant_id,user_id,role,status) VALUES(uuidv7(),$1,$2,'MANAGER','ACTIVE')`, other.A.TenantID, p.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Run(ctx, pool, config.EnvironmentTest, "down", ""); err == nil {
		t.Fatal("cross-organization membership ignored")
	}
	assertCount(t, pool, ctx, `SELECT count(*) FROM conversations`, 264)
	assertCount(t, pool, ctx, `SELECT count(*) FROM frontend_data_migrations`, 1)
	assertCount(t, pool, ctx, `SELECT count(*) FROM pg_trigger WHERE NOT tgisinternal AND tgrelid='memberships'::regclass AND tgenabled<>'O'`, 0)
}

func TestMigrationConcurrentUpAndChecksum(t *testing.T) {
	pool := frontendPool(t).Owner
	ctx := context.Background()
	var wg sync.WaitGroup
	results := make(chan Result, 2)
	failures := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			r, err := Run(ctx, pool, config.EnvironmentTest, "up", testPassword)
			results <- r
			failures <- err
		})
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	changed := 0
	for r := range results {
		if r.Changed {
			changed++
		}
	}
	if changed != 1 {
		t.Fatalf("changed %d times", changed)
	}
	if _, err := pool.Exec(ctx, `UPDATE frontend_data_migrations SET checksum='changed'`); err != nil {
		t.Fatal(err)
	}
	for _, direction := range []string{"up", "down", "status"} {
		if _, err := Run(ctx, pool, config.EnvironmentTest, direction, testPassword); err == nil {
			t.Fatalf("checksum ignored by %s", direction)
		}
	}
}

// Ошибка ПОСЛЕ временной приостановки запретов удаления должна вернуть не
// только все строки, но и защиту журналов. Внешний ключ при этом не отключён.
func TestRollbackFailureRestoresDataAndProtection(t *testing.T) {
	pool := frontendPool(t).Owner
	ctx := context.Background()
	if _, err := Run(ctx, pool, config.EnvironmentTest, "up", testPassword); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE external_reference (risk_id UUID REFERENCES risk_signals(id));
		INSERT INTO external_reference SELECT id FROM risk_signals ORDER BY id LIMIT 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(ctx, pool, config.EnvironmentTest, "down", ""); err == nil {
		t.Fatal("foreign key was bypassed")
	}
	assertCount(t, pool, ctx, `SELECT count(*) FROM conversations`, 264)
	assertCount(t, pool, ctx, `SELECT count(*) FROM revenue_events`, 44)
	assertCount(t, pool, ctx, `SELECT count(*) FROM frontend_data_migrations`, 1)
	assertCount(t, pool, ctx, `SELECT count(*) FROM pg_trigger WHERE NOT tgisinternal AND tgrelid IN (SELECT oid FROM pg_class WHERE relnamespace=current_schema()::regnamespace) AND tgenabled<>'O'`, 0)
	if _, err := pool.Exec(ctx, `DELETE FROM actions`); err == nil {
		t.Fatal("append-only protection lost")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM external_reference`); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(ctx, pool, config.EnvironmentTest, "down", ""); err != nil {
		t.Fatal(err)
	}
}

func assertCount(t *testing.T, pool *pgxpool.Pool, ctx context.Context, query string, want int) {
	t.Helper()
	var got int
	if err := pool.QueryRow(ctx, query).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s: got %d, want %d", query, got, want)
	}
}

// Имя переменной одинаково с каноническими интеграционными тестами.
func testingDatabaseURL() string { return os.Getenv("LIDRADAR_DATABASE_URL") }
