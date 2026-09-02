package application_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	eventsdomain "lidradar/backend/internal/events/domain"
	"lidradar/backend/internal/opportunity/application"
	"lidradar/backend/internal/opportunity/domain"
)

type semanticRepository struct {
	opportunity domain.Opportunity
	found       bool
	commands    []domain.TransitionCommand
}

func (repository *semanticRepository) Create(context.Context, domain.Opportunity, domain.StageHistory) (domain.Opportunity, bool, error) {
	return domain.Opportunity{}, false, errors.New("не используется")
}
func (repository *semanticRepository) Detail(context.Context, string, string) (domain.Detail, bool, error) {
	return domain.Detail{}, false, nil
}
func (repository *semanticRepository) ActiveByConversation(context.Context, string, string) (domain.Opportunity, bool, error) {
	return repository.opportunity, repository.found, nil
}
func (repository *semanticRepository) Transition(_ context.Context, command domain.TransitionCommand) (domain.Opportunity, bool, error) {
	repository.commands = append(repository.commands, command)
	repository.opportunity.Stage = command.ToStage
	return repository.opportunity, true, nil
}

type semanticFacts struct {
	fact  application.BookingIntentFact
	found bool
}

func (facts semanticFacts) TrustedBookingIntent(context.Context, string, string, string) (application.BookingIntentFact, bool, error) {
	return facts.fact, facts.found, nil
}

type semanticIDs struct{}

func (semanticIDs) NewID() (string, error) { return "history-1", nil }

func appliedEvent(t *testing.T) eventsdomain.Event {
	t.Helper()
	data, _ := json.Marshal(map[string]any{
		"conversationId": "conversation", "runId": "run-1",
		"baseConversationRevision": 4, "analysisThroughMessageId": "message-4",
	})
	event, err := eventsdomain.NewEvent(
		"event", "ai.analysis.applied", 1, "tenant", "ai_run", "run-1", "run-1", data,
		time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func semanticOpportunity(t *testing.T, stage domain.Stage) domain.Opportunity {
	t.Helper()
	at := time.Date(2026, 9, 2, 11, 0, 0, 0, time.UTC)
	opportunity, err := domain.NewOpportunity("opportunity", "tenant", "conversation", nil, nil, nil, "RUB", at)
	if err != nil {
		t.Fatal(err)
	}
	opportunity.Stage = stage
	if !stage.Active() {
		opportunity.ClosedAt = &at
	}
	return opportunity
}

// LR-BE-RM-013: сильный факт переводит новую сделку в BOOKING_INTENT с
// источником AI, уверенностью и ссылкой на run; повтор события идемпотентен.
func TestAnalysisAppliedMovesNewOpportunityToBookingIntent(t *testing.T) {
	repository := &semanticRepository{opportunity: semanticOpportunity(t, domain.StageNew), found: true}
	handler := application.AnalysisAppliedEventHandler(
		repository, semanticFacts{fact: application.BookingIntentFact{RunID: "run-1", Confidence: "0.920"}, found: true},
		semanticIDs{}, func() time.Time { return time.Date(2026, 9, 2, 12, 0, 1, 0, time.UTC) },
	)
	event := appliedEvent(t)
	if err := handler(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if err := handler(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if len(repository.commands) != 1 {
		t.Fatalf("переходов %d, ожидался один", len(repository.commands))
	}
	command := repository.commands[0]
	if command.ToStage != domain.StageBookingIntent || command.Source != domain.SourceAI ||
		command.Confidence == nil || command.Confidence.String() != "0.920" ||
		command.AIRunID == nil || *command.AIRunID != "run-1" || command.ActorUserID != nil ||
		command.HistoryID != "history-1" || command.OpportunityID != "opportunity" {
		t.Fatalf("переход = %#v", command)
	}
}

// Сделка на подтверждённой записи или без факта остаётся на месте.
func TestAnalysisAppliedLeavesOpportunityWithoutStrongFactOrPastBooking(t *testing.T) {
	for name, test := range map[string]struct {
		stage domain.Stage
		found bool
	}{
		"без сильного факта":     {stage: domain.StageNew, found: false},
		"запись подтверждена":    {stage: domain.StageBooked, found: true},
		"сделка закрыта":         {stage: domain.StageLost, found: true},
		"уже намерение записи":   {stage: domain.StageBookingIntent, found: true},
		"ожидает ответа бизнеса": {stage: domain.StageWaitingBusiness, found: false},
	} {
		t.Run(name, func(t *testing.T) {
			repository := &semanticRepository{opportunity: semanticOpportunity(t, test.stage), found: true}
			handler := application.AnalysisAppliedEventHandler(
				repository, semanticFacts{fact: application.BookingIntentFact{RunID: "run-1", Confidence: "0.950"}, found: test.found},
				semanticIDs{}, time.Now,
			)
			if err := handler(context.Background(), appliedEvent(t)); err != nil {
				t.Fatal(err)
			}
			if len(repository.commands) != 0 {
				t.Fatalf("неожиданный переход: %#v", repository.commands)
			}
		})
	}
}
