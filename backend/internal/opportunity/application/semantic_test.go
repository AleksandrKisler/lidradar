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
	estimates   []domain.EstimateUpdate
}

func (repository *semanticRepository) UpdateEstimate(_ context.Context, update domain.EstimateUpdate) (bool, error) {
	repository.estimates = append(repository.estimates, update)
	return true, nil
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
	fact          application.BookingIntentFact
	found         bool
	price         application.PriceFact
	priceFound    bool
	followUp      application.BookingIntentFact
	followUpFound bool
}

func (facts semanticFacts) TrustedBookingIntent(context.Context, string, string, string) (application.BookingIntentFact, bool, error) {
	return facts.fact, facts.found, nil
}

func (facts semanticFacts) TrustedPriceMentioned(context.Context, string, string, string) (application.PriceFact, bool, error) {
	return facts.price, facts.priceFound, nil
}

func (facts semanticFacts) TrustedFollowUpCandidate(context.Context, string, string, string) (application.BookingIntentFact, bool, error) {
	return facts.followUp, facts.followUpFound, nil
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

// LR-BE-1802/1807: названная цена переводит сделку в PRICE_SENT и записывает
// оценку выручки только в валюте сделки и только с разбираемой суммой.
func TestAnalysisAppliedPriceMovesToPriceSentAndGuardsEstimate(t *testing.T) {
	for name, test := range map[string]struct {
		price         application.PriceFact
		wantEstimates int
	}{
		"цена в валюте сделки":          {price: application.PriceFact{RunID: "run-1", Confidence: "0.960", Amount: "5200", Currency: "RUB"}, wantEstimates: 1},
		"другая валюта не выдумывается": {price: application.PriceFact{RunID: "run-1", Confidence: "0.960", Amount: "50", Currency: "EUR"}, wantEstimates: 0},
		"слишком точная сумма":          {price: application.PriceFact{RunID: "run-1", Confidence: "0.960", Amount: "5200.123", Currency: "RUB"}, wantEstimates: 0},
	} {
		t.Run(name, func(t *testing.T) {
			repository := &semanticRepository{opportunity: semanticOpportunity(t, domain.StageNew), found: true}
			handler := application.AnalysisAppliedEventHandler(
				repository, semanticFacts{price: test.price, priceFound: true}, semanticIDs{},
				func() time.Time { return time.Date(2026, 9, 2, 12, 0, 1, 0, time.UTC) },
			)
			if err := handler(context.Background(), appliedEvent(t)); err != nil {
				t.Fatal(err)
			}
			if len(repository.commands) != 1 || repository.commands[0].ToStage != domain.StagePriceSent ||
				repository.commands[0].Source != domain.SourceAI {
				t.Fatalf("переходы = %#v", repository.commands)
			}
			if len(repository.estimates) != test.wantEstimates {
				t.Fatalf("оценок выручки %d, ожидалось %d: %#v", len(repository.estimates), test.wantEstimates, repository.estimates)
			}
			if test.wantEstimates == 1 {
				estimate := repository.estimates[0]
				if estimate.Amount.String() != "5200.00" || estimate.Confidence.String() != "0.960" ||
					estimate.Currency != "RUB" || estimate.OpportunityID != "opportunity" {
					t.Fatalf("оценка = %#v", estimate)
				}
			}
		})
	}
}

// Цена и намерение записаться в одном анализе проходят оба этапа по порядку.
func TestAnalysisAppliedPriceAndBookingKeepStageOrder(t *testing.T) {
	repository := &semanticRepository{opportunity: semanticOpportunity(t, domain.StageNew), found: true}
	handler := application.AnalysisAppliedEventHandler(
		repository, semanticFacts{
			price: application.PriceFact{RunID: "run-1", Confidence: "0.900", Amount: "5200", Currency: "RUB"}, priceFound: true,
			fact: application.BookingIntentFact{RunID: "run-1", Confidence: "0.950"}, found: true,
		},
		semanticIDs{}, time.Now,
	)
	if err := handler(context.Background(), appliedEvent(t)); err != nil {
		t.Fatal(err)
	}
	if len(repository.commands) != 2 || repository.commands[0].ToStage != domain.StagePriceSent ||
		repository.commands[1].ToStage != domain.StageBookingIntent {
		t.Fatalf("переходы = %#v", repository.commands)
	}
}

// LR-BE-1903: колебание переводит сделку в WAITING_CUSTOMER; в одном анализе
// с ценой и намерением записаться этапы проходят по порядку машины.
func TestAnalysisAppliedFollowUpMovesToWaitingCustomerInOrder(t *testing.T) {
	repository := &semanticRepository{opportunity: semanticOpportunity(t, domain.StageNew), found: true}
	handler := application.AnalysisAppliedEventHandler(
		repository, semanticFacts{followUp: application.BookingIntentFact{RunID: "run-1", Confidence: "0.920"}, followUpFound: true},
		semanticIDs{}, time.Now,
	)
	if err := handler(context.Background(), appliedEvent(t)); err != nil {
		t.Fatal(err)
	}
	if len(repository.commands) != 1 || repository.commands[0].ToStage != domain.StageWaitingCustomer ||
		repository.commands[0].Source != domain.SourceAI || repository.commands[0].Confidence.String() != "0.920" {
		t.Fatalf("переходы = %#v", repository.commands)
	}

	ordered := &semanticRepository{opportunity: semanticOpportunity(t, domain.StageNew), found: true}
	handler = application.AnalysisAppliedEventHandler(
		ordered, semanticFacts{
			price: application.PriceFact{RunID: "run-1", Confidence: "0.900", Amount: "5200", Currency: "RUB"}, priceFound: true,
			followUp: application.BookingIntentFact{RunID: "run-1", Confidence: "0.910"}, followUpFound: true,
			fact: application.BookingIntentFact{RunID: "run-1", Confidence: "0.950"}, found: true,
		},
		semanticIDs{}, time.Now,
	)
	if err := handler(context.Background(), appliedEvent(t)); err != nil {
		t.Fatal(err)
	}
	if len(ordered.commands) != 3 || ordered.commands[0].ToStage != domain.StagePriceSent ||
		ordered.commands[1].ToStage != domain.StageWaitingCustomer || ordered.commands[2].ToStage != domain.StageBookingIntent {
		t.Fatalf("переходы = %#v", ordered.commands)
	}
	// Сделка уже дальше WAITING_CUSTOMER — колебание её не откатывает.
	later := &semanticRepository{opportunity: semanticOpportunity(t, domain.StageBookingIntent), found: true}
	handler = application.AnalysisAppliedEventHandler(
		later, semanticFacts{followUp: application.BookingIntentFact{RunID: "run-1", Confidence: "0.920"}, followUpFound: true},
		semanticIDs{}, time.Now,
	)
	if err := handler(context.Background(), appliedEvent(t)); err != nil {
		t.Fatal(err)
	}
	if len(later.commands) != 0 {
		t.Fatalf("откат этапа: %#v", later.commands)
	}
}
