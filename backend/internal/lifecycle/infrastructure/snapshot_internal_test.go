package infrastructure

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"lidradar/backend/internal/testsupport"
)

// Снимок организации только читает (ADR 0059): запись внутри него отвергает сама база, поэтому ошибка в запросе
// выгрузки не сможет ничего изменить. Тест внутренний: ему нужна транзакция снимка, которой нет в интерфейсе.
func TestSnapshotTransactionIsReadOnly(t *testing.T) {
	pools := testsupport.PostgresRoles(t)
	ctx := context.Background()
	pair := testsupport.TwoTenants(t, ctx, pools.Owner)
	store := NewPostgresStore(pools.Platform, pools.App)

	for name, statement := range map[string]string{
		"временная таблица": `CREATE TEMP TABLE snapshot_must_not_write(i int)`,
		"организация":       `UPDATE organizations SET name = name || '!'`,
	} {
		// После отказа транзакция прервана, поэтому каждая проверка получает собственный снимок.
		opened, err := store.OpenSnapshot(ctx, pair.A.TenantID)
		if err != nil {
			t.Fatalf("%s: OpenSnapshot() error = %v", name, err)
		}
		_, err = opened.(*snapshot).tx.Exec(ctx, statement)
		var failure *pgconn.PgError
		if !errors.As(err, &failure) || failure.Code != "25006" {
			t.Errorf("%s: запись в снимке не отвергнута как read_only_sql_transaction: %v", name, err)
		}
		_ = opened.Close(ctx)
	}
}
