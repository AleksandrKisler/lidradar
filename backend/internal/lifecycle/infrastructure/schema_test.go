package infrastructure_test

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/internal/lifecycle/domain"
	"lidradar/backend/internal/testsupport"
)

// Реестр данных против живой схемы (ADR 0059). Тесты падают, когда схема меняется без записи в реестре:
// новая ссылка на пользователя, новая таблица с tenant_id или новая колонка выгружаемой таблицы не могут молча
// остаться вне выгрузки и обезличивания.

// personalExportSections — разделы личной выгрузки, на которые вправе ссылаться реестр.
var personalExportSections = map[string]bool{
	"account": true, "memberships": true, "sessions": true, "authEvents": true, "notificationSettings": true,
	"telegramLinks": true, "consentDecisions": true, "authoredRecords": true,
}

// schemaRows читает каталог. Системные представления обходят все схемы кластера, а соседние тесты создают и
// удаляют свои схемы одновременно: запрос может наткнуться на отношение, исчезнувшее посреди чтения («could not open
// relation with OID»). Такая ошибка не говорит ничего о схеме теста, поэтому запрос повторяется.
func schemaRows(t *testing.T, pool *pgxpool.Pool, query string) [][2]string {
	t.Helper()
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		rows, err := pool.Query(context.Background(), query)
		if err == nil {
			var result [][2]string
			for rows.Next() {
				var pair [2]string
				if err = rows.Scan(&pair[0], &pair[1]); err != nil {
					break
				}
				result = append(result, pair)
			}
			if err == nil {
				err = rows.Err()
			}
			rows.Close()
			if err == nil {
				return result
			}
		}
		lastErr = err
		if !strings.Contains(err.Error(), "could not open relation") {
			break
		}
	}
	t.Fatal(lastErr)
	return nil
}

// schemaUserReferences — колонки, указывающие на пользователя: внешние ключи на users(id) и memberships(user_id) и
// uuid-колонки без ключа, названные как идентификатор пользователя или администратора.
func schemaUserReferences(t *testing.T, pool *pgxpool.Pool) map[string]bool {
	t.Helper()
	found := map[string]bool{}
	for _, pair := range schemaRows(t, pool, `
		SELECT c.conrelid::regclass::text, a.attname
		FROM pg_constraint c
		CROSS JOIN LATERAL unnest(c.conkey, c.confkey) AS k(attnum, refnum)
		JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.attnum
		JOIN pg_attribute r ON r.attrelid = c.confrelid AND r.attnum = k.refnum
		WHERE c.contype = 'f' AND c.connamespace = current_schema()::regnamespace
		  AND ((c.confrelid = 'users'::regclass AND r.attname = 'id')
		    OR (c.confrelid = 'memberships'::regclass AND r.attname = 'user_id'))`) {
		found[pair[0]+"."+pair[1]] = true
	}
	for _, pair := range schemaRows(t, pool, `
		SELECT table_name, column_name FROM information_schema.columns
		WHERE table_schema = current_schema() AND data_type = 'uuid'
		  AND (column_name IN ('user_id', 'actor_user_id') OR column_name LIKE '%\_by' OR column_name LIKE '%\_by\_user\_id')
		  AND table_name IN (SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema() AND table_type = 'BASE TABLE')`) {
		found[pair[0]+"."+pair[1]] = true
	}
	for notUser := range domain.NotUserColumns {
		delete(found, notUser)
	}
	return found
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func TestRegistryClassifiesEveryReferenceToUsers(t *testing.T) {
	pool := testsupport.Postgres(t)
	schema := schemaUserReferences(t, pool)
	registry := map[string]bool{}
	for _, reference := range domain.UserReferences {
		key := reference.Table + "." + reference.Column
		if registry[key] {
			t.Errorf("%s записана в реестре дважды", key)
		}
		registry[key] = true
		switch reference.Disposition {
		case domain.Erase, domain.Anonymize, domain.Revoke, domain.Retain:
		default:
			t.Errorf("%s: неизвестное действие %q", key, reference.Disposition)
		}
		if reference.Export != "" && !personalExportSections[reference.Export] {
			t.Errorf("%s: раздел личной выгрузки %q не существует", key, reference.Export)
		}
	}
	for _, key := range sortedKeys(schema) {
		if !registry[key] {
			t.Errorf("колонка %s указывает на пользователя, но её нет в domain.UserReferences: решите, выгружается ли она и что с ней при обезличивании", key)
		}
	}
	for _, key := range sortedKeys(registry) {
		if key == "users.id" {
			continue // сама строка пользователя
		}
		if !schema[key] {
			t.Errorf("запись реестра %s не соответствует схеме: колонка удалена или не ссылается на пользователя", key)
		}
	}
	// Колонки, которые обезличивание стирает, обязаны быть известны и выгрузке: пользователь должен видеть то, что стирается.
	for _, reference := range domain.UserReferences {
		if reference.Disposition == domain.Erase && reference.Export == "" && reference.Reason == "" {
			t.Errorf("%s.%s стирается, но не выгружается и без объяснения причины", reference.Table, reference.Column)
		}
	}
}

func tenantTables(t *testing.T, pool *pgxpool.Pool) map[string]bool {
	t.Helper()
	tables := map[string]bool{}
	for _, pair := range schemaRows(t, pool, `
		SELECT c.table_name, c.column_name FROM information_schema.columns c
		JOIN information_schema.tables t ON t.table_schema = c.table_schema AND t.table_name = c.table_name AND t.table_type = 'BASE TABLE'
		WHERE c.table_schema = current_schema() AND c.column_name = 'tenant_id'`) {
		tables[pair[0]] = true
	}
	return tables
}

func tableColumns(t *testing.T, pool *pgxpool.Pool, table string) map[string]bool {
	t.Helper()
	columns := map[string]bool{}
	rows, err := pool.Query(context.Background(),
		`SELECT column_name FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = $1`, table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			t.Fatal(err)
		}
		columns[column] = true
	}
	return columns
}

