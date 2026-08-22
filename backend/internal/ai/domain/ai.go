// Package domain defines the durable AI queue state. It has no provider,
// transport, or persistence dependencies.
package domain

import "time"

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
	ID, TenantID, ConversationID, Prompt string
	BaseConversationRevision             int64
	AnalysisThroughMessageID             string
	Status                               JobStatus
	ClaimedBy                            string
	LeaseUntil                           time.Time
	CreatedAt, UpdatedAt                 time.Time
}

type ApplicationStatus string

const (
	ApplicationPending  ApplicationStatus = "PENDING"
	ApplicationApplied  ApplicationStatus = "APPLIED"
	ApplicationStale    ApplicationStatus = "STALE"
	ApplicationRejected ApplicationStatus = "REJECTED"
)

type Run struct {
	ID, JobID, NodeID, Output, Error string
	Status                           JobStatus
	ApplicationStatus                ApplicationStatus
	BaseConversationRevision         int64
	AnalysisThroughMessageID         string
	StartedAt, CompletedAt           time.Time
}
