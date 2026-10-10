package postgres_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/internal/testsupport"
	"lidradar/backend/platform/ids"
)

// scripts/sql/bootstrap-roles.sql возвращает базе, восстановленной без прав
// (pg_dump --no-owner --no-privileges), ровно те права, которые дали миграции,
// а scripts/sql/verify-roles.sql замечает разницу. Тест не даёт файлам разойтись
// с миграцией 000020: изменение прав в миграциях без правки скрипта его ломает.
func TestRoleBootstrapReproducesMigrationGrants(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	bootstrap, verify := readScriptSQL(t, "bootstrap-roles.sql"), readScriptSQL(t, "verify-roles.sql")

	want := aclSnapshot(t, ctx, pool)
	requireMeaningfulSnapshot(t, want)
	if problems := verifyProblems(t, ctx, pool, verify, ""); len(problems) != 0 {
		t.Fatalf("схема после миграций не проходит проверку ролей:\n%s", strings.Join(problems, "\n"))
	}

	// Состояние после восстановления: ни прав на объекты, ни прав по умолчанию.
	var schema string
	if err := pool.QueryRow(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	roles := "lidradar_app, lidradar_worker, lidradar_platform"
	for _, statement := range []string{
		"REVOKE ALL ON SCHEMA %[1]s FROM " + roles,
		"REVOKE ALL ON ALL TABLES IN SCHEMA %[1]s FROM " + roles,
		"REVOKE ALL ON ALL SEQUENCES IN SCHEMA %[1]s FROM " + roles,
		"ALTER DEFAULT PRIVILEGES IN SCHEMA %[1]s REVOKE ALL ON TABLES FROM " + roles,
		"ALTER DEFAULT PRIVILEGES IN SCHEMA %[1]s REVOKE ALL ON SEQUENCES FROM " + roles,
	} {
		if _, err := pool.Exec(ctx, fmt.Sprintf(statement, pgx.Identifier{schema}.Sanitize())); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if reflect.DeepEqual(aclSnapshot(t, ctx, pool), want) {
		t.Fatal("снятие прав ничего не изменило: тест не проверяет восстановление")
	}
	problems := verifyProblems(t, ctx, pool, verify, "")
	joined := strings.Join(problems, "\n")
	for _, fragment := range []string{"нет USAGE на схему", "на таблицу", "права по умолчанию"} {
		if !strings.Contains(joined, fragment) {
			t.Fatalf("проверка не сообщила о %q:\n%.600s", fragment, joined)
		}
	}

	for attempt := 1; attempt <= 2; attempt++ { // повторный запуск безопасен
		if _, err := pool.Exec(ctx, bootstrap); err != nil {
			t.Fatalf("bootstrap-roles.sql, запуск %d: %v", attempt, err)
		}
		if got := aclSnapshot(t, ctx, pool); !reflect.DeepEqual(got, want) {
			t.Fatalf("запуск %d: права после bootstrap отличаются от выданных миграциями:\n%s", attempt, firstDifference(want, got))
		}
		if problems := verifyProblems(t, ctx, pool, verify, ""); len(problems) != 0 {
			t.Fatalf("запуск %d: проверка после bootstrap:\n%s", attempt, strings.Join(problems, "\n"))
		}
	}
}

// Отдельный пользователь приложения получает то же членство в ролях, а
// несуществующий пользователь замечается проверкой.
func TestRoleBootstrapGrantsMembershipToSeparateApplicationUser(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	bootstrap, verify := readScriptSQL(t, "bootstrap-roles.sql"), readScriptSQL(t, "verify-roles.sql")

	suffix, err := (ids.Generator{}).NewID()
	if err != nil {
		t.Fatal(err)
	}
	user := "lr_rt_" + strings.ReplaceAll(suffix[len(suffix)-12:], "-", "")
	if problems := verifyProblems(t, ctx, pool, verify, user); len(problems) != 1 || !strings.Contains(problems[0], "нет пользователя "+user) {
		t.Fatalf("несуществующий пользователь приложения: %v", problems)
	}
	if _, err := pool.Exec(ctx, `CREATE ROLE `+pgx.Identifier{user}.Sanitize()+` NOLOGIN`); err != nil {
		t.Fatalf("создание тестового пользователя: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DROP ROLE IF EXISTS `+pgx.Identifier{user}.Sanitize()) })

	if problems := verifyProblems(t, ctx, pool, verify, user); len(problems) == 0 || !strings.Contains(strings.Join(problems, "\n"), "не может переключаться") {
		t.Fatalf("пользователь без членства принят: %v", problems)
	}
	transaction, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()
	if _, err := transaction.Exec(ctx, `SELECT set_config('lidradar.app_user', $1, true)`, user); err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Exec(ctx, bootstrap); err != nil {
		t.Fatalf("bootstrap-roles.sql с отдельным пользователем: %v", err)
	}
	if err := transaction.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"lidradar_app", "lidradar_worker", "lidradar_platform"} {
		var member bool
		if err := pool.QueryRow(ctx, `SELECT pg_has_role($1::name, $2::name, 'MEMBER')`, user, role).Scan(&member); err != nil || !member {
			t.Fatalf("пользователь приложения не состоит в роли %s: %v, %v", role, member, err)
		}
	}
	if problems := verifyProblems(t, ctx, pool, verify, user); len(problems) != 0 {
		t.Fatalf("проверка с отдельным пользователем:\n%s", strings.Join(problems, "\n"))
	}
}

// requireMeaningfulSnapshot защищает сравнение от пустого результата: в снимке
// должны быть схема, десятки таблиц и права по умолчанию, и у каждой таблицы и
// каждого набора прав по умолчанию все три роли.
func requireMeaningfulSnapshot(t *testing.T, snapshot []string) {
	t.Helper()
	tables, defaults, schemas := 0, 0, 0
	for _, line := range snapshot {
		switch {
		case strings.HasPrefix(line, "relation r "):
			tables++
		case strings.HasPrefix(line, "default "):
			defaults++
		case strings.HasPrefix(line, "schema "):
			schemas++
		default:
			continue
		}
		for _, role := range []string{"lidradar_app=", "lidradar_worker=", "lidradar_platform="} {
			if !strings.Contains(line, role) {
				t.Fatalf("в строке снимка нет прав роли %s: %s", role, line)
			}
		}
	}
	if schemas != 1 || tables < 40 || defaults != 2 {
		t.Fatalf("снимок прав неполон: схем %d, таблиц %d, прав по умолчанию %d", schemas, tables, defaults)
	}
}

func readScriptSQL(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "scripts", "sql", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// verifyProblems выполняет verify-roles.sql; appUser задаёт lidradar.app_user на
// время одной транзакции, чтобы настройка не осталась в соединении пула.
func verifyProblems(t *testing.T, ctx context.Context, pool *pgxpool.Pool, verify, appUser string) []string {
	t.Helper()
	transaction, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()
	if appUser != "" {
		if _, err := transaction.Exec(ctx, `SELECT set_config('lidradar.app_user', $1, true)`, appUser); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := transaction.Query(ctx, verify)
	if err != nil {
		t.Fatalf("verify-roles.sql: %v", err)
	}
	defer rows.Close()
	var problems []string
	for rows.Next() {
		var problem string
		if err := rows.Scan(&problem); err != nil {
			t.Fatal(err)
		}
		problems = append(problems, problem)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return problems
}

// aclSnapshot — права на схему, таблицы, последовательности и права по умолчанию
// текущей схемы одной строкой на объект. Сравниваются списки ACL целиком, включая
// выдавшего права, отсортированные внутри строки.
func aclSnapshot(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `
		SELECT 'schema ' || n.nspname || ' ' || ARRAY(SELECT x::text FROM unnest(n.nspacl) AS x ORDER BY 1)::text
		FROM pg_namespace n
		WHERE n.nspname = current_schema()
		UNION ALL
		SELECT 'relation ' || c.relkind::text || ' ' || c.relname || ' ' || ARRAY(SELECT x::text FROM unnest(c.relacl) AS x ORDER BY 1)::text
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = current_schema() AND c.relkind IN ('r', 'p', 'S')
		UNION ALL
		SELECT 'default ' || d.defaclobjtype::text || ' ' || ARRAY(SELECT x::text FROM unnest(d.defaclacl) AS x ORDER BY 1)::text
		FROM pg_default_acl d
		WHERE d.defaclnamespace = to_regnamespace(current_schema())
		ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var snapshot []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		snapshot = append(snapshot, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func firstDifference(want, got []string) string {
	index := map[string]bool{}
	for _, line := range want {
		index[line] = true
	}
	var extra []string
	for _, line := range got {
		if !index[line] {
			extra = append(extra, "получено: "+line)
		} else {
			delete(index, line)
		}
	}
	var missing []string
	for line := range index {
		missing = append(missing, "ожидалось: "+line)
	}
	all := append(missing, extra...)
	if len(all) > 6 {
		all = all[:6]
	}
	return strings.Join(all, "\n")
}
