package application_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	jobsdomain "lidradar/backend/internal/jobs/domain"
	"lidradar/backend/internal/risk/application"
	"lidradar/backend/internal/risk/domain"
)

type capturedChecks struct{ checks []jobsdomain.ScheduledCheck }

func (store *capturedChecks) Schedule(
	_ context.Context,
	check jobsdomain.ScheduledCheck,
) (jobsdomain.ScheduledCheck, bool, error) {
	store.checks = append(store.checks, check)
	return check, true, nil
}

type fixedOpportunityLocator struct{ opportunityID string }

func (locator fixedOpportunityLocator) ActiveOpportunityByConversation(
	context.Context,
	string,
	string,
) (string, bool, error) {
	return locator.opportunityID, locator.opportunityID != "", nil
}

func TestPlannerStoresIdentifiersInsteadOfConversationSnapshot(t *testing.T) {
	state := evaluationState()
	state.LastMeaningfulAt = time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	reader := &stateReader{state: state}
	checks := &capturedChecks{}
	now := state.LastMeaningfulAt.Add(time.Minute)
	planner := application.NewPlanner(
		fixedOpportunityLocator{opportunityID: state.OpportunityID}, reader, checks,
		application.Evaluator{}, domain.NoResponsePolicy{},
		idFunction(func() (string, error) { return "check", nil }), func() time.Time { return now },
	)
	if err := planner.RefreshConversation(context.Background(), state.TenantID, "conversation"); err != nil {
		t.Fatal(err)
	}
	if len(checks.checks) != 1 {
		t.Fatalf("проверок = %d, нужна одна", len(checks.checks))
	}
	check := checks.checks[0]
	if !check.DueAt.Equal(state.LastMeaningfulAt.Add(45*time.Minute)) || check.JobType != application.NoResponseEvaluationJobType {
		t.Fatalf("проверка = %#v", check)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(check.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload) != 1 || string(payload["opportunityId"]) != `"opportunity"` {
		t.Fatalf("полезная нагрузка содержит снимок или неверный ID: %s", check.Payload)
	}
}

func TestDueEvaluationAfterEarlyReplyDoesNotCreateRisk(t *testing.T) {
	repository := newCountingRepository()
	state := evaluationState()
	state.LastMeaningful = domain.DirectionOutgoing
	state.LastMeaningfulID = "early-reply"
	state.LastMeaningfulAt = time.Date(2026, 8, 21, 10, 20, 0, 0, time.UTC)
	reader := &stateReader{state: state}
	evaluator := application.NewEvaluator(
		repository, reader, domain.NoResponsePolicy{},
		idFunction(func() (string, error) { return "unused", nil }),
		func() time.Time { return time.Date(2026, 8, 21, 10, 45, 0, 0, time.UTC) },
	)
	if _, created, err := evaluator.EvaluateDue(context.Background(), "tenant", "opportunity"); err != nil || created {
		t.Fatalf("created=%v, err=%v", created, err)
	}
	if repository.upserts != 0 {
		t.Fatalf("после раннего ответа записей риска = %d", repository.upserts)
	}
}

type countingRepository struct{ upserts int }

func newCountingRepository() *countingRepository { return &countingRepository{} }

func (repository *countingRepository) UpsertActive(
	context.Context,
	domain.Risk,
) (domain.Risk, bool, error) {
	repository.upserts++
	return domain.Risk{}, true, nil
}

func (*countingRepository) FindActive(context.Context, string, string, domain.Type) (domain.Risk, bool, error) {
	return domain.Risk{}, false, nil
}

func (*countingRepository) ResolveActive(context.Context, string, string, domain.Type, time.Time) (bool, error) {
	return false, nil
}
