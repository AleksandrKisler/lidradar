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
	NodeRevoked NodeStatus = "REVOKED"
)

type Node struct {
	ID, Name, TenantID          string
	SecretHash                  [32]byte
	Status                      NodeStatus
	ModelVersion                string
	AvailableSlots, MaxInflight int
	LastHeartbeatAt             time.Time
	RevokedAt                   time.Time
	CreatedAt, UpdatedAt        time.Time
}

type JobStatus string

const (
	JobPending   JobStatus = "PENDING"
	JobLeased    JobStatus = "LEASED"
	JobRunning   JobStatus = "RUNNING"
	JobSucceeded JobStatus = "SUCCEEDED"
	JobRetry     JobStatus = "RETRY"
	JobDead      JobStatus = "DEAD"
)

type Job struct {
	ID, TenantID, JobType, EntityType          string
	ConversationID, Prompt                     string
	BaseConversationRevision                   int64
	AnalysisThroughMessageID                   string
	ModelVersion, PromptVersion, SchemaVersion string
	Priority, Attempts, MaxAttempts            int
	Status                                     JobStatus
	LeasedBy                                   string
	AvailableAt, LeaseUntil                    time.Time
	// LeasedAt фиксирует момент захвата и не продлевается heartbeat: вместе с
	// абсолютным потолком аренды он возвращает зависшее задание в очередь.
	LeasedAt             time.Time
	LastErrorCode        string
	CompletedAt          time.Time
	CreatedAt, UpdatedAt time.Time
}

type ApplicationStatus string

const (
	ApplicationPending  ApplicationStatus = "PENDING"
	ApplicationApplied  ApplicationStatus = "APPLIED"
	ApplicationStale    ApplicationStatus = "STALE"
	ApplicationRejected ApplicationStatus = "REJECTED"
)

type RunStatus string

const (
	RunRunning   RunStatus = "RUNNING"
	RunSucceeded RunStatus = "SUCCEEDED"
	RunFailed    RunStatus = "FAILED"
)

type Run struct {
	ID, JobID, NodeID, TenantID, ConversationID, Output string
	ErrorCode                                           string
	Status                                              RunStatus
	ApplicationStatus                                   ApplicationStatus
	BaseConversationRevision                            int64
	AnalysisThroughMessageID                            string
	ModelVersion, PromptVersion, SchemaVersion          string
	ValidationError                                     string
	StartedAt, CompletedAt                              time.Time
}

// ConversationSnapshot — авторитетное состояние переписки на момент проверки
// свежести результата. Его читает Cloud Core, а не присылает домашний узел.
type ConversationSnapshot struct {
	Revision      int64
	LastMessageID string
}

// ConversationSummary is derived AI data, never authoritative conversation
// state. Source fields make every summary freshness-auditable.
type ConversationSummary struct {
	TenantID, ConversationID, Text         string
	BaseConversationRevision               int64
	AnalysisThroughMessageID, ModelVersion string
	PromptVersion, SchemaVersion, RunID    string
	Facts                                  []AppliedFact
	Agreements                             []Agreement
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
	FactPurchaseIntent     FactType = "PURCHASE_INTENT"
)

type AgreementKind string

const (
	AgreementBookingConfirmation AgreementKind = "BOOKING_CONFIRMATION"
	AgreementReschedule          AgreementKind = "RESCHEDULE"
	AgreementCommitment          AgreementKind = "COMMITMENT"
	AgreementPurchaseBlocker     AgreementKind = "PURCHASE_BLOCKER"
)

type AgreementWaitingFor string

const (
	AgreementCustomer AgreementWaitingFor = "CUSTOMER"
	AgreementBusiness AgreementWaitingFor = "BUSINESS"
)

type AgreementStatus string

const (
	AgreementPending   AgreementStatus = "PENDING"
	AgreementResolved  AgreementStatus = "RESOLVED"
	AgreementCancelled AgreementStatus = "CANCELLED"
)

// Agreement is a derived observation, never a risk or an authoritative booking.
// Trusted is assigned only by Cloud Core after validation.
type Agreement struct {
	Kind               AgreementKind       `json:"kind"`
	WaitingFor         AgreementWaitingFor `json:"waitingFor"`
	Status             AgreementStatus     `json:"status"`
	TriggerMessageID   string              `json:"triggerMessageId"`
	EvidenceMessageIDs []string            `json:"evidenceMessageIds"`
	Confidence         float64             `json:"confidence"`
	Trusted            bool                `json:"trusted"`
}

// AgreementObservation is the model-facing shape without server authority.
type AgreementObservation struct {
	Kind               AgreementKind       `json:"kind"`
	WaitingFor         AgreementWaitingFor `json:"waitingFor"`
	Status             AgreementStatus     `json:"status"`
	TriggerMessageID   string              `json:"triggerMessageId"`
	EvidenceMessageIDs []string            `json:"evidenceMessageIds"`
	Confidence         float64             `json:"confidence"`
}

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

// AppliedFact — факт, сохранённый в производной проекции. Trusted вычисляет
// Cloud Core по уровням уверенности §59; модель это поле прислать не может,
// потому что результат разбирается в SemanticFact без него. Ненадёжный факт
// (trusted = false) остаётся для наблюдения и метрик, но не меняет домен.
type AppliedFact struct {
	SemanticFact
	Trusted bool `json:"trusted"`
}

type AnalysisResultV1 struct {
	SchemaVersion            string         `json:"schemaVersion"`
	AnalysisThroughMessageID string         `json:"analysisThroughMessageId"`
	Summary                  string         `json:"summary"`
	Facts                    []SemanticFact `json:"facts"`
}

type AnalysisResultV2 struct {
	SchemaVersion            string                 `json:"schemaVersion"`
	AnalysisThroughMessageID string                 `json:"analysisThroughMessageId"`
	Summary                  string                 `json:"summary"`
	Facts                    []SemanticFact         `json:"facts"`
	Agreements               []AgreementObservation `json:"agreements"`
}

// RawJSON retains the exact provider result in a run while typed values are
// used by deterministic validation and application policies.
func (r AnalysisResultV1) RawJSON() ([]byte, error) { return json.Marshal(r) }
