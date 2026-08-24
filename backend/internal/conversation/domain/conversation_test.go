package domain

import (
	"encoding/json"
	"testing"
	"time"
)

func TestCanonicalChangeRequiresContactOnlyForFirstMessage(t *testing.T) {
	now := time.Now().UTC()
	base := CanonicalChange{
		SourceEventID: "event-1", Type: ChangeReceived, TenantID: "tenant", ConnectionID: "connection",
		Provider: "TEST", ConversationExternalID: "dialog", MessageExternalID: "message",
		Direction: DirectionIncoming, MessageType: MessageText, SentAt: now, OccurredAt: now,
		ReceivedAt: now, Metadata: json.RawMessage(`{}`),
	}
	if base.Validate() == nil {
		t.Fatal("первое сообщение без внешнего идентификатора контакта принято")
	}
	base.ContactExternalID = "contact"
	if err := base.Validate(); err != nil {
		t.Fatalf("корректное первое сообщение отклонено: %v", err)
	}
	base.Type = ChangeDeleted
	base.ContactExternalID = ""
	base.Direction = ""
	base.MessageType = ""
	base.SentAt = time.Time{}
	if err := base.Validate(); err != nil {
		t.Fatalf("корректное удаление отклонено: %v", err)
	}
}

func TestConversationRejectsInconsistentMessageBounds(t *testing.T) {
	now := time.Now().UTC()
	conversation := Conversation{
		ID: "conversation", TenantID: "tenant", ConnectionID: "connection", ContactID: "contact",
		ExternalID: "dialog", Status: ConversationActive, CreatedAt: now, UpdatedAt: now,
	}
	direction := DirectionIncoming
	conversation.LastMessageDirection = &direction
	if conversation.Validate() == nil {
		t.Fatal("переписка с направлением без временных границ принята")
	}
	conversation.FirstMessageAt = &now
	conversation.LastMessageAt = &now
	if err := conversation.Validate(); err != nil {
		t.Fatalf("корректная переписка отклонена: %v", err)
	}
}
