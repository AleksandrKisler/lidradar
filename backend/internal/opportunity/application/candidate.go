package application

import (
	"context"
	"strings"
	"time"

	catalogdomain "lidradar/backend/internal/catalog/domain"
	conversationdomain "lidradar/backend/internal/conversation/domain"
	"lidradar/backend/internal/opportunity/domain"
)

// ConversationSource — явный межмодульный контракт чтения актуального среза.
type ConversationSource interface {
	CommercialSnapshot(context.Context, string, string) (conversationdomain.CandidateSnapshot, bool, error)
}

// CatalogSource даёт кандидату доверенный каталог выбранной организации.
type CatalogSource interface {
	List(context.Context, string) ([]catalogdomain.ServiceCatalogItem, error)
}

type CandidateProcessor struct {
	repository    domain.Repository
	conversations ConversationSource
	catalog       CatalogSource
	ids           IDs
	now           func() time.Time
}

func NewCandidateProcessor(
	repository domain.Repository,
	conversations ConversationSource,
	catalog CatalogSource,
	ids IDs,
	now func() time.Time,
) CandidateProcessor {
	return CandidateProcessor{
		repository: repository, conversations: conversations, catalog: catalog, ids: ids, now: now,
	}
}

// Evaluate применяет консервативное правило: новое коммерческое намерение
// подтверждается одним недвусмысленным совпадением активной услуги во входящем
// текстовом сообщении с учётом регулярных падежных форм названия.
// Неуверенный случай остаётся без Opportunity.
func (processor CandidateProcessor) Evaluate(
	ctx context.Context,
	tenantID, conversationID string,
) (domain.Opportunity, bool, error) {
	if processor.repository == nil || processor.conversations == nil || processor.catalog == nil ||
		processor.ids == nil || processor.now == nil || tenantID == "" || strings.TrimSpace(conversationID) == "" {
		return domain.Opportunity{}, false, ErrInvalid
	}
	snapshot, found, err := processor.conversations.CommercialSnapshot(ctx, tenantID, conversationID)
	if err != nil {
		return domain.Opportunity{}, false, err
	}
	if !found {
		return domain.Opportunity{}, false, ErrNotFound
	}
	if snapshot.Conversation.TenantID != tenantID || snapshot.Conversation.ID != conversationID ||
		snapshot.Conversation.Status != conversationdomain.ConversationActive {
		return domain.Opportunity{}, false, ErrInvalid
	}
	message := snapshot.LatestMessage
	if message.Direction != conversationdomain.DirectionIncoming || message.Type != conversationdomain.MessageText ||
		message.Text == nil || message.ProviderDeletedAt != nil {
		return domain.Opportunity{}, false, nil
	}
	items, err := processor.catalog.List(ctx, tenantID)
	if err != nil {
		return domain.Opportunity{}, false, err
	}
	matched := matchingServices(*message.Text, snapshot.Conversation.LocationID, items)
	if len(matched) != 1 {
		return domain.Opportunity{}, false, nil
	}
	item := matched[0]
	var amount *domain.PotentialRevenue
	var confidence *domain.Confidence
	if item.PriceFrom != nil && item.PriceTo != nil && item.PriceFrom.Decimal().Equal(item.PriceTo.Decimal()) {
		parsedAmount, parseErr := domain.ParsePotentialRevenue(item.PriceFrom.String())
		if parseErr != nil {
			return domain.Opportunity{}, false, ErrInvalid
		}
		certain, parseErr := domain.ParseConfidence("1")
		if parseErr != nil {
			return domain.Opportunity{}, false, ErrInvalid
		}
		amount, confidence = &parsedAmount, &certain
	}
	opportunityID, err := processor.ids.NewID()
	if err != nil {
		return domain.Opportunity{}, false, err
	}
	historyID, err := processor.ids.NewID()
	if err != nil {
		return domain.Opportunity{}, false, err
	}
	now := processor.now().UTC()
	serviceID := item.ID
	opportunity, err := domain.NewOpportunity(
		opportunityID, tenantID, conversationID, &serviceID, amount, confidence, item.Currency, now,
	)
	if err != nil {
		return domain.Opportunity{}, false, ErrInvalid
	}
	history, err := domain.NewHistory(
		historyID, tenantID, opportunity.ID, nil, domain.StageNew, domain.SourceRule, nil, nil, nil, now,
	)
	if err != nil {
		return domain.Opportunity{}, false, ErrInvalid
	}
	created, wasCreated, err := processor.repository.Create(ctx, opportunity, history)
	if err != nil {
		return domain.Opportunity{}, false, mapDomainError(err)
	}
	if !wasCreated && created.ServiceID == nil {
		if enricher, ok := processor.repository.(domain.ServiceEnricher); ok {
			enriched, _, enrichErr := enricher.EnrichService(ctx, domain.ServiceEnrichment{
				TenantID: tenantID, OpportunityID: created.ID, ServiceID: serviceID,
				Currency: item.Currency, Amount: amount, Confidence: confidence, At: now,
			})
			if enrichErr != nil {
				return domain.Opportunity{}, false, mapDomainError(enrichErr)
			}
			return enriched, false, nil
		}
	}
	return created, wasCreated, nil
}
