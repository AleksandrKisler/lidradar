// Package domain описывает платформенное администрирование: право
// PLATFORM_ADMIN, журнал административных команд и модели чтения для
// диагностики контура без прямого доступа к базе (ТЗ, этап 23).
package domain

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
)

var ErrInvalid = errors.New("некорректный запрос администрирования")

var namePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,99}$`)

// Admin — выдача права PLATFORM_ADMIN пользователю. Отзыв не удаляет строку.
type Admin struct {
	ID          string     `json:"id"`
	UserID      string     `json:"userId"`
	Email       string     `json:"email,omitempty"`
	DisplayName string     `json:"displayName,omitempty"`
	GrantedBy   *string    `json:"grantedBy"`
	GrantedAt   time.Time  `json:"grantedAt"`
	RevokedBy   *string    `json:"revokedBy,omitempty"`
	RevokedAt   *time.Time `json:"revokedAt,omitempty"`
	Note        string     `json:"note"`
}

func NewAdmin(id, userID string, grantedBy *string, note string, at time.Time) (Admin, error) {
	admin := Admin{ID: id, UserID: userID, GrantedBy: grantedBy, GrantedAt: at.UTC(), Note: strings.TrimSpace(note)}
	if admin.Validate() != nil {
		return Admin{}, ErrInvalid
	}
	return admin, nil
}

func (admin Admin) Validate() error {
	if admin.ID == "" || admin.UserID == "" || admin.GrantedAt.IsZero() || len(admin.Note) > 500 ||
		admin.Note != strings.TrimSpace(admin.Note) || (admin.GrantedBy != nil && *admin.GrantedBy == "") ||
		(admin.RevokedBy != nil && admin.RevokedAt == nil) || (admin.RevokedAt != nil && admin.RevokedAt.Before(admin.GrantedAt)) {
		return ErrInvalid
	}
	return nil
}

func (admin Admin) Active() bool { return admin.RevokedAt == nil }

type AuditSource string

const (
	SourceAPI AuditSource = "API"
	SourceCLI AuditSource = "CLI"
)

// AuditEntry — запись журнала административных команд (§65). Команда из CLI
// не имеет актора-пользователя.
type AuditEntry struct {
	ID          string
	ActorUserID *string
	Source      AuditSource
	Operation   string
	EntityType  string
	EntityID    string
	TenantID    *string
	Details     map[string]any
	At          time.Time
}

func (entry AuditEntry) Validate() error {
	if entry.ID == "" || entry.EntityID == "" || entry.At.IsZero() || !namePattern.MatchString(entry.Operation) ||
		!namePattern.MatchString(entry.EntityType) || (entry.TenantID != nil && *entry.TenantID == "") {
		return ErrInvalid
	}
	switch entry.Source {
	case SourceAPI:
		if entry.ActorUserID == nil || *entry.ActorUserID == "" {
			return ErrInvalid
		}
	case SourceCLI:
		if entry.ActorUserID != nil {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

// OrganizationSummary — организация с ключевыми счётчиками (LR-BE-2302).
type OrganizationSummary struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Timezone        string    `json:"timezone"`
	Currency        string    `json:"currency"`
	Status          string    `json:"status"`
	CreatedAt       time.Time `json:"createdAt"`
	Members         int       `json:"members"`
	Locations       int       `json:"locations"`
	Connections     int       `json:"connections"`
	OpenRisks       int       `json:"openRisks"`
	MessagesLast24h int       `json:"messagesLast24h"`
}

// ConnectionHealth — состояние канала с очередью сырых событий (LR-BE-2303).
type ConnectionHealth struct {
	ID               string     `json:"id"`
	TenantID         string     `json:"tenantId"`
	TenantName       string     `json:"tenantName"`
	Provider         string     `json:"provider"`
	Name             string     `json:"name"`
	Status           string     `json:"status"`
	LocationID       *string    `json:"locationId"`
	LastEventAt      *time.Time `json:"lastEventAt"`
	LastSuccessAt    *time.Time `json:"lastSuccessAt"`
	LastErrorAt      *time.Time `json:"lastErrorAt"`
	LastErrorCode    *string    `json:"lastErrorCode"`
	RawEventsPending int        `json:"rawEventsPending"`
	RawEventsFailed  int        `json:"rawEventsFailed"`
}

type LifecycleCounts struct {
	Pending       int `json:"pending"`
	Processing    int `json:"processing"`
	Retry         int `json:"retry"`
	Dead          int `json:"dead"`
	ExpiredLeases int `json:"expiredLeases"`
}

type AILifecycleCounts struct {
	Pending    int `json:"pending"`
	Leased     int `json:"leased"`
	Running    int `json:"running"`
	Retry      int `json:"retry"`
	Dead       int `json:"dead"`
	NodesReady int `json:"nodesReady"`
}

type DeliveryCounts struct {
	Pending    int `json:"pending"`
	Processing int `json:"processing"`
	Retry      int `json:"retry"`
	Dead       int `json:"dead"`
}

// QueueStats — панель очередей (LR-BE-2304): §67 jobs_pending, jobs_dead и
// смежные счётчики outbox, AI и доставок одним снимком.
type QueueStats struct {
	CheckedAt        time.Time         `json:"checkedAt"`
	Jobs             LifecycleCounts   `json:"jobs"`
	Outbox           LifecycleCounts   `json:"outbox"`
	AIJobs           AILifecycleCounts `json:"aiJobs"`
	Deliveries       DeliveryCounts    `json:"deliveries"`
	ScheduledOverdue int               `json:"scheduledOverdue"`
	DeadUnhandled    int               `json:"deadUnhandled"`
}

type JobRecord struct {
	ID            string          `json:"id"`
	TenantID      string          `json:"tenantId"`
	Type          string          `json:"type"`
	DedupKey      string          `json:"dedupKey"`
	Status        string          `json:"status"`
	Priority      int             `json:"priority"`
	AvailableAt   time.Time       `json:"availableAt"`
	AttemptCount  int             `json:"attemptCount"`
	MaxAttempts   int             `json:"maxAttempts"`
	LeasedBy      *string         `json:"leasedBy"`
	LeaseUntil    *time.Time      `json:"leaseUntil"`
	LastErrorCode *string         `json:"lastErrorCode"`
	CompletedAt   *time.Time      `json:"completedAt"`
	DiscardedAt   *time.Time      `json:"discardedAt"`
	CreatedAt     time.Time       `json:"createdAt"`
	UpdatedAt     time.Time       `json:"updatedAt"`
	Payload       json.RawMessage `json:"payload"`
}

type OutboxRecord struct {
	ID            string     `json:"id"`
	TenantID      string     `json:"tenantId"`
	EventType     string     `json:"eventType"`
	AggregateType string     `json:"aggregateType"`
	AggregateID   string     `json:"aggregateId"`
	Status        string     `json:"status"`
	AttemptCount  int        `json:"attemptCount"`
	MaxAttempts   int        `json:"maxAttempts"`
	LastErrorCode *string    `json:"lastErrorCode"`
	OccurredAt    time.Time  `json:"occurredAt"`
	CompletedAt   *time.Time `json:"completedAt"`
	DiscardedAt   *time.Time `json:"discardedAt"`
}

type AIJobRecord struct {
	ID                       string     `json:"id"`
	TenantID                 string     `json:"tenantId"`
	ConversationID           string     `json:"conversationId"`
	AnalysisThroughMessageID string     `json:"analysisThroughMessageId"`
	Status                   string     `json:"status"`
	ModelRequirement         string     `json:"modelRequirement"`
	Attempts                 int        `json:"attempts"`
	MaxAttempts              int        `json:"maxAttempts"`
	LastErrorCode            *string    `json:"lastErrorCode"`
	LeasedBy                 *string    `json:"leasedBy"`
	LeasedAt                 *time.Time `json:"leasedAt"`
	LeaseUntil               *time.Time `json:"leaseUntil"`
	CompletedAt              *time.Time `json:"completedAt"`
	DiscardedAt              *time.Time `json:"discardedAt"`
	CreatedAt                time.Time  `json:"createdAt"`
}

type DeliveryRecord struct {
	ID             string     `json:"id"`
	TenantID       string     `json:"tenantId"`
	NotificationID string     `json:"notificationId"`
	Kind           string     `json:"kind"`
	Channel        string     `json:"channel"`
	Status         string     `json:"status"`
	Attempt        int        `json:"attempt"`
	FailureCode    *string    `json:"failureCode"`
	AttemptedAt    *time.Time `json:"attemptedAt"`
	DiscardedAt    *time.Time `json:"discardedAt"`
	CreatedAt      time.Time  `json:"createdAt"`
}

// DeadLetters — все мёртвые элементы, которые ещё никто не отложил (LR-BE-2305).
type DeadLetters struct {
	Jobs       []JobRecord      `json:"jobs"`
	Outbox     []OutboxRecord   `json:"outbox"`
	AIJobs     []AIJobRecord    `json:"aiJobs"`
	Deliveries []DeliveryRecord `json:"deliveries"`
}

type AINode struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	Status          string     `json:"status"`
	ModelVersion    *string    `json:"modelVersion"`
	AvailableSlots  int        `json:"availableSlots"`
	Inflight        int        `json:"inflight"`
	LastHeartbeatAt *time.Time `json:"lastHeartbeatAt"`
	RevokedAt       *time.Time `json:"revokedAt"`
	Tenants         []string   `json:"tenants"`
	CreatedAt       time.Time  `json:"createdAt"`
}

// AIRun не раскрывает сырой вывод модели и промпт (§64): только статусы,
// версии, коды ошибок и длительность.
type AIRun struct {
	ID                string     `json:"id"`
	TenantID          string     `json:"tenantId"`
	JobID             string     `json:"jobId"`
	NodeID            string     `json:"nodeId"`
	ConversationID    string     `json:"conversationId"`
	Status            string     `json:"status"`
	ApplicationStatus string     `json:"applicationStatus"`
	ModelVersion      string     `json:"modelVersion"`
	PromptVersion     string     `json:"promptVersion"`
	SchemaVersion     string     `json:"schemaVersion"`
	ErrorCode         *string    `json:"errorCode"`
	ValidationError   *string    `json:"validationError"`
	StartedAt         time.Time  `json:"startedAt"`
	CompletedAt       *time.Time `json:"completedAt"`
	DurationMs        *int64     `json:"durationMs"`
}

type SemanticFact struct {
	Type               string          `json:"type"`
	Value              json.RawMessage `json:"value"`
	Confidence         float64         `json:"confidence"`
	Trusted            bool            `json:"trusted"`
	EvidenceMessageIDs []string        `json:"evidenceMessageIds"`
}

// ConversationSummary — семантический результат для admin-диагностики
// (LR-BE-RM-017): слабые факты видны, текст резюме не раскрывается.
type ConversationSummary struct {
	TenantID                 string         `json:"tenantId"`
	ConversationID           string         `json:"conversationId"`
	Revision                 int64          `json:"revision"`
	AnalysisThroughMessageID string         `json:"analysisThroughMessageId"`
	ModelVersion             string         `json:"modelVersion"`
	PromptVersion            string         `json:"promptVersion"`
	SchemaVersion            string         `json:"schemaVersion"`
	AIRunID                  string         `json:"aiRunId"`
	UpdatedAt                time.Time      `json:"updatedAt"`
	Facts                    []SemanticFact `json:"facts"`
	TrustedFacts             int            `json:"trustedFacts"`
	WeakFacts                int            `json:"weakFacts"`
}

// TenantUsage — потребление организации за окно (LR-BE-2308, §68): основа
// для стоимости AI на организацию.
type TenantUsage struct {
	TenantID        string  `json:"tenantId"`
	Name            string  `json:"name"`
	Messages        int     `json:"messages"`
	RawEvents       int     `json:"rawEvents"`
	Jobs            int     `json:"jobs"`
	AIJobs          int     `json:"aiJobs"`
	AIRuns          int     `json:"aiRuns"`
	AIRunsApplied   int     `json:"aiRunsApplied"`
	AIRunsRejected  int     `json:"aiRunsRejected"`
	AIRunsStale     int     `json:"aiRunsStale"`
	AIRunSeconds    float64 `json:"aiRunSeconds"`
	Risks           int     `json:"risks"`
	Notifications   int     `json:"notifications"`
	Deliveries      int     `json:"deliveries"`
	RevenueConfirmd int     `json:"-"`
}

type UsageReport struct {
	From    time.Time     `json:"from"`
	To      time.Time     `json:"to"`
	Tenants []TenantUsage `json:"tenants"`
}

type TraceMessage struct {
	ID             string    `json:"id"`
	TenantID       string    `json:"tenantId"`
	ConversationID string    `json:"conversationId"`
	ConnectionID   string    `json:"connectionId"`
	Direction      string    `json:"direction"`
	Type           string    `json:"type"`
	ExternalID     string    `json:"externalId"`
	SentAt         time.Time `json:"sentAt"`
	ReceivedAt     time.Time `json:"receivedAt"`
}

type TraceRisk struct {
	ID            string     `json:"id"`
	OpportunityID string     `json:"opportunityId"`
	Type          string     `json:"type"`
	Severity      string     `json:"severity"`
	Status        string     `json:"status"`
	Source        string     `json:"source"`
	PolicyVersion string     `json:"policyVersion"`
	AIRunID       *string    `json:"aiRunId"`
	DetectedAt    time.Time  `json:"detectedAt"`
	ResolvedAt    *time.Time `json:"resolvedAt"`
}

type TraceNotification struct {
	ID         string           `json:"id"`
	UserID     string           `json:"userId"`
	Kind       string           `json:"kind"`
	RiskID     *string          `json:"riskId"`
	DedupKey   string           `json:"dedupKey"`
	CreatedAt  time.Time        `json:"createdAt"`
	Deliveries []DeliveryRecord `json:"deliveries"`
}

type TraceAction struct {
	ID        string    `json:"id"`
	RiskID    string    `json:"riskId"`
	Type      string    `json:"type"`
	ActorID   string    `json:"actorId"`
	CreatedAt time.Time `json:"createdAt"`
}

type TraceOutcome struct {
	ID            string    `json:"id"`
	OpportunityID string    `json:"opportunityId"`
	Status        string    `json:"status"`
	ActorID       string    `json:"actorId"`
	CreatedAt     time.Time `json:"createdAt"`
}

type TraceRevenue struct {
	EventID       string    `json:"eventId"`
	OpportunityID string    `json:"opportunityId"`
	Amount        string    `json:"amount"`
	Currency      string    `json:"currency"`
	Status        string    `json:"status"`
	Attribution   *string   `json:"attribution"`
	RiskID        *string   `json:"riskId"`
	ConfirmedAt   time.Time `json:"confirmedAt"`
}

// Trace — цепочка LR-BE-2310 от сообщения до выручки без текста сообщений,
// промптов и сырого вывода модели.
type Trace struct {
	Message       TraceMessage         `json:"message"`
	Jobs          []JobRecord          `json:"jobs"`
	AIJobs        []AIJobRecord        `json:"aiJobs"`
	AIRuns        []AIRun              `json:"aiRuns"`
	Summary       *ConversationSummary `json:"semanticResult"`
	Risks         []TraceRisk          `json:"risks"`
	Notifications []TraceNotification  `json:"notifications"`
	Actions       []TraceAction        `json:"actions"`
	Outcomes      []TraceOutcome       `json:"outcomes"`
	Revenue       []TraceRevenue       `json:"revenue"`
}
