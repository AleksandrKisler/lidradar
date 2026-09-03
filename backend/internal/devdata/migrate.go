// Package devdata управляет отдельной обратимой миграцией синтетических
// кабинетов. Это инструмент разработки, не часть рабочих миграций или API.
package devdata

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/platform/config"
	cryptoplatform "lidradar/backend/platform/crypto"
	"lidradar/backend/platform/postgres"
)

const Version = "frontend-v1"
const DatabaseName = "lidradar_frontend"

//go:embed migrations/*.sql
var files embed.FS

// Profile описывает отдельную организацию с единственным владельцем.
type Profile struct {
	Number        int    `json:"number"`
	UserID        string `json:"userId"`
	TenantID      string `json:"tenantId"`
	Email         string `json:"email"`
	Name          string `json:"name"`
	Conversations int    `json:"conversations"`
	Locations     int    `json:"locations"`
}

// Profiles возвращает копию манифеста; эти идентификаторы зарезервированы
// только в отдельной базе разработки, а не в рабочей базе пользователя.
func Profiles() []Profile {
	return []Profile{
		{1, "01990000-0000-7000-8000-000000000101", "01990000-0000-7000-8000-000000000201", "empty@lidradar.test", "Новый кабинет · учебный", 0, 0},
		{2, "01990000-0000-7000-8000-000000000102", "01990000-0000-7000-8000-000000000202", "small@lidradar.test", "Студия Линия · учебная", 24, 1},
		{3, "01990000-0000-7000-8000-000000000103", "01990000-0000-7000-8000-000000000203", "large@lidradar.test", "Сеть студий Линия · учебная", 240, 3},
	}
}

type Counts struct {
	Email         string `json:"email"`
	TenantID      string `json:"tenantId"`
	Conversations int    `json:"conversations"`
	Messages      int    `json:"messages"`
	Opportunities int    `json:"opportunities"`
	Risks         int    `json:"risks"`
}

type Result struct {
	Version   string     `json:"version"`
	Applied   bool       `json:"applied"`
	Changed   bool       `json:"changed"`
	AppliedAt *time.Time `json:"appliedAt,omitempty"`
	Profiles  []Counts   `json:"profiles"`
}

// ValidateTarget запрещает случайный запуск в обычной базе даже при ошибочно
// выставленном development. Секреты адреса подключения не включаются в ошибки.
func ValidateTarget(environment config.Environment, databaseURL string) error {
	if environment != config.EnvironmentDevelopment && environment != config.EnvironmentTest {
		return errors.New("тестовые данные разрешены только в development и test")
	}
	configuration, err := pgx.ParseConfig(databaseURL)
	if databaseURL == "" || err != nil || configuration.Database != DatabaseName {
		return fmt.Errorf("нужен явный LIDRADAR_DATABASE_URL к отдельной базе %s", DatabaseName)
	}
	return nil
}

