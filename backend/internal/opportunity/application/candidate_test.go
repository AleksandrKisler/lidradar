package application

import (
	"context"
	"errors"
	"testing"
	"time"

	catalogdomain "lidradar/backend/internal/catalog/domain"
	conversationdomain "lidradar/backend/internal/conversation/domain"
	"lidradar/backend/internal/opportunity/domain"
)

type candidateRepository struct {
	created []domain.Opportunity
}

func (repository *candidateRepository) Create(_ context.Context, opportunity domain.Opportunity, _ domain.StageHistory) (domain.Opportunity, bool, error) {
	repository.created = append(repository.created, opportunity)
	return opportunity, true, nil
}

func (*candidateRepository) Detail(context.Context, string, string) (domain.Detail, bool, error) {
	return domain.Detail{}, false, nil
}

func (*candidateRepository) Transition(context.Context, domain.TransitionCommand) (domain.Opportunity, bool, error) {
	return domain.Opportunity{}, false, nil
}

func (*candidateRepository) ActiveByConversation(context.Context, string, string) (domain.Opportunity, bool, error) {
	return domain.Opportunity{}, false, nil
}

func (*candidateRepository) UpdateEstimate(context.Context, domain.EstimateUpdate) (bool, error) {
	return false, nil
}

type candidateConversations struct {
	snapshot conversationdomain.CandidateSnapshot
}

func (source candidateConversations) CommercialSnapshot(context.Context, string, string) (conversationdomain.CandidateSnapshot, bool, error) {
	return source.snapshot, true, nil
}

type candidateCatalog []catalogdomain.ServiceCatalogItem

func (catalog candidateCatalog) List(context.Context, string) ([]catalogdomain.ServiceCatalogItem, error) {
	return catalog, nil
}

type sequenceIDs struct{ values []string }

func (ids *sequenceIDs) NewID() (string, error) {
	if len(ids.values) == 0 {
		return "", errors.New("идентификаторы закончились")
	}
	value := ids.values[0]
	ids.values = ids.values[1:]
	return value, nil
}

func TestCandidateProcessorCreatesOnlyUnambiguousCommercialOpportunity(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	text := "Здравствуйте! Нужна полировка, когда можно приехать?"
	snapshot := commercialSnapshot(text, now)
	price, _ := catalogdomain.ParsePrice("5000")
	item, err := catalogdomain.NewServiceCatalogItem("service", "tenant", "Полировка", nil, &price, &price, "RUB", now)
	if err != nil {
		t.Fatal(err)
	}
	repository := &candidateRepository{}
	processor := NewCandidateProcessor(
		repository, candidateConversations{snapshot}, candidateCatalog{item},
		&sequenceIDs{values: []string{"opportunity", "history"}}, func() time.Time { return now },
	)
	opportunity, created, err := processor.Evaluate(context.Background(), "tenant", "conversation")
	if err != nil || !created {
		t.Fatalf("Evaluate() = %#v, %v, %v", opportunity, created, err)
	}
	if opportunity.Stage != domain.StageNew || opportunity.ServiceID == nil || *opportunity.ServiceID != item.ID ||
		opportunity.EstimatedAmount == nil || opportunity.EstimatedAmount.String() != "5000.00" ||
		opportunity.EstimatedAmountConfidence == nil || opportunity.EstimatedAmountConfidence.String() != "1.000" {
		t.Fatalf("возможность = %#v", opportunity)
	}
}

func TestCandidateProcessorRejectsNoiseAndAmbiguityWithoutCreatingLead(t *testing.T) {
	now := time.Now().UTC()
	priceFrom, _ := catalogdomain.ParsePrice("1000")
	priceTo, _ := catalogdomain.ParsePrice("2000")
	polishing, _ := catalogdomain.NewServiceCatalogItem("service-1", "tenant", "Полировка", nil, &priceFrom, &priceTo, "RUB", now)
	bodyPolishing, _ := catalogdomain.NewServiceCatalogItem("service-2", "tenant", "Полировка кузова", nil, nil, nil, "RUB", now)

	for _, test := range []struct {
		name  string
		text  string
		items []catalogdomain.ServiceCatalogItem
	}{
		{"обслуживание без коммерческого намерения", "Где найти старый чек?", []catalogdomain.ServiceCatalogItem{polishing}},
		{"SQL-подобный мусор", `' OR 1=1; DROP TABLE opportunities; --`, []catalogdomain.ServiceCatalogItem{polishing}},
		{"пустая строка", "\x00\n\t", []catalogdomain.ServiceCatalogItem{polishing}},
		{"двусмысленные услуги", "Нужна полировка кузова", []catalogdomain.ServiceCatalogItem{polishing, bodyPolishing}},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := &candidateRepository{}
			processor := NewCandidateProcessor(
				repository, candidateConversations{commercialSnapshot(test.text, now)}, candidateCatalog(test.items),
				&sequenceIDs{values: []string{"unused"}}, func() time.Time { return now },
			)
			_, created, err := processor.Evaluate(context.Background(), "tenant", "conversation")
			if err != nil || created || len(repository.created) != 0 {
				t.Fatalf("Evaluate() = created %v, err %v, записи %#v", created, err, repository.created)
			}
		})
	}
}

func TestCandidateProcessorLeavesRangePotentialUnknown(t *testing.T) {
	now := time.Now().UTC()
	from, _ := catalogdomain.ParsePrice("1000")
	to, _ := catalogdomain.ParsePrice("2000")
	item, _ := catalogdomain.NewServiceCatalogItem("service", "tenant", "Диагностика", nil, &from, &to, "RUB", now)
	repository := &candidateRepository{}
	processor := NewCandidateProcessor(
		repository, candidateConversations{commercialSnapshot("Нужна диагностика", now)}, candidateCatalog{item},
		&sequenceIDs{values: []string{"opportunity", "history"}}, func() time.Time { return now },
	)
	opportunity, created, err := processor.Evaluate(context.Background(), "tenant", "conversation")
	if err != nil || !created || opportunity.EstimatedAmount != nil || opportunity.EstimatedAmountConfidence != nil {
		t.Fatalf("Evaluate() = %#v, created %v, err %v", opportunity, created, err)
	}
}

func commercialSnapshot(text string, at time.Time) conversationdomain.CandidateSnapshot {
	direction := conversationdomain.DirectionIncoming
	return conversationdomain.CandidateSnapshot{
		Conversation: conversationdomain.Conversation{
			ID: "conversation", TenantID: "tenant", ConnectionID: "connection", ContactID: "contact",
			ExternalID: "external-conversation", Status: conversationdomain.ConversationActive,
			FirstMessageAt: &at, LastMessageAt: &at, LastMessageDirection: &direction,
			Revision: 1, CreatedAt: at, UpdatedAt: at,
		},
		LatestMessage: conversationdomain.Message{
			ID: "message", TenantID: "tenant", ConversationID: "conversation", ConnectionID: "connection",
			ExternalID: "external-message", Direction: conversationdomain.DirectionIncoming,
			Type: conversationdomain.MessageText, Text: &text, SentAt: at, ReceivedAt: at,
			Metadata: []byte(`{}`), CreatedAt: at,
		},
	}
}
