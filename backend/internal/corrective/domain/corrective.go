// Package domain owns corrective recommendations and the append-only facts
// recorded after a Risk is detected.
package domain

import (
	"errors"
	"strings"
	"time"
)

var ErrInvalid = errors.New("invalid corrective record")

type ActionType string

const (
	ActionOpenConversation ActionType = "OPEN_CONVERSATION"
	ActionCopyReply        ActionType = "COPY_REPLY"
	ActionMarkContacted    ActionType = "MARK_CONTACTED"
	ActionCall             ActionType = "CALL"
	ActionSendMessage      ActionType = "SEND_MESSAGE"
	ActionOther            ActionType = "OTHER"
)

type OutcomeStatus string

const (
	OutcomeResponded OutcomeStatus = "RESPONDED"
	OutcomeBooked    OutcomeStatus = "BOOKED"
	OutcomePaid      OutcomeStatus = "PAID"
	OutcomeLost      OutcomeStatus = "LOST"
	OutcomeThinking  OutcomeStatus = "THINKING"
	OutcomeNotALead  OutcomeStatus = "NOT_A_LEAD"
)

type Recommendation struct {
	ID        string    `json:"id"`
	TenantID  string    `json:"-"`
	RiskID    string    `json:"riskId"`
	Text      string    `json:"text"`
	Source    string    `json:"source"`
	CreatedAt time.Time `json:"createdAt"`
}

type Action struct {
	ID        string     `json:"id"`
	TenantID  string     `json:"-"`
	RiskID    string     `json:"riskId"`
	ActorID   string     `json:"actorId"`
	Type      ActionType `json:"type"`
	Note      string     `json:"note,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
}

type Outcome struct {
	ID            string        `json:"id"`
	TenantID      string        `json:"-"`
	OpportunityID string        `json:"opportunityId"`
	ActorID       string        `json:"actorId"`
	Status        OutcomeStatus `json:"status"`
	Note          string        `json:"note,omitempty"`
	CreatedAt     time.Time     `json:"createdAt"`
}

func NewRecommendation(id, tenantID, riskID, text string, at time.Time) (Recommendation, error) {
	if id == "" || tenantID == "" || riskID == "" || strings.TrimSpace(text) == "" || at.IsZero() {
		return Recommendation{}, ErrInvalid
	}
	return Recommendation{ID: id, TenantID: tenantID, RiskID: riskID, Text: text, Source: "TEMPLATE", CreatedAt: at.UTC()}, nil
}

func NewAction(id, tenantID, riskID, actorID string, kind ActionType, note string, at time.Time) (Action, error) {
	if id == "" || tenantID == "" || riskID == "" || actorID == "" || !validAction(kind) || at.IsZero() {
		return Action{}, ErrInvalid
	}
	return Action{ID: id, TenantID: tenantID, RiskID: riskID, ActorID: actorID, Type: kind, Note: strings.TrimSpace(note), CreatedAt: at.UTC()}, nil
}

func NewOutcome(id, tenantID, opportunityID, actorID string, status OutcomeStatus, note string, at time.Time) (Outcome, error) {
	if id == "" || tenantID == "" || opportunityID == "" || actorID == "" || !validOutcome(status) || at.IsZero() {
		return Outcome{}, ErrInvalid
	}
	return Outcome{ID: id, TenantID: tenantID, OpportunityID: opportunityID, ActorID: actorID, Status: status, Note: strings.TrimSpace(note), CreatedAt: at.UTC()}, nil
}

func validAction(value ActionType) bool {
	switch value {
	case ActionOpenConversation, ActionCopyReply, ActionMarkContacted, ActionCall, ActionSendMessage, ActionOther:
		return true
	}
	return false
}
func validOutcome(value OutcomeStatus) bool {
	switch value {
	case OutcomeResponded, OutcomeBooked, OutcomePaid, OutcomeLost, OutcomeThinking, OutcomeNotALead:
		return true
	}
	return false
}