// Run выполняет up/down/status. Повторное применение и повторный откат —
// пустые операции. Пароль нужен только для первого up и никогда не печатается.
func Run(ctx context.Context, pool *pgxpool.Pool, environment config.Environment, direction, password string) (Result, error) {
	if direction != "up" && direction != "down" && direction != "status" {
		return Result{}, errors.New("ожидается up, down или status")
	}
	if pool == nil {
		return Result{}, errors.New("не задано подключение к базе")
	}
	if err := ValidateTarget(environment, pool.Config().ConnString()); err != nil {
		return Result{}, err
	}
	var database string
	var owner bool
	if err := pool.QueryRow(ctx, `SELECT current_database(), pg_get_userbyid(relowner) = current_user FROM pg_class WHERE oid = 'users'::regclass`).Scan(&database, &owner); err != nil || database != DatabaseName || !owner {
		return Result{}, errors.New("нужны актуальная отдельная база фронтенда и её владелец схемы")
	}
	state, err := postgres.NewSchemaReadiness(pool).Check(ctx)
	if err != nil {
		return Result{}, errors.New("сначала примените обычные миграции текущей сборки")
	}
	if state.Latest != "000021_auth_audit" {
		return Result{}, errors.New("набор frontend-v1 рассчитан на схему по 000021_auth_audit; для новой схемы обновите и проверьте версию учебной миграции")
	}
	up, err := files.ReadFile("migrations/000001_frontend.up.sql")
	if err != nil {
		return Result{}, errors.New("в сборке отсутствует файл применения учебной миграции")
	}
	down, err := files.ReadFile("migrations/000001_frontend.down.sql")
	if err != nil {
		return Result{}, errors.New("в сборке отсутствует файл отката учебной миграции")
	}
	manifest, err := json.Marshal(Profiles())
	if err != nil {
		return Result{}, errors.New("не удалось подготовить манифест учебных кабинетов")
	}
	digest := sha256.Sum256(append(append(up, down...), manifest...))
	checksum := hex.EncodeToString(digest[:])
	tx, err := pool.Begin(ctx)
	if err != nil {
		return Result{}, safeError(err)
	}
	defer func() {
		rollbackContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(rollbackContext)
	}()
	// Тот же замок, что у рабочих миграций; параллельный up/down сериализован.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(1279541842); SET LOCAL lock_timeout = '5s'`); err != nil {
		return Result{}, safeError(err)
	}
	var exists bool
	if err = tx.QueryRow(ctx, `SELECT to_regclass('frontend_data_migrations') IS NOT NULL`).Scan(&exists); err != nil {
		return Result{}, safeError(err)
	}
	if !exists && direction != "status" {
		_, err = tx.Exec(ctx, `CREATE TABLE frontend_data_migrations (version TEXT PRIMARY KEY, checksum TEXT NOT NULL, applied_at TIMESTAMPTZ NOT NULL);
		REVOKE ALL ON frontend_data_migrations FROM PUBLIC, lidradar_app, lidradar_worker, lidradar_platform`)
		if err != nil {
			return Result{}, safeError(err)
		}
		exists = true
	}
	result := Result{Version: Version, Profiles: []Counts{}}
	if exists {
		var recorded string
		var at time.Time
		err = tx.QueryRow(ctx, `SELECT checksum, applied_at FROM frontend_data_migrations WHERE version=$1`, Version).Scan(&recorded, &at)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return Result{}, safeError(err)
		}
		if err == nil {
			if recorded != checksum {
				return Result{}, errors.New("контрольная сумма тестовой миграции изменилась; используйте исходную версию для отката")
			}
			result.Applied, result.AppliedAt = true, &at
		}
	}
	if (direction == "up" && !result.Applied) || (direction == "down" && result.Applied) {
		if _, err = tx.Exec(ctx, `CREATE TEMP TABLE frontend_profiles (
			number INTEGER, user_id UUID, tenant_id UUID, email TEXT, name TEXT,
			conversation_count INTEGER, location_count INTEGER, password_hash TEXT
		) ON COMMIT DROP`); err != nil {
			return Result{}, safeError(err)
		}
		for _, p := range Profiles() {
			hash := ""
			if direction == "up" {
				if len(password) < 12 || len(password) > 1024 {
					return Result{}, errors.New("пароль учебных кабинетов должен содержать 12–1024 байта")
				}
				hash, err = (cryptoplatform.PasswordHasher{}).Hash(password)
				if err != nil {
					return Result{}, errors.New("не удалось подготовить пароль")
				}
			}
			if _, err = tx.Exec(ctx, `INSERT INTO frontend_profiles VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, p.Number, p.UserID, p.TenantID, p.Email, p.Name, p.Conversations, p.Locations, hash); err != nil {
				return Result{}, safeError(err)
			}
		}
		sql := up
		if direction == "down" {
			sql = down
		}
		if _, err = tx.Exec(ctx, string(sql)); err != nil {
			return Result{}, safeError(err)
		}
		if direction == "up" {
			var at time.Time
			err = tx.QueryRow(ctx, `INSERT INTO frontend_data_migrations VALUES ($1,$2,transaction_timestamp()) RETURNING applied_at`, Version, checksum).Scan(&at)
			result.AppliedAt = &at
		} else {
			_, err = tx.Exec(ctx, `DELETE FROM frontend_data_migrations WHERE version=$1`, Version)
			result.AppliedAt = nil
		}
		if err != nil {
			return Result{}, safeError(err)
		}
		result.Changed, result.Applied = true, direction == "up"
	}
	if result.Applied {
		for _, p := range Profiles() {
			c := Counts{Email: p.Email, TenantID: p.TenantID}
			if err = tx.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM conversations WHERE tenant_id=$1),
				(SELECT count(*) FROM messages WHERE tenant_id=$1),
				(SELECT count(*) FROM opportunities WHERE tenant_id=$1),
				(SELECT count(*) FROM risk_signals WHERE tenant_id=$1)`, p.TenantID).Scan(&c.Conversations, &c.Messages, &c.Opportunities, &c.Risks); err != nil {
				return Result{}, safeError(err)
			}
			result.Profiles = append(result.Profiles, c)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return Result{}, safeError(err)
	}
	return result, nil
}

// PostgreSQL Detail может содержать пароль, адрес или пользовательский текст.
// Наружу возвращается только код, пригодный для диагностики в инструкции.
func safeError(err error) error {
	var state interface{ SQLState() string }
	if errors.As(err, &state) {
		return fmt.Errorf("тестовая миграция отменена целиком (PostgreSQL %s)", state.SQLState())
	}
	return errors.New("результат тестовой миграции не подтверждён; проверьте подключение и выполните status")
}