func TestRegistryClassifiesEveryTenantTable(t *testing.T) {
	pool := testsupport.Postgres(t)
	schema := tenantTables(t, pool)
	classified := map[string]string{}
	for _, dataset := range domain.OrganizationDatasets {
		if previous, duplicate := classified[dataset.Table]; duplicate {
			t.Errorf("таблица %s попала в наборы дважды (%s и %s)", dataset.Table, previous, dataset.File)
		}
		classified[dataset.Table] = "набор " + dataset.File
	}
	for table, reason := range domain.ExcludedTables {
		if previous, duplicate := classified[table]; duplicate {
			t.Errorf("таблица %s одновременно исключена и входит в %s", table, previous)
		}
		if strings.TrimSpace(reason) == "" {
			t.Errorf("исключённая таблица %s без причины", table)
		}
		classified[table] = "исключена"
	}
	for _, table := range sortedKeys(schema) {
		if classified[table] == "" {
			t.Errorf("таблица %s имеет tenant_id, но не входит ни в наборы выгрузки, ни в исключения domain.ExcludedTables", table)
		}
	}
	for table := range classified {
		if !schema[table] {
			t.Errorf("запись реестра о таблице %s не соответствует схеме: таблицы с tenant_id нет", table)
		}
	}
}

func TestDatasetColumnsAreExactlyTheSchemaColumnsMinusTheExcludedOnes(t *testing.T) {
	pool := testsupport.Postgres(t)
	for _, dataset := range domain.OrganizationDatasets {
		if dataset.Source != domain.SourceTable {
			continue // набор команды собирается соединением, его колонки проверяет TestTeamDatasetKeepsRegistryColumns
		}
		schema := tableColumns(t, pool, dataset.Table)
		excluded := domain.ExcludedColumns[dataset.Table]
		seen := map[string]bool{}
		for _, column := range dataset.Columns {
			if seen[column] {
				t.Errorf("%s: колонка %s повторяется", dataset.File, column)
			}
			seen[column] = true
			if column == domain.TenantColumn() {
				t.Errorf("%s: идентификатор организации не должен выгружаться", dataset.File)
			}
			if !schema[column] {
				t.Errorf("%s: колонки %s нет в таблице %s", dataset.File, column, dataset.Table)
			}
			if _, both := excluded[column]; both {
				t.Errorf("%s: колонка %s одновременно выгружается и исключена", dataset.File, column)
			}
		}
		for column, reason := range excluded {
			if !schema[column] {
				t.Errorf("%s: исключённой колонки %s нет в таблице", dataset.Table, column)
			}
			if strings.TrimSpace(reason) == "" {
				t.Errorf("%s.%s исключена без причины", dataset.Table, column)
			}
		}
		for column := range schema {
			if column == domain.TenantColumn() || seen[column] {
				continue
			}
			if _, isExcluded := excluded[column]; !isExcluded {
				t.Errorf("%s.%s не выгружается и не объяснена в domain.ExcludedColumns: добавьте её в набор или в исключения с причиной", dataset.Table, column)
			}
		}
	}
	for table := range domain.ExcludedColumns {
		found := false
		for _, dataset := range domain.OrganizationDatasets {
			found = found || dataset.Table == table
		}
		if !found {
			t.Errorf("исключения колонок описывают таблицу %s, которой нет в наборах", table)
		}
	}
}
