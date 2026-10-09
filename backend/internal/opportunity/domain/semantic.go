package domain

import (
	"context"
	"time"
)

// SemanticCreation binds creation to the exact applied snapshot. Storage checks
// freshness and incoming intent under the conversation lock before writing.
type SemanticCreation struct {
	TenantID, ConversationID, RunID, ThroughMessageID string
	OpportunityID, HistoryID                          string
	Revision                                          int64
	At                                                time.Time
}

type SemanticCreator interface {
	CreateFromAnalysis(context.Context, SemanticCreation) (Opportunity, bool, error)
}

// ServiceEnrichment only fills an unknown service; it never replaces a known
// service or a reliable amount with a catalogue average.
type ServiceEnrichment struct {
	TenantID, OpportunityID, ServiceID, Currency string
	Amount                                       *PotentialRevenue
	Confidence                                   *Confidence
	At                                           time.Time
}

type ServiceEnricher interface {
	EnrichService(context.Context, ServiceEnrichment) (Opportunity, bool, error)
}
