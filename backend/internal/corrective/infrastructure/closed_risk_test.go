package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/internal/corrective/application"
	"lidradar/backend/internal/corrective/domain"
	"lidradar/backend/internal/testsupport"
	"lidradar/backend/platform/ids"
)

func TestPostgresActionsRejectTerminalRiskAndPreserveReplay(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	tenant := testsupport.TwoTenants(t, ctx, pool).A
	service := application.NewService(NewPostgresStore(pool), allowCorrective{}, ids.Generator{}, time.Now)
	for _, status := range []string{"RESOLVED", "FALSE_POSITIVE", "IGNORED", "EXPIRED"} {
		t.Run(status, func(t *testing.T) {
			f := insertCorrectiveFixture(t, pool, tenant.TenantID, tenant.LocationID, tenant.UserID)
			key := "original-" + f.riskID
			original, created, err := service.AddAction(ctx, f.userID, f.tenantID, f.riskID, key, domain.ActionCall, "Звонок")
			if err != nil || !created {
				t.Fatalf("original: created=%v err=%v", created, err)
			}
			if _, err := pool.Exec(ctx, `UPDATE risk_signals SET status=$2, resolved_at=now() WHERE id=$1`, f.riskID, status); err != nil {
				t.Fatal(err)
			}
			replayed, created, err := service.AddAction(ctx, f.userID, f.tenantID, f.riskID, key, domain.ActionCall, "Звонок")
			if err != nil || created || replayed != original {
				t.Fatalf("replay after closure: created=%v err=%v", created, err)
			}
			_, created, err = service.AddAction(ctx, f.userID, f.tenantID, f.riskID, "late-"+f.riskID, domain.ActionOther, "Позднее действие")
			if !errors.Is(err, application.ErrRiskClosed) || created {
				t.Errorf("new action accepted for %s: created=%v err=%v", status, created, err)
			}
			assertActionState(t, pool, f, status, 1)
			if _, _, err := service.AddAction(ctx, f.userID, f.tenantID, f.riskID, key, domain.ActionOther, "Звонок"); !errors.Is(err, application.ErrConflict) {
				t.Fatalf("conflicting replay after closure: %v", err)
			}
			var reserved int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM idempotency_keys WHERE tenant_id=$1 AND key=$2`, f.tenantID, "late-"+f.riskID).Scan(&reserved); err != nil || reserved != 0 {
				t.Fatalf("rejected request reserved key: count=%d err=%v", reserved, err)
			}
		})
	}
}

// Hold an uncommitted closure until the action is actually waiting on its row
// lock. This catches a stale application-layer check without timing-based luck.
func TestPostgresActionSerializesWithRiskClosure(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tenant := testsupport.TwoTenants(t, ctx, pool).A
	service := application.NewService(NewPostgresStore(pool), allowCorrective{}, ids.Generator{}, time.Now)
	for _, commit := range []bool{true, false} {
		name := "closure rolled back"
		if commit {
			name = "closure committed"
		}
		t.Run(name, func(t *testing.T) {
			f := insertCorrectiveFixture(t, pool, tenant.TenantID, tenant.LocationID, tenant.UserID)
			closing, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer closing.Rollback(context.Background())
			if _, err := closing.Exec(ctx, `UPDATE risk_signals SET status='RESOLVED', resolved_at=now() WHERE id=$1`, f.riskID); err != nil {
				t.Fatal(err)
			}
			type result struct {
				created bool
				err     error
			}
			done := make(chan result, 1)
			go func() {
				_, created, err := service.AddAction(ctx, f.userID, f.tenantID, f.riskID, "racing-"+f.riskID, domain.ActionCall, "Звонок")
				done <- result{created, err}
			}()
			waitForCorrectiveRowLock(t, ctx, pool, closing.Conn().PgConn().PID())
			if commit {
				err = closing.Commit(ctx)
			} else {
				err = closing.Rollback(ctx)
			}
			if err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-done:
				if commit {
					if !errors.Is(got.err, application.ErrRiskClosed) || got.created {
						t.Errorf("action committed after closure: %+v", got)
					}
					assertActionState(t, pool, f, "RESOLVED", 0)
				} else {
					if got.err != nil || !got.created {
						t.Fatalf("open risk rejected: %+v", got)
					}
					assertActionState(t, pool, f, "ACTED", 1)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		})
	}
}

func waitForCorrectiveRowLock(t *testing.T, ctx context.Context, pool *pgxpool.Pool, blocker uint32) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE $1::int = ANY(pg_blocking_pids(pid)))`, blocker).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

// Pause the real action INSERT after it has acquired the risk row lock, then
// start closure. Closure must wait for the complete action/key/audit transaction.
func TestPostgresActionCommitsBeforeWaitingClosure(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tenant := testsupport.TwoTenants(t, ctx, pool).A
	f := insertCorrectiveFixture(t, pool, tenant.TenantID, tenant.LocationID, tenant.UserID)
	barrier, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer barrier.Rollback(context.Background())
	lock := barrier.Conn().PgConn().PID()
	if _, err := barrier.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(lock)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION qa07_pause_action() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN PERFORM pg_advisory_xact_lock(%d); RETURN NEW; END $$;
		CREATE TRIGGER qa07_pause BEFORE INSERT ON actions FOR EACH ROW EXECUTE FUNCTION qa07_pause_action();`, lock)); err != nil {
		t.Fatal(err)
	}
	// Both objects are in testsupport's isolated test_<random> schema. Remove
	// them explicitly as well as the fixture's final DROP SCHEMA ... CASCADE.
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if _, err := pool.Exec(cleanup, `DROP TRIGGER IF EXISTS qa07_pause ON actions; DROP FUNCTION IF EXISTS qa07_pause_action();`); err != nil {
			t.Errorf("remove test barrier: %v", err)
		}
	})
	service := application.NewService(NewPostgresStore(pool), allowCorrective{}, ids.Generator{}, time.Now)
	done := make(chan error, 1)
	go func() {
		_, created, err := service.AddAction(ctx, f.userID, f.tenantID, f.riskID, "action-first", domain.ActionCall, "Звонок")
		if err == nil && !created {
			err = errors.New("action was not created")
		}
		done <- err
	}()
	waitForCorrectiveRowLock(t, ctx, pool, lock)
	var actionPID uint32
	if err := pool.QueryRow(ctx, `SELECT pid FROM pg_stat_activity WHERE $1::int = ANY(pg_blocking_pids(pid))`, lock).Scan(&actionPID); err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() {
		_, err := pool.Exec(ctx, `UPDATE risk_signals SET status='RESOLVED', resolved_at=now() WHERE tenant_id=$1 AND id=$2`, f.tenantID, f.riskID)
		closed <- err
	}()
	waitForCorrectiveRowLock(t, ctx, pool, actionPID)
	if err := barrier.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for _, result := range []chan error{done, closed} {
		select {
		case err := <-result:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	assertActionState(t, pool, f, "RESOLVED", 1)
}

func assertActionState(t *testing.T, pool *pgxpool.Pool, f correctiveFixture, wantStatus string, wantCount int) {
	t.Helper()
	var status string
	var actions, audits, keys int
	if err := pool.QueryRow(context.Background(), `SELECT status,
		(SELECT count(*) FROM actions WHERE tenant_id=$1 AND risk_id=$2),
		(SELECT count(*) FROM audit_log a JOIN actions x ON x.id=a.entity_id AND x.tenant_id=a.tenant_id WHERE x.tenant_id=$1 AND x.risk_id=$2),
		(SELECT count(*) FROM idempotency_keys WHERE tenant_id=$1 AND response_body->>'riskId'=$2::text)
		FROM risk_signals WHERE tenant_id=$1 AND id=$2`, f.tenantID, f.riskID).Scan(&status, &actions, &audits, &keys); err != nil {
		t.Fatal(err)
	}
	if status != wantStatus || actions != wantCount || audits != wantCount || keys != wantCount {
		t.Fatalf("state=%s actions=%d audits=%d keys=%d; want %s and %d records", status, actions, audits, keys, wantStatus, wantCount)
	}
}
