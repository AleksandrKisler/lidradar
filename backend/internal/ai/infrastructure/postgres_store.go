package infrastructure

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/internal/ai/application"
	"lidradar/backend/internal/ai/domain"
	eventsdomain "lidradar/backend/internal/events/domain"
	eventsinfrastructure "lidradar/backend/internal/events/infrastructure"
)

type PostgresStore struct {
	pool   *pgxpool.Pool
	events *eventsinfrastructure.PostgresStore
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool, events: eventsinfrastructure.NewPostgresStore(pool)}
}

func (store *PostgresStore) RegisterNode(ctx context.Context, node domain.Node) error {
	if store == nil || store.pool == nil || node.ID == "" || node.Name == "" || node.TenantID == "" || node.Status != domain.NodeOffline {
		return application.ErrInvalid
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("начало регистрации AI-узла: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		INSERT INTO ai_nodes(
			id, name, secret_digest, status, available_slots, max_inflight,
			created_at, updated_at
		) VALUES ($1, $2, $3, 'OFFLINE', 0, 1, $4, $4)`,
		node.ID, node.Name, node.SecretHash[:], node.CreatedAt.UTC()); err != nil {
		return mapAIStoreError("регистрация AI-узла", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO ai_node_tenants(node_id, tenant_id, created_at)
		VALUES ($1, $2, $3)`, node.ID, node.TenantID, node.CreatedAt.UTC()); err != nil {
		return mapAIStoreError("назначение организации AI-узлу", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("фиксация регистрации AI-узла: %w", err)
	}
	return nil
}

func (store *PostgresStore) AllowNodeTenant(ctx context.Context, nodeID, tenantID string, now time.Time) error {
	if store == nil || store.pool == nil || nodeID == "" || tenantID == "" || now.IsZero() {
		return application.ErrInvalid
	}
	if _, err := store.pool.Exec(ctx, `
		INSERT INTO ai_node_tenants(node_id, tenant_id, created_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (node_id, tenant_id) DO NOTHING`, nodeID, tenantID, now.UTC()); err != nil {
		return mapAIStoreError("разрешение организации AI-узлу", err)
	}
	return nil
}

func (store *PostgresStore) RotateNodeSecret(ctx context.Context, id string, digest [32]byte, now time.Time) error {
	if store == nil || store.pool == nil || id == "" || now.IsZero() {
		return application.ErrInvalid
	}
	result, err := store.pool.Exec(ctx, `
		UPDATE ai_nodes
		SET secret_digest = $2, status = 'OFFLINE', model_version = NULL,
		    available_slots = 0, last_heartbeat_at = NULL, updated_at = $3
		WHERE id = $1 AND status <> 'REVOKED'`, id, digest[:], now.UTC())
	if err != nil {
		return mapAIStoreError("смена секрета AI-узла", err)
	}
	if result.RowsAffected() == 1 {
		return nil
	}
	var exists bool
	if err := store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM ai_nodes WHERE id = $1)`, id).Scan(&exists); err != nil {
		return mapAIStoreError("проверка AI-узла при смене секрета", err)
	}
	if exists {
		return application.ErrConflict
	}
	return application.ErrNotFound
}

func (store *PostgresStore) RevokeNode(ctx context.Context, id string, now time.Time) error {
	if store == nil || store.pool == nil || id == "" || now.IsZero() {
		return application.ErrInvalid
	}
	result, err := store.pool.Exec(ctx, `
		UPDATE ai_nodes
		SET status = 'REVOKED', model_version = NULL, available_slots = 0,
		    revoked_at = COALESCE(revoked_at, $2), updated_at = $2
		WHERE id = $1`, id, now.UTC())
	if err != nil {
		return mapAIStoreError("отзыв AI-узла", err)
	}
	if result.RowsAffected() != 1 {
		return application.ErrNotFound
	}
	return nil
}

func (store *PostgresStore) AuthenticateNode(ctx context.Context, id string, digest [32]byte) (domain.Node, bool, error) {
	if store == nil || store.pool == nil || id == "" {
		return domain.Node{}, false, application.ErrUnauthorized
	}
	node, storedDigest, err := scanNode(store.pool.QueryRow(ctx, `
		SELECT id, name, secret_digest, status, model_version, available_slots,
		       max_inflight, last_heartbeat_at, revoked_at, created_at, updated_at
		FROM ai_nodes
		WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Node{}, false, nil
	}
	if err != nil {
		return domain.Node{}, false, mapAIStoreError("проверка AI-узла", err)
	}
	if subtle.ConstantTimeCompare(storedDigest, digest[:]) != 1 {
		return domain.Node{}, false, nil
	}
	copy(node.SecretHash[:], storedDigest)
	return node, true, nil
}

