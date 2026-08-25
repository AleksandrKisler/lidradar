package application_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"lidradar/backend/internal/risk/application"
	"lidradar/backend/internal/risk/domain"
	"lidradar/backend/internal/risk/infrastructure"
)

type stateReader struct{ state domain.ConversationState }

func (r *stateReader) CurrentState(context.Context, string, string) (domain.ConversationState, error) {
	return r.state, nil
}

func evaluationState() domain.ConversationState {
	weekly := make(map[time.Weekday][]domain.BusinessPeriod)
	for day := time.Sunday; day <= time.Saturday; day++ {
		weekly[day] = []domain.BusinessPeriod{{Open: domain.Clock(0, 0), Close: domain.Clock(24, 0)}}
	}
	return domain.ConversationState{
		TenantID: "tenant", OpportunityID: "opportunity", LocationID: "location", ActiveOpportunity: true,
		LastMeaningfulID: "incoming", LastMeaningfulAt: time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC),
		LastMeaningful: domain.DirectionIncoming, ResponseThreshold: 45 * time.Minute,
		BusinessHours: domain.BusinessHours{Timezone: "UTC", Weekly: weekly},
	}
}

func TestEvaluateDueRereadsStateAndAutoResolves(t *testing.T) {
	repository := infrastructure.NewTestMemoryRepository()
	reader := &stateReader{state: evaluationState()}
	now := time.Date(2026, 8, 21, 10, 45, 0, 0, time.UTC)
	evaluator := application.NewEvaluator(repository, reader, domain.NoResponsePolicy{}, func() string { return "risk-1" }, func() time.Time { return now })

	risk, created, err := evaluator.EvaluateDue(context.Background(), "tenant", "opportunity")
	if err != nil || !created || risk.Status != domain.StatusOpen {
		t.Fatalf("risk = %#v, created = %v, err = %v", risk, created, err)
	}
	reader.state.OutgoingAfterTrigger = true // canonical state changed after scheduling
	now = now.Add(time.Minute)
	_, created, err = evaluator.EvaluateDue(context.Background(), "tenant", "opportunity")
	if err != nil || created {
		t.Fatalf("created = %v, err = %v", created, err)
	}
	if _, active, err := repository.FindActive(context.Background(), "tenant", "opportunity", domain.TypeNoResponse); err != nil || active {
		t.Fatalf("active = %v, err = %v", active, err)
	}
}

func TestEvaluateDueReplayCreatesOneRisk(t *testing.T) {
	repository := infrastructure.NewTestMemoryRepository()
	reader := &stateReader{state: evaluationState()}
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	ids := 0
	evaluator := application.NewEvaluator(repository, reader, domain.NoResponsePolicy{}, func() string {
		mu.Lock()
		defer mu.Unlock()
		ids++
		return "risk-" + string(rune('a'+ids))
	}, func() time.Time { return now })

	const attempts = 10
	created := make(chan bool, attempts)
	var wg sync.WaitGroup
	for range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, wasCreated, err := evaluator.EvaluateDue(context.Background(), "tenant", "opportunity")
			if err != nil {
				t.Errorf("EvaluateDue: %v", err)
			}
			created <- wasCreated
		}()
	}
	wg.Wait()
	close(created)
	count := 0
	for wasCreated := range created {
		if wasCreated {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("created %d risks; want one", count)
	}
}

func TestEvaluateDueRejectsCrossTenantState(t *testing.T) {
	repository := infrastructure.NewTestMemoryRepository()
	reader := &stateReader{state: evaluationState()}
	reader.state.TenantID = "another-tenant"
	evaluator := application.NewEvaluator(repository, reader, domain.NoResponsePolicy{}, func() string { return "risk" }, time.Now)
	if _, _, err := evaluator.EvaluateDue(context.Background(), "tenant", "opportunity"); err != application.ErrInvalidCheck {
		t.Fatalf("err = %v", err)
	}
}
