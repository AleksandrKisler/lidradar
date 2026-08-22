// Package domain defines the durable AI queue state. It has no provider,
// transport, or persistence dependencies.
package domain

import (
	"encoding/json"
	"time"
)

type NodeStatus string

const (
	NodeReady   NodeStatus = "READY"
	NodeOffline NodeStatus = "OFFLINE"
)

type Node struct {
	ID, Name        string
	SecretHash      [32]byte
	Status          NodeStatus
	LastHeartbeatAt time.Time
	CreatedAt       time.Time
}

type JobStatus string

const (
	JobQueued    JobStatus = "QUEUED"
	JobRunning   JobStatus = "RUNNING"
	JobSucceeded JobStatus = "SUCCEEDED"
	JobFailed    JobStatus = "FAILED"
)

type Job struct {
	ID, TenantID, ConversationID, Prompt       string
	BaseConversationRevision                   int64
	AnalysisThroughMessageID                   string
	ModelVersion, PromptVersion, SchemaVersion string
	Status                                     JobStatus
	ClaimedBy                                  string
	LeaseUntil                                 time.Time
	CreatedAt, UpdatedAt                       time.Time
}

type ApplicationStatus string

const (
	ApplicationPending  ApplicationStatus = "PENDING"
	ApplicationApplied  ApplicationStatus = "APPLIED"
	ApplicationStale    ApplicationStatus = "STALE"
	ApplicationRejected ApplicationStatus = "REJECTED"
)

type Run struct {
	ID, JobID, NodeID, TenantID, ConversationID, Output, Error string
	Status                                                     JobStatus
	ApplicationStatus                                          ApplicationStatus
	BaseConversationRevision                                   int64
	AnalysisThroughMessageID                                   string
	ModelVersion, PromptVersion, SchemaVersion                 string
	ValidationError                                            string
	StartedAt, CompletedAt                                     time.Time
}

// ConversationSummary is derived AI data, never authoritative conversation
// state. Source fields make every summary freshness-auditable.
type ConversationSummary struct {
	TenantID, ConversationID, Text         string
	BaseConversationRevision               int64
	AnalysisThroughMessageID, ModelVersion string
	PromptVersion, SchemaVersion, RunID    string
	UpdatedAt                              time.Time
}

type ConfidenceBand string

const (
	ConfidenceStrong    ConfidenceBand = "STRONG"
	ConfidenceWeak      ConfidenceBand = "WEAK"
	ConfidenceUntrusted ConfidenceBand = "UNTRUSTED"
)

type FactType string

const (
	FactBookingIntent      FactType = "BOOKING_INTENT"
	FactBusinessCommitment FactType = "BUSINESS_COMMITMENT"
	FactPriceMentioned     FactType = "PRICE_MENTIONED"
	FactFollowUpCandidate  FactType = "FOLLOW_UP_CANDIDATE"
)

// SemanticFact is an interpretation only. It deliberately contains no Risk
// or Opportunity state transition.
type SemanticFact struct {
	Type               FactType `json:"type"`
	Value              bool     `json:"value"`
	Confidence         float64  `json:"confidence"`
	EvidenceMessageIDs []string `json:"evidenceMessageIds"`
	Amount             *string  `json:"amount,omitempty"`
	Currency           string   `json:"currency,omitempty"`
}

type AnalysisResultV1 struct {
	SchemaVersion            string         `json:"schemaVersion"`
	AnalysisThroughMessageID string         `json:"analysisThroughMessageId"`
	Summary                  string         `json:"summary"`
	Facts                    []SemanticFact `json:"facts"`
}

// RawJSON retains the exact provider result in a run while typed values are
// used by deterministic validation and application policies.
func (r AnalysisResultV1) RawJSON() ([]byte, error) { return json.Marshal(r) }