func (store *PostgresStore) UseRequestNonce(
	ctx context.Context,
	nodeID, requestID string,
	now, expiresAt time.Time,
) error {
	if store == nil || store.pool == nil || nodeID == "" || requestID == "" || !expiresAt.After(now) {
		return application.ErrInvalid
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("начало фиксации запроса AI-узла: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `DELETE FROM ai_node_request_nonces WHERE expires_at <= $1`, now.UTC()); err != nil {
		return mapAIStoreError("очистка окон повтора AI-узла", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO ai_node_request_nonces(node_id, request_id, expires_at, created_at)
		VALUES ($1, $2, $3, $4)`, nodeID, requestID, expiresAt.UTC(), now.UTC()); err != nil {
		var postgresError *pgconn.PgError
		if errors.As(err, &postgresError) && postgresError.Code == "23505" {
			return application.ErrReplay
		}
		return mapAIStoreError("фиксация запроса AI-узла", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("фиксация защиты от повтора AI-узла: %w", err)
	}
	return nil
}

func (store *PostgresStore) Heartbeat(
	ctx context.Context,
	nodeID string,
	status domain.NodeStatus,
	modelVersion string,
	availableSlots int,
	now, leaseUntil time.Time,
) error {
	if store == nil || store.pool == nil || nodeID == "" || !leaseUntil.After(now) {
		return application.ErrInvalid
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("начало heartbeat AI-узла: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	result, err := tx.Exec(ctx, `
		UPDATE ai_nodes
		SET status = $2, model_version = NULLIF($3, ''), available_slots = $4,
		    last_heartbeat_at = $5, updated_at = $5
		WHERE id = $1 AND status <> 'REVOKED'`,
		nodeID, status, modelVersion, availableSlots, now.UTC())
	if err != nil {
		return mapAIStoreError("обновление heartbeat AI-узла", err)
	}
	if result.RowsAffected() != 1 {
		return application.ErrUnauthorized
	}
	if _, err := tx.Exec(ctx, `
		UPDATE ai_jobs
		SET lease_until = $3, updated_at = $2
		WHERE leased_by = $1
		  AND status IN ('LEASED', 'RUNNING')
		  AND lease_until > $2`, nodeID, now.UTC(), leaseUntil.UTC()); err != nil {
		return mapAIStoreError("продление аренды AI-заданий", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("фиксация heartbeat AI-узла: %w", err)
	}
	return nil
}

func (store *PostgresStore) Enqueue(ctx context.Context, job domain.Job) (domain.Job, error) {
	if store == nil || store.pool == nil || job.ID == "" || job.TenantID == "" ||
		job.ConversationID == "" || job.Prompt == "" || job.Status != domain.JobPending {
		return domain.Job{}, application.ErrInvalid
	}
	persisted, err := upsertQueuedJob(ctx, store.pool, job)
	if err == nil {
		return persisted, nil
	}
	if !errors.Is(err, errSnapshotAlreadyQueued) {
		return domain.Job{}, err
	}
	// Точно такой снимок уже анализировался или анализируется: повтор события
	// безопасен, а расхождение инструкции при одинаковом снимке — конфликт.
	persisted, err = scanJob(store.pool.QueryRow(ctx, `
		SELECT `+queuedJobColumns+`
		FROM ai_jobs
		WHERE tenant_id = $1 AND job_type = $2 AND entity_type = $3 AND entity_id = $4
		  AND base_conversation_revision = $5 AND model_requirement = $6
		  AND schema_version = $7 AND prompt_version = $8`,
		job.TenantID, job.JobType, job.EntityType, job.ConversationID,
		job.BaseConversationRevision, job.ModelVersion, job.SchemaVersion, job.PromptVersion))
	if err != nil {
		return domain.Job{}, mapAIStoreError("чтение повторного AI-задания", err)
	}
	if persisted.Prompt != job.Prompt || persisted.AnalysisThroughMessageID != job.AnalysisThroughMessageID {
		return domain.Job{}, application.ErrConflict
	}
	return persisted, nil
}

const queuedJobColumns = `id, tenant_id, job_type, entity_type, entity_id, priority, payload,
		       model_requirement, schema_version, prompt_version,
		       base_conversation_revision, analysis_through_message_id,
		       status, attempts, max_attempts, available_at, leased_by, lease_until, leased_at,
		       last_error_code, completed_at, created_at, updated_at`

// errSnapshotAlreadyQueued означает, что задание с тем же снимком переписки и
// теми же версиями уже существует в любом состоянии (ai_jobs_snapshot_unique).
var errSnapshotAlreadyQueued = errors.New("снимок переписки уже поставлен в очередь")

type rowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// upsertQueuedJob держит не более одного ожидающего задания на сущность
// (ai_jobs_one_queued_per_entity_idx). Более свежий снимок заменяет инструкцию
// уже ожидающего задания, сохраняя его идентификатор и более ранний
// available_at; более старый снимок ничего не меняет. Захваченное или
// выполняющееся задание не трогается: его инструкция уже у узла, а снимок
// запуска копируется из строки при старте.
func upsertQueuedJob(ctx context.Context, querier rowQuerier, job domain.Job) (domain.Job, error) {
	payload, err := json.Marshal(map[string]string{"prompt": job.Prompt})
	if err != nil {
		return domain.Job{}, fmt.Errorf("кодирование AI-задания: %w", err)
	}
	persisted, err := scanJob(querier.QueryRow(ctx, `
		INSERT INTO ai_jobs(
			id, tenant_id, job_type, entity_type, entity_id, priority, payload,
			model_requirement, schema_version, prompt_version,
			base_conversation_revision, analysis_through_message_id,
			status, attempts, max_attempts, available_at, created_at, updated_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7::jsonb,
			$8, $9, $10, $11, $12,
			'PENDING', 0, $13, $14, $15, $15
		) ON CONFLICT (tenant_id, entity_type, entity_id) WHERE status IN ('PENDING', 'RETRY')
		DO UPDATE SET
			payload = EXCLUDED.payload,
			priority = GREATEST(ai_jobs.priority, EXCLUDED.priority),
			model_requirement = EXCLUDED.model_requirement,
			schema_version = EXCLUDED.schema_version,
			prompt_version = EXCLUDED.prompt_version,
			base_conversation_revision = EXCLUDED.base_conversation_revision,
			analysis_through_message_id = EXCLUDED.analysis_through_message_id,
			available_at = LEAST(ai_jobs.available_at, EXCLUDED.available_at),
			updated_at = EXCLUDED.updated_at
		WHERE ai_jobs.base_conversation_revision <= EXCLUDED.base_conversation_revision
		RETURNING `+queuedJobColumns,
		job.ID, job.TenantID, job.JobType, job.EntityType, job.ConversationID,
		job.Priority, payload, job.ModelVersion, job.SchemaVersion, job.PromptVersion,
		job.BaseConversationRevision, job.AnalysisThroughMessageID, job.MaxAttempts,
		job.AvailableAt.UTC(), job.CreatedAt.UTC()))
	if err == nil {
		return persisted, nil
	}
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) && postgresError.Code == "23505" &&
		postgresError.ConstraintName == "ai_jobs_snapshot_unique" {
		return domain.Job{}, errSnapshotAlreadyQueued
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return domain.Job{}, mapAIStoreError("добавление AI-задания", err)
	}
	// Ожидающее задание уже несёт более свежий снимок: возвращаем его.
	persisted, err = scanJob(querier.QueryRow(ctx, `
		SELECT `+queuedJobColumns+`
		FROM ai_jobs
		WHERE tenant_id = $1 AND entity_type = $2 AND entity_id = $3
		  AND status IN ('PENDING', 'RETRY')`,
		job.TenantID, job.EntityType, job.ConversationID))
	if err != nil {
		return domain.Job{}, mapAIStoreError("чтение ожидающего AI-задания", err)
	}
	return persisted, nil
}

func (store *PostgresStore) Claim(
	ctx context.Context,
	nodeID string,
	now, leaseUntil time.Time,
) (domain.Job, bool, error) {
	if store == nil || store.pool == nil || nodeID == "" || !leaseUntil.After(now) {
		return domain.Job{}, false, application.ErrInvalid
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return domain.Job{}, false, fmt.Errorf("начало захвата AI-задания: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var nodeStatus domain.NodeStatus
	var availableSlots, maxInflight int
	var lastHeartbeat *time.Time
	var modelVersion *string
	if err := tx.QueryRow(ctx, `
		SELECT status, model_version, available_slots, max_inflight, last_heartbeat_at
		FROM ai_nodes WHERE id = $1 FOR UPDATE`, nodeID).Scan(&nodeStatus, &modelVersion, &availableSlots, &maxInflight, &lastHeartbeat); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Job{}, false, application.ErrUnauthorized
		}
		return domain.Job{}, false, mapAIStoreError("чтение AI-узла", err)
	}
	if nodeStatus != domain.NodeReady || modelVersion == nil || availableSlots < 1 || lastHeartbeat == nil ||
		now.Sub(lastHeartbeat.UTC()) > application.NodeUnavailableAfter {
		if nodeStatus == domain.NodeReady && (lastHeartbeat == nil || now.Sub(lastHeartbeat.UTC()) > application.NodeUnavailableAfter) {
			if _, err := tx.Exec(ctx, `UPDATE ai_nodes SET status = 'OFFLINE', available_slots = 0, updated_at = $2 WHERE id = $1`, nodeID, now.UTC()); err != nil {
				return domain.Job{}, false, mapAIStoreError("пометка AI-узла недоступным", err)
			}
			if err := tx.Commit(ctx); err != nil {
				return domain.Job{}, false, fmt.Errorf("фиксация недоступности AI-узла: %w", err)
			}
		}
		return domain.Job{}, false, nil
	}
	var inflight int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM ai_jobs
		WHERE leased_by = $1 AND status IN ('LEASED', 'RUNNING') AND lease_until > $2`,
		nodeID, now.UTC()).Scan(&inflight); err != nil {
		return domain.Job{}, false, mapAIStoreError("подсчёт заданий AI-узла", err)
	}
	if inflight >= maxInflight {
		return domain.Job{}, false, nil
	}
	// Аренда теряется по скользящему сроку (lease_until, продлевается heartbeat)
	// либо по абсолютному потолку от момента захвата (leased_at + LeaseCap),
	// который не продлевается никогда (§3.5, LR-BE-RM-016).
	capBefore := now.Add(-application.LeaseCap).UTC()
	if _, err := tx.Exec(ctx, `
		UPDATE ai_runs AS run
		SET status = 'FAILED', application_status = 'REJECTED',
		    error_code = CASE WHEN job.lease_until <= $1 THEN 'LEASE_EXPIRED_MAX_ATTEMPTS' ELSE 'LEASE_CAP_EXCEEDED' END,
		    completed_at = $1
		FROM ai_jobs AS job
		WHERE run.job_id = job.id AND run.status = 'RUNNING'
		  AND EXISTS (
			SELECT 1 FROM ai_node_tenants AS allowed
			WHERE allowed.node_id = $2 AND allowed.tenant_id = job.tenant_id
		  )
		  AND job.status IN ('LEASED', 'RUNNING')
		  AND (job.lease_until <= $1 OR job.leased_at <= $3)
		  AND job.attempts >= job.max_attempts`, now.UTC(), nodeID, capBefore); err != nil {
		return domain.Job{}, false, mapAIStoreError("завершение попыток AI с исчерпанной арендой", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE ai_jobs AS job
		SET status = 'DEAD', leased_by = NULL, lease_until = NULL, leased_at = NULL,
		    last_error_code = CASE WHEN lease_until <= $1 THEN 'LEASE_EXPIRED_MAX_ATTEMPTS' ELSE 'LEASE_CAP_EXCEEDED' END,
		    completed_at = $1, updated_at = $1
		WHERE status IN ('LEASED', 'RUNNING') AND (lease_until <= $1 OR leased_at <= $3)
		  AND attempts >= max_attempts
		  AND EXISTS (
			SELECT 1 FROM ai_node_tenants AS allowed
			WHERE allowed.node_id = $2 AND allowed.tenant_id = job.tenant_id
		  )`, now.UTC(), nodeID, capBefore); err != nil {
		return domain.Job{}, false, mapAIStoreError("завершение AI-заданий с исчерпанными попытками", err)
	}
	job, err := scanJob(tx.QueryRow(ctx, `
		SELECT id, tenant_id, job_type, entity_type, entity_id, priority, payload,
		       model_requirement, schema_version, prompt_version,
		       base_conversation_revision, analysis_through_message_id,
		       status, attempts, max_attempts, available_at, leased_by, lease_until, leased_at,
		       last_error_code, completed_at, created_at, updated_at
		FROM ai_jobs AS job
		WHERE model_requirement = $2
		  AND EXISTS (
			SELECT 1 FROM ai_node_tenants AS allowed
			WHERE allowed.node_id = $3 AND allowed.tenant_id = job.tenant_id
		  )
		  AND attempts < max_attempts AND (
			(status IN ('PENDING', 'RETRY') AND available_at <= $1)
			OR (status IN ('LEASED', 'RUNNING') AND (lease_until <= $1 OR leased_at <= $4))
		)
		ORDER BY priority DESC, available_at, created_at, id
		FOR UPDATE SKIP LOCKED
		LIMIT 1`, now.UTC(), *modelVersion, nodeID, capBefore))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Job{}, false, nil
	}
	if err != nil {
		return domain.Job{}, false, mapAIStoreError("поиск AI-задания", err)
	}
	if job.Status == domain.JobLeased || job.Status == domain.JobRunning {
		lostCode := "LEASE_EXPIRED"
		if job.LeaseUntil.After(now) {
			lostCode = "LEASE_CAP_EXCEEDED"
		}
		if _, err := tx.Exec(ctx, `
			UPDATE ai_runs
			SET status = 'FAILED', application_status = 'REJECTED',
			    error_code = $3, completed_at = $2
			WHERE job_id = $1 AND status = 'RUNNING'`, job.ID, now.UTC(), lostCode); err != nil {
			return domain.Job{}, false, mapAIStoreError("завершение потерянной попытки AI", err)
		}
	}
	job.Status, job.LeasedBy, job.LeaseUntil, job.LeasedAt = domain.JobLeased, nodeID, leaseUntil.UTC(), now.UTC()
	job.Attempts++
	job.UpdatedAt = now.UTC()
	if _, err := tx.Exec(ctx, `
		UPDATE ai_jobs
		SET status = 'LEASED', attempts = attempts + 1, leased_by = $2,
		    lease_until = $3, leased_at = $4, last_error_code = NULL, completed_at = NULL, updated_at = $4
		WHERE id = $1`, job.ID, nodeID, leaseUntil.UTC(), now.UTC()); err != nil {
		return domain.Job{}, false, mapAIStoreError("захват AI-задания", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE ai_nodes SET available_slots = 0, updated_at = $2 WHERE id = $1`, nodeID, now.UTC()); err != nil {
		return domain.Job{}, false, mapAIStoreError("обновление ёмкости AI-узла", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Job{}, false, fmt.Errorf("фиксация захвата AI-задания: %w", err)
	}
	return job, true, nil
}

func (store *PostgresStore) Start(ctx context.Context, run domain.Run, now time.Time) (domain.Run, error) {
	if store == nil || store.pool == nil || run.ID == "" || run.JobID == "" || run.NodeID == "" || now.IsZero() {
		return domain.Run{}, application.ErrInvalid
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return domain.Run{}, fmt.Errorf("начало AI-run: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	job, err := scanJob(tx.QueryRow(ctx, `
		SELECT id, tenant_id, job_type, entity_type, entity_id, priority, payload,
		       model_requirement, schema_version, prompt_version,
		       base_conversation_revision, analysis_through_message_id,
		       status, attempts, max_attempts, available_at, leased_by, lease_until, leased_at,
		       last_error_code, completed_at, created_at, updated_at
		FROM ai_jobs WHERE id = $1 FOR UPDATE`, run.JobID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Run{}, application.ErrNotFound
	}
	if err != nil {
		return domain.Run{}, mapAIStoreError("чтение AI-задания", err)
	}
	if job.LeasedBy != run.NodeID || (job.Status != domain.JobLeased && job.Status != domain.JobRunning) || !job.LeaseUntil.After(now) {
		return domain.Run{}, application.ErrLeaseLost
	}
	existing, err := scanRun(tx.QueryRow(ctx, `
		SELECT id, job_id, node_id, tenant_id, entity_id, status, application_status,
		       base_conversation_revision, analysis_through_message_id,
		       model_version, prompt_version, schema_version,
		       raw_output, error_code, validation_error, started_at, completed_at
		FROM ai_runs WHERE job_id = $1 AND status = 'RUNNING'`, job.ID))
	if err == nil {
		if existing.NodeID != run.NodeID {
			return domain.Run{}, application.ErrLeaseLost
		}
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return domain.Run{}, mapAIStoreError("чтение активного AI-run", err)
	}
	run.TenantID, run.ConversationID = job.TenantID, job.ConversationID
	run.BaseConversationRevision, run.AnalysisThroughMessageID = job.BaseConversationRevision, job.AnalysisThroughMessageID
	run.ModelVersion, run.PromptVersion, run.SchemaVersion = job.ModelVersion, job.PromptVersion, job.SchemaVersion
	if _, err := tx.Exec(ctx, `
		INSERT INTO ai_runs(
			id, tenant_id, job_id, node_id, entity_type, entity_id,
			status, application_status, base_conversation_revision,
			analysis_through_message_id, model_version, prompt_version,
			schema_version, started_at
		) VALUES ($1, $2, $3, $4, 'CONVERSATION', $5, 'RUNNING', 'PENDING', $6, $7, $8, $9, $10, $11)`,
		run.ID, run.TenantID, run.JobID, run.NodeID, run.ConversationID,
		run.BaseConversationRevision, run.AnalysisThroughMessageID,
		run.ModelVersion, run.PromptVersion, run.SchemaVersion, run.StartedAt.UTC()); err != nil {
		return domain.Run{}, mapAIStoreError("создание AI-run", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE ai_jobs SET status = 'RUNNING', updated_at = $2 WHERE id = $1`, job.ID, now.UTC()); err != nil {
		return domain.Run{}, mapAIStoreError("запуск AI-задания", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Run{}, fmt.Errorf("фиксация AI-run: %w", err)
	}
	return run, nil
}

func (store *PostgresStore) Run(ctx context.Context, id string) (domain.Run, error) {
	if store == nil || store.pool == nil || id == "" {
		return domain.Run{}, application.ErrInvalid
	}
	run, err := scanRun(store.pool.QueryRow(ctx, `
		SELECT id, job_id, node_id, tenant_id, entity_id, status, application_status,
		       base_conversation_revision, analysis_through_message_id,
		       model_version, prompt_version, schema_version,
		       raw_output, error_code, validation_error, started_at, completed_at
		FROM ai_runs WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Run{}, application.ErrNotFound
	}
	if err != nil {
		return domain.Run{}, mapAIStoreError("чтение AI-run", err)
	}
	return run, nil
}

func (store *PostgresStore) ConversationSnapshot(ctx context.Context, tenantID, conversationID string) (domain.ConversationSnapshot, error) {
	if store == nil || store.pool == nil || tenantID == "" || conversationID == "" {
		return domain.ConversationSnapshot{}, application.ErrInvalid
	}
	var snapshot domain.ConversationSnapshot
	err := store.pool.QueryRow(ctx, `
		SELECT conversation.revision,
		       COALESCE((
			SELECT message.id::text
			FROM messages AS message
			WHERE message.tenant_id = conversation.tenant_id
			  AND message.conversation_id = conversation.id
			  AND message.direction IN ('INCOMING', 'OUTGOING')
			  AND message.provider_deleted_at IS NULL
			  AND message.text IS NOT NULL AND btrim(message.text) <> ''
			ORDER BY message.sent_at DESC, message.id DESC
			LIMIT 1
		), '')
		FROM conversations AS conversation
		WHERE conversation.tenant_id = $1 AND conversation.id = $2`, tenantID, conversationID).
		Scan(&snapshot.Revision, &snapshot.LastMessageID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ConversationSnapshot{}, application.ErrNotFound
	}
	if err != nil {
		return domain.ConversationSnapshot{}, mapAIStoreError("чтение снимка переписки", err)
	}
	if snapshot.Revision < 1 || snapshot.LastMessageID == "" {
		return domain.ConversationSnapshot{}, application.ErrInvalid
	}
	return snapshot, nil
}

func (store *PostgresStore) Finalize(ctx context.Context, final application.Finalization) (domain.Run, error) {
	if store == nil || store.pool == nil || final.NodeID == "" || final.JobID == "" || final.RunID == "" || final.CompletedAt.IsZero() {
		return domain.Run{}, application.ErrInvalid
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return domain.Run{}, fmt.Errorf("начало завершения AI-run: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	job, err := scanJob(tx.QueryRow(ctx, `
		SELECT id, tenant_id, job_type, entity_type, entity_id, priority, payload,
		       model_requirement, schema_version, prompt_version,
		       base_conversation_revision, analysis_through_message_id,
		       status, attempts, max_attempts, available_at, leased_by, lease_until, leased_at,
		       last_error_code, completed_at, created_at, updated_at
		FROM ai_jobs WHERE id = $1 FOR UPDATE`, final.JobID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Run{}, application.ErrNotFound
	}
	if err != nil {
		return domain.Run{}, mapAIStoreError("чтение завершаемого AI-задания", err)
	}
	run, err := scanRun(tx.QueryRow(ctx, `
		SELECT id, job_id, node_id, tenant_id, entity_id, status, application_status,
		       base_conversation_revision, analysis_through_message_id,
		       model_version, prompt_version, schema_version,
		       raw_output, error_code, validation_error, started_at, completed_at
		FROM ai_runs WHERE id = $1 FOR UPDATE`, final.RunID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Run{}, application.ErrNotFound
	}
	if err != nil {
		return domain.Run{}, mapAIStoreError("чтение завершаемого AI-run", err)
	}
	if run.Status != domain.RunRunning {
		if run.NodeID == final.NodeID && run.Status == final.RunStatus &&
			run.Output == final.Output && run.ErrorCode == final.ErrorCode {
			return run, nil
		}
		return domain.Run{}, application.ErrLeaseLost
	}
	if job.LeasedBy != final.NodeID || run.NodeID != final.NodeID ||
		job.Status != domain.JobRunning || !job.LeaseUntil.After(final.CompletedAt) {
		return domain.Run{}, application.ErrLeaseLost
	}
	if final.Summary != nil {
		if final.Summary.TenantID != run.TenantID || final.Summary.ConversationID != run.ConversationID ||
			final.Summary.BaseConversationRevision != run.BaseConversationRevision ||
			final.Summary.AnalysisThroughMessageID != run.AnalysisThroughMessageID || final.Summary.RunID != run.ID {
			return domain.Run{}, application.ErrInvalid
		}
		var revision int64
		var lastMessageID string
		if err := tx.QueryRow(ctx, `
			SELECT conversation.revision,
			       COALESCE((
				SELECT message.id::text
				FROM messages AS message
				WHERE message.tenant_id = conversation.tenant_id
				  AND message.conversation_id = conversation.id
				  AND message.direction IN ('INCOMING', 'OUTGOING')
				  AND message.provider_deleted_at IS NULL
				  AND message.text IS NOT NULL AND btrim(message.text) <> ''
				ORDER BY message.sent_at DESC, message.id DESC
				LIMIT 1
			), '')
			FROM conversations AS conversation
			WHERE conversation.tenant_id = $1 AND conversation.id = $2
			FOR UPDATE`, run.TenantID, run.ConversationID).Scan(&revision, &lastMessageID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.Run{}, application.ErrNotFound
			}
			return domain.Run{}, mapAIStoreError("атомарная проверка свежести AI-run", err)
		}
		if revision != run.BaseConversationRevision || lastMessageID != run.AnalysisThroughMessageID {
			return domain.Run{}, application.ErrFreshnessChanged
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE ai_runs
		SET status = $2, application_status = $3,
		    raw_output = NULLIF($4, ''), error_code = NULLIF($5, ''),
		    validation_error = NULLIF($6, ''), completed_at = $7
		WHERE id = $1`, final.RunID, final.RunStatus, final.ApplicationStatus,
		final.Output, final.ErrorCode, final.ValidationError, final.CompletedAt.UTC()); err != nil {
		return domain.Run{}, mapAIStoreError("завершение AI-run", err)
	}
	if final.RunStatus == domain.RunSucceeded {
		job.Status, job.CompletedAt = domain.JobSucceeded, final.CompletedAt.UTC()
		if _, err := tx.Exec(ctx, `
			UPDATE ai_jobs
			SET status = 'SUCCEEDED', leased_by = NULL, lease_until = NULL, leased_at = NULL,
			    completed_at = $2, updated_at = $2
			WHERE id = $1`, job.ID, final.CompletedAt.UTC()); err != nil {
			return domain.Run{}, mapAIStoreError("успешное завершение AI-задания", err)
		}
	} else if job.Attempts < job.MaxAttempts {
		job.Status, job.AvailableAt = domain.JobRetry, final.CompletedAt.Add(5*time.Second)
		if _, err := tx.Exec(ctx, `
			UPDATE ai_jobs
			SET status = 'RETRY', leased_by = NULL, lease_until = NULL, leased_at = NULL,
			    last_error_code = $2, available_at = $3, updated_at = $4
			WHERE id = $1`, job.ID, final.ErrorCode, job.AvailableAt.UTC(), final.CompletedAt.UTC()); err != nil {
			return domain.Run{}, mapAIStoreError("повтор AI-задания", err)
		}
	} else {
		job.Status, job.CompletedAt = domain.JobDead, final.CompletedAt.UTC()
		if _, err := tx.Exec(ctx, `
			UPDATE ai_jobs
			SET status = 'DEAD', leased_by = NULL, lease_until = NULL, leased_at = NULL,
			    last_error_code = $2, completed_at = $3, updated_at = $3
			WHERE id = $1`, job.ID, final.ErrorCode, final.CompletedAt.UTC()); err != nil {
			return domain.Run{}, mapAIStoreError("окончательное завершение AI-задания", err)
		}
	}
	if final.Summary != nil {
		summary := final.Summary
		facts, marshalErr := json.Marshal(summary.Facts)
		if marshalErr != nil {
			return domain.Run{}, fmt.Errorf("кодирование смысловых фактов AI: %w", marshalErr)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO conversation_summaries(
				tenant_id, conversation_id, summary_text, base_conversation_revision,
				analysis_through_message_id, model_version, prompt_version,
				schema_version, ai_run_id, semantic_facts, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb, $11)
			ON CONFLICT (tenant_id, conversation_id) DO UPDATE
			SET summary_text = EXCLUDED.summary_text,
			    base_conversation_revision = EXCLUDED.base_conversation_revision,
			    analysis_through_message_id = EXCLUDED.analysis_through_message_id,
			    model_version = EXCLUDED.model_version,
			    prompt_version = EXCLUDED.prompt_version,
			    schema_version = EXCLUDED.schema_version,
			    ai_run_id = EXCLUDED.ai_run_id,
			    semantic_facts = EXCLUDED.semantic_facts,
			    updated_at = EXCLUDED.updated_at
			WHERE conversation_summaries.base_conversation_revision <= EXCLUDED.base_conversation_revision`,
			summary.TenantID, summary.ConversationID, summary.Text,
			summary.BaseConversationRevision, summary.AnalysisThroughMessageID,
			summary.ModelVersion, summary.PromptVersion, summary.SchemaVersion,
			summary.RunID, facts, summary.UpdatedAt.UTC()); err != nil {
			return domain.Run{}, mapAIStoreError("сохранение производного резюме", err)
		}
		if err := store.appendAnalysisAppliedEvent(ctx, tx, *summary, final.CompletedAt); err != nil {
			return domain.Run{}, err
		}
	}
	if final.Replacement != nil {
		// Точка сохранения изолирует конфликт снимка: если задание с тем же
		// снимком уже существует, замена не нужна, а завершение run не откатывается.
		savepoint, err := tx.Begin(ctx)
		if err != nil {
			return domain.Run{}, fmt.Errorf("точка сохранения повторного AI-задания: %w", err)
		}
		if _, err := upsertQueuedJob(ctx, savepoint, *final.Replacement); err != nil {
			_ = savepoint.Rollback(ctx)
			if !errors.Is(err, errSnapshotAlreadyQueued) {
				return domain.Run{}, err
			}
		} else if err := savepoint.Commit(ctx); err != nil {
			return domain.Run{}, fmt.Errorf("фиксация повторного AI-задания: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Run{}, fmt.Errorf("фиксация завершения AI-run: %w", err)
	}
	run.Status, run.ApplicationStatus, run.CompletedAt = final.RunStatus, final.ApplicationStatus, final.CompletedAt.UTC()
	run.Output, run.ErrorCode, run.ValidationError = final.Output, final.ErrorCode, final.ValidationError
	return run, nil
}

type analysisAppliedEventData struct {
	ConversationID           string `json:"conversationId"`
	RunID                    string `json:"runId"`
	BaseConversationRevision int64  `json:"baseConversationRevision"`
	AnalysisThroughMessageID string `json:"analysisThroughMessageId"`
}

func (store *PostgresStore) appendAnalysisAppliedEvent(
	ctx context.Context,
	tx pgx.Tx,
	summary domain.ConversationSummary,
	at time.Time,
) error {
	if store.events == nil {
		return errors.New("исходящий журнал AI не настроен")
	}
	data, err := json.Marshal(analysisAppliedEventData{
		ConversationID: summary.ConversationID, RunID: summary.RunID,
		BaseConversationRevision: summary.BaseConversationRevision,
		AnalysisThroughMessageID: summary.AnalysisThroughMessageID,
	})
	if err != nil {
		return fmt.Errorf("подготовка события применённого анализа: %w", err)
	}
	event, err := eventsdomain.NewEvent(
		summary.RunID, "ai.analysis.applied", 1, summary.TenantID,
		"ai_run", summary.RunID, summary.RunID, data, at.UTC(),
	)
	if err != nil {
		return fmt.Errorf("проверка события применённого анализа: %w", err)
	}
	if _, _, err := store.events.AppendTx(ctx, tx, event); err != nil {
		return fmt.Errorf("добавление события применённого анализа: %w", err)
	}
	return nil
}

type scanner interface{ Scan(...any) error }

func scanNode(row scanner) (domain.Node, []byte, error) {
	var node domain.Node
	var digest []byte
	var modelVersion *string
	var heartbeat, revoked *time.Time
	if err := row.Scan(
		&node.ID, &node.Name, &digest, &node.Status, &modelVersion,
		&node.AvailableSlots, &node.MaxInflight, &heartbeat, &revoked,
		&node.CreatedAt, &node.UpdatedAt,
	); err != nil {
		return domain.Node{}, nil, err
	}
	if len(digest) != sha256Size {
		return domain.Node{}, nil, fmt.Errorf("AI node secret digest has invalid length")
	}
	if modelVersion != nil {
		node.ModelVersion = *modelVersion
	}
	if heartbeat != nil {
		node.LastHeartbeatAt = heartbeat.UTC()
	}
	if revoked != nil {
		node.RevokedAt = revoked.UTC()
	}
	return node, digest, nil
}

const sha256Size = 32

func scanJob(row scanner) (domain.Job, error) {
	var job domain.Job
	var payload []byte
	var leasedBy, lastError *string
	var leaseUntil, leasedAt, completedAt *time.Time
	if err := row.Scan(
		&job.ID, &job.TenantID, &job.JobType, &job.EntityType, &job.ConversationID,
		&job.Priority, &payload, &job.ModelVersion, &job.SchemaVersion, &job.PromptVersion,
		&job.BaseConversationRevision, &job.AnalysisThroughMessageID,
		&job.Status, &job.Attempts, &job.MaxAttempts, &job.AvailableAt,
		&leasedBy, &leaseUntil, &leasedAt, &lastError, &completedAt, &job.CreatedAt, &job.UpdatedAt,
	); err != nil {
		return domain.Job{}, err
	}
	var body struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(payload, &body); err != nil || body.Prompt == "" {
		return domain.Job{}, fmt.Errorf("AI job payload is invalid")
	}
	job.Prompt = body.Prompt
	if leasedBy != nil {
		job.LeasedBy = *leasedBy
	}
	if leaseUntil != nil {
		job.LeaseUntil = leaseUntil.UTC()
	}
	if leasedAt != nil {
		job.LeasedAt = leasedAt.UTC()
	}
	if lastError != nil {
		job.LastErrorCode = *lastError
	}
	if completedAt != nil {
		job.CompletedAt = completedAt.UTC()
	}
	return job, nil
}

func scanRun(row scanner) (domain.Run, error) {
	var run domain.Run
	var output, errorCode, validationError *string
	var completedAt *time.Time
	if err := row.Scan(
		&run.ID, &run.JobID, &run.NodeID, &run.TenantID, &run.ConversationID,
		&run.Status, &run.ApplicationStatus, &run.BaseConversationRevision,
		&run.AnalysisThroughMessageID, &run.ModelVersion, &run.PromptVersion,
		&run.SchemaVersion, &output, &errorCode, &validationError,
		&run.StartedAt, &completedAt,
	); err != nil {
		return domain.Run{}, err
	}
	if output != nil {
		run.Output = *output
	}
	if errorCode != nil {
		run.ErrorCode = *errorCode
	}
	if validationError != nil {
		run.ValidationError = *validationError
	}
	if completedAt != nil {
		run.CompletedAt = completedAt.UTC()
	}
	return run, nil
}

func mapAIStoreError(operation string, err error) error {
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) {
		switch postgresError.Code {
		case "23503":
			return application.ErrNotFound
		case "23505", "23514", "22P02":
			return application.ErrInvalid
		}
	}
	return fmt.Errorf("%s: %w", operation, err)
}
