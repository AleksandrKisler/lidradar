package postgres_test

import (
	"context"
	"strings"
	"testing"

	"lidradar/backend/internal/testsupport"
)

// TestSchemaInvariantsTenantTablesAndCompositeForeignKeys печатает таблицы с
// tenant_id и проверяет, что связи между tenant-таблицами включают tenant_id
// (ТЗ §13, LR-BE-2402).
func TestSchemaInvariantsTenantTablesAndCompositeForeignKeys(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	var super, createRole bool
	var user string
	if err := pool.QueryRow(ctx, `SELECT current_user, rolsuper, rolcreaterole FROM pg_roles WHERE rolname = current_user`).Scan(&user, &super, &createRole); err != nil {
		t.Fatal(err)
	}
	t.Logf("db user=%s superuser=%v createrole=%v", user, super, createRole)
	rows, err := pool.Query(ctx, `
		SELECT table_name FROM information_schema.columns
		WHERE table_schema = current_schema() AND column_name = 'tenant_id' ORDER BY table_name`)
	if err != nil {
		t.Fatal(err)
	}
	var tenantTables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tenantTables = append(tenantTables, name)
	}
	rows.Close()
	t.Logf("tenant tables (%d): %s", len(tenantTables), strings.Join(tenantTables, ", "))
	rows, err = pool.Query(ctx, `
		SELECT con.conname, src.relname, dst.relname,
		       (SELECT string_agg(a.attname, ',' ORDER BY k.ord) FROM unnest(con.conkey) WITH ORDINALITY AS k(attnum, ord)
		        JOIN pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = k.attnum) AS columns
		FROM pg_constraint con
		JOIN pg_class src ON src.oid = con.conrelid
		JOIN pg_class dst ON dst.oid = con.confrelid
		JOIN pg_namespace ns ON ns.oid = src.relnamespace
		WHERE con.contype = 'f' AND ns.nspname = current_schema()
		ORDER BY src.relname, con.conname`)
	if err != nil {
		t.Fatal(err)
	}
	tenant := map[string]bool{}
	for _, name := range tenantTables {
		tenant[name] = true
	}
	var violations []string
	for rows.Next() {
		var name, source, target, columns string
		if err := rows.Scan(&name, &source, &target, &columns); err != nil {
			t.Fatal(err)
		}
		if tenant[source] && tenant[target] && !strings.Contains(","+columns+",", ",tenant_id,") {
			violations = append(violations, source+"."+name+"("+columns+") -> "+target)
		}
	}
	rows.Close()
	if len(violations) != 0 {
		t.Fatalf("связи между tenant-таблицами без tenant_id:\n%s", strings.Join(violations, "\n"))
	}
}
