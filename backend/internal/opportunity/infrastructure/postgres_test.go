package infrastructure

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/internal/opportunity/domain"
	"lidradar/backend/internal/testsupport"
	"lidradar/backend/platform/ids"
)

func TestPostgresOpportunityHistoryTransitionAndTenantIsolation(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	pair := testsupport.TwoTenants(t, ctx, pool)
	conversationID := insertOpportunityConversation(t, pool, pair.A.TenantID, pair.A.LocationID)
	repository := NewPostgresRepository(pool)
	generator := ids.Generator{}
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

	opportunity := newOpportunity(t, generator, pair.A.TenantID, conversationID, now)
	historyID, _ := generator.NewID()
	history, err := domain.NewHistory(
		historyID, pair.A.TenantID, opportunity.ID, nil, domain.StageNew, domain.SourceRule, nil, nil, nil, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	created, wasCreated, err := repository.Create(ctx, opportunity, history)
	if err != nil || !wasCreated || created.ID != opportunity.ID {
		t.Fatalf("Create() = %#v, %v, %v", created, wasCreated, err)
	}

	duplicate := newOpportunity(t, generator, pair.A.TenantID, conversationID, now.Add(time.Second))
	duplicateHistoryID, _ := generator.NewID()
	duplicateHistory, _ := domain.NewHistory(
		duplicateHistoryID, pair.A.TenantID, duplicate.ID, nil, domain.StageNew, domain.SourceRule, nil, nil, nil, now,
	)
	existing, wasCreated, err := repository.Create(ctx, duplicate, duplicateHistory)
	if err != nil || wasCreated || existing.ID != opportunity.ID {
		t.Fatalf("повторный Create() = %#v, %v, %v", existing, wasCreated, err)
	}

	actor := pair.A.UserID
	transitionHistoryID, _ := generator.NewID()
	transitioned, changed, err := repository.Transition(ctx, domain.TransitionCommand{
		TenantID: pair.A.TenantID, OpportunityID: opportunity.ID, HistoryID: transitionHistoryID,
		ToStage: domain.StagePriceSent, Source: domain.SourceUser, ActorUserID: &actor, At: now.Add(time.Minute),
	})
	if err != nil || !changed || transitioned.Stage != domain.StagePriceSent {
		t.Fatalf("Transition() = %#v, %v, %v", transitioned, changed, err)
	}
	repeatHistoryID, _ := generator.NewID()
	_, changed, err = repository.Transition(ctx, domain.TransitionCommand{
		TenantID: pair.A.TenantID, OpportunityID: opportunity.ID, HistoryID: repeatHistoryID,
		ToStage: domain.StagePriceSent, Source: domain.SourceUser, ActorUserID: &actor, At: now.Add(2 * time.Minute),
	})
	if err != nil || changed {
		t.Fatalf("повторный Transition() = %v, %v", changed, err)
	}
	backwardHistoryID, _ := generator.NewID()
	_, _, err = repository.Transition(ctx, domain.TransitionCommand{
		TenantID: pair.A.TenantID, OpportunityID: opportunity.ID, HistoryID: backwardHistoryID,
		ToStage: domain.StageEngaged, Source: domain.SourceUser, ActorUserID: &actor, At: now.Add(3 * time.Minute),
	})
	if !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("обратный переход: %v", err)
	}

	detail, found, err := repository.Detail(ctx, pair.A.TenantID, opportunity.ID)
	if err != nil || !found || len(detail.History) != 2 || detail.History[1].FromStage == nil ||
		*detail.History[1].FromStage != domain.StageNew || detail.History[1].Source != domain.SourceUser ||
		detail.History[1].ActorUserID == nil || *detail.History[1].ActorUserID != actor {
		t.Fatalf("Detail() = %#v, %v, %v", detail, found, err)
	}
	if _, found, err := repository.Detail(ctx, pair.B.TenantID, opportunity.ID); err != nil || found {
		t.Fatalf("чужая возможность: found=%v, err=%v", found, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE opportunity_stage_history SET source = 'RULE' WHERE id = $1`, history.ID); err == nil {
		t.Fatal("база разрешила изменить append-only историю")
	}
}

func TestPostgresOneActiveOpportunitySurvivesConcurrentCandidates(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	pair := testsupport.TwoTenants(t, ctx, pool)
	conversationID := insertOpportunityConversation(t, pool, pair.A.TenantID, pair.A.LocationID)
	repository := NewPostgresRepository(pool)
	generator := ids.Generator{}
	now := time.Now().UTC()

	const workers = 24
	type candidate struct {
		opportunity domain.Opportunity
		history     domain.StageHistory
	}
	candidates := make([]candidate, 0, workers)
	for index := 0; index < workers; index++ {
		opportunity := newOpportunity(t, generator, pair.A.TenantID, conversationID, now)
		historyID, err := generator.NewID()
		if err != nil {
			t.Fatal(err)
		}
		history, err := domain.NewHistory(
			historyID, pair.A.TenantID, opportunity.ID, nil, domain.StageNew, domain.SourceRule, nil, nil, nil, now,
		)
		if err != nil {
			t.Fatal(err)
		}
		candidates = append(candidates, candidate{opportunity: opportunity, history: history})
	}
	var wait sync.WaitGroup
	errorsChannel := make(chan error, workers)
	createdChannel := make(chan bool, workers)
	for _, item := range candidates {
		wait.Add(1)
		go func(item candidate) {
			defer wait.Done()
			_, created, err := repository.Create(ctx, item.opportunity, item.history)
			errorsChannel <- err
			createdChannel <- created
		}(item)
	}
	wait.Wait()
	close(errorsChannel)
	close(createdChannel)
	for err := range errorsChannel {
		if err != nil {
			t.Fatalf("конкурентный Create(): %v", err)
		}
	}
	createdCount := 0
	for created := range createdChannel {
		if created {
			createdCount++
		}
	}
	var opportunities, history int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM opportunities WHERE tenant_id = $1`, pair.A.TenantID).Scan(&opportunities); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM opportunity_stage_history WHERE tenant_id = $1`, pair.A.TenantID).Scan(&history); err != nil {
		t.Fatal(err)
	}
	if createdCount != 1 || opportunities != 1 || history != 1 {
		t.Fatalf("создано=%d, opportunities=%d, history=%d", createdCount, opportunities, history)
	}
}

func TestConcurrentIdenticalStageTransitionAddsOneHistoryEntry(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	pair := testsupport.TwoTenants(t, ctx, pool)
	conversationID := insertOpportunityConversation(t, pool, pair.A.TenantID, pair.A.LocationID)
	repository := NewPostgresRepository(pool)
	generator := ids.Generator{}
	now := time.Now().UTC()
	opportunity := newOpportunity(t, generator, pair.A.TenantID, conversationID, now)
	initialID, _ := generator.NewID()
	initial, _ := domain.NewHistory(
		initialID, pair.A.TenantID, opportunity.ID, nil, domain.StageNew, domain.SourceRule, nil, nil, nil, now,
	)
	if _, _, err := repository.Create(ctx, opportunity, initial); err != nil {
		t.Fatal(err)
	}

	const workers = 16
	commands := make([]domain.TransitionCommand, 0, workers)
	for index := 0; index < workers; index++ {
		historyID, err := generator.NewID()
		if err != nil {
			t.Fatal(err)
		}
		actor := pair.A.UserID
		commands = append(commands, domain.TransitionCommand{
			TenantID: pair.A.TenantID, OpportunityID: opportunity.ID, HistoryID: historyID,
			ToStage: domain.StageQualifying, Source: domain.SourceUser, ActorUserID: &actor, At: now.Add(time.Minute),
		})
	}
	var wait sync.WaitGroup
	errorsChannel := make(chan error, workers)
	changedChannel := make(chan bool, workers)
	for _, command := range commands {
		wait.Add(1)
		go func(command domain.TransitionCommand) {
			defer wait.Done()
			_, changed, err := repository.Transition(ctx, command)
			errorsChannel <- err
			changedChannel <- changed
		}(command)
	}
	wait.Wait()
	close(errorsChannel)
	close(changedChannel)
	for err := range errorsChannel {
		if err != nil {
			t.Fatalf("Transition(): %v", err)
		}
	}
	changedCount := 0
	for changed := range changedChannel {
		if changed {
			changedCount++
		}
	}
	detail, found, err := repository.Detail(ctx, pair.A.TenantID, opportunity.ID)
	if err != nil || !found || detail.Opportunity.Stage != domain.StageQualifying || len(detail.History) != 2 || changedCount != 1 {
		t.Fatalf("итог = %#v, found=%v, err=%v, changed=%d", detail, found, err, changedCount)
	}
}

func newOpportunity(t *testing.T, generator ids.Generator, tenantID, conversationID string, at time.Time) domain.Opportunity {
	t.Helper()
	id, err := generator.NewID()
	if err != nil {
		t.Fatal(err)
	}
	opportunity, err := domain.NewOpportunity(id, tenantID, conversationID, nil, nil, nil, "RUB", at)
	if err != nil {
		t.Fatal(err)
	}
	return opportunity
}

func insertOpportunityConversation(t *testing.T, pool *pgxpool.Pool, tenantID, locationID string) string {
	t.Helper()
	generator := ids.Generator{}
	connectionID, err := generator.NewID()
	if err != nil {
		t.Fatal(err)
	}
	contactID, err := generator.NewID()
	if err != nil {
		t.Fatal(err)
	}
	conversationID, err := generator.NewID()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(context.Background(), `
		INSERT INTO channel_connections(
			id, tenant_id, location_id, provider, name, status, capabilities,
			verification_secret_hash, created_at, updated_at
		) VALUES ($1, $2, $3, 'TEST', 'Opportunity fixture', 'ACTIVE', '["RECEIVE_MESSAGES"]'::jsonb,
		          repeat('0', 64), $4, $4)`, connectionID, tenantID, locationID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `
		INSERT INTO contacts(id, tenant_id, display_name, created_at, updated_at)
		VALUES ($1, $2, 'Candidate', $3, $3)`, contactID, tenantID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `
		INSERT INTO conversations(
			id, tenant_id, location_id, connection_id, contact_id, external_id,
			status, revision, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, 'ACTIVE', 0, $7, $7)`,
		conversationID, tenantID, locationID, connectionID, contactID, "fixture-"+conversationID, now,
	); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	return conversationID
}
