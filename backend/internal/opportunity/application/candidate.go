package application

import (
	"context"
	"strings"
	"time"
	"unicode"

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

// Evaluate применяет намеренно точное правило: новое коммерческое намерение
// подтверждается одним недвусмысленным совпадением активной услуги во входящем
// текстовом сообщении. Неуверенный случай остаётся без Opportunity.
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
	return created, wasCreated, nil
}

func matchingServices(
	text string,
	locationID *string,
	items []catalogdomain.ServiceCatalogItem,
) []catalogdomain.ServiceCatalogItem {
	normalizedText := normalizeWords(text)
	if normalizedText == "" {
		return nil
	}
	matches := make([]catalogdomain.ServiceCatalogItem, 0, 1)
	for _, item := range items {
		if !item.Active || !locationApplies(locationID, item.LocationID) {
			continue
		}
		name := normalizeWords(item.NormalizedName)
		if name != "" && strings.Contains(" "+normalizedText+" ", " "+name+" ") {
			matches = append(matches, item)
		}
	}
	return matches
}

func locationApplies(conversationLocation, serviceLocation *string) bool {
	return serviceLocation == nil || (conversationLocation != nil && *conversationLocation == *serviceLocation)
}

func normalizeWords(value string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(value), func(character rune) bool {
		return !unicode.IsLetter(character) && !unicode.IsDigit(character)
	}), " ")
}
