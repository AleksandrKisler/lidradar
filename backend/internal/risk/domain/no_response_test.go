package domain

import (
	"errors"
	"testing"
	"time"
)

func weekdayHours() BusinessHours {
	weekly := make(map[time.Weekday][]BusinessPeriod)
	for day := time.Monday; day <= time.Friday; day++ {
		weekly[day] = []BusinessPeriod{{Open: Clock(9, 0), Close: Clock(21, 0)}}
	}
	return BusinessHours{Timezone: "Europe/Moscow", Weekly: weekly}
}

func localTime(t *testing.T, value string) time.Time {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := time.ParseInLocation("2006-01-02 15:04", value, loc)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func baseState(t *testing.T, received string) ConversationState {
	return ConversationState{
		TenantID: "tenant-a", OpportunityID: "opportunity-a", LocationID: "location-a",
		ActiveOpportunity: true, LastMeaningfulID: "message-a", LastMeaningfulAt: localTime(t, received),
		LastMeaningful: DirectionIncoming, ResponseThreshold: 45 * time.Minute, BusinessHours: weekdayHours(),
	}
}

func TestNoResponseBoundaryAtFortyFiveMinutes(t *testing.T) {
	state := baseState(t, "2026-08-17 12:00")
	decision, err := (NoResponsePolicy{}).Evaluate(state, localTime(t, "2026-08-17 12:45"))
	if err != nil {
		t.Fatal(err)
	}
	if !decision.DueAt.Equal(localTime(t, "2026-08-17 12:45")) || decision.Finding == nil || decision.Finding.Severity != SeverityHigh {
		t.Fatalf("unexpected boundary decision: %#v", decision)
	}
}

func TestNoResponseCarriesIntoNextWorkingPeriod(t *testing.T) {
	state := baseState(t, "2026-08-17 20:50")
	decision, err := (NoResponsePolicy{}).Evaluate(state, state.LastMeaningfulAt)
	if err != nil {
		t.Fatal(err)
	}
	want := localTime(t, "2026-08-18 09:35")
	if !decision.DueAt.Equal(want) || decision.Finding != nil {
		t.Fatalf("due = %v, finding = %#v; want %v and nil", decision.DueAt, decision.Finding, want)
	}
}

func TestNoResponseRequiresCurrentUnansweredIncomingAndActiveOpportunity(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ConversationState)
	}{
		{"response before threshold", func(s *ConversationState) { s.OutgoingAfterTrigger = true }},
		{"latest message outgoing", func(s *ConversationState) { s.LastMeaningful = DirectionOutgoing }},
		{"inactive opportunity", func(s *ConversationState) { s.ActiveOpportunity = false }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := baseState(t, "2026-08-17 12:00")
			test.mutate(&state)
			decision, err := (NoResponsePolicy{}).Evaluate(state, localTime(t, "2026-08-17 14:00"))
			if err != nil || decision.Finding != nil {
				t.Fatalf("finding = %#v, err = %v", decision.Finding, err)
			}
		})
	}
}

func TestNoResponseSeverityUsesBusinessMinutes(t *testing.T) {
	state := baseState(t, "2026-08-17 20:50")
	decision, err := (NoResponsePolicy{}).Evaluate(state, localTime(t, "2026-08-18 10:20"))
	if err != nil {
		t.Fatal(err)
	}
	if decision.Finding == nil || decision.Finding.Severity != SeverityCritical {
		t.Fatalf("decision = %#v", decision)
	}
}

func TestBusinessHoursRejectInvalidSchedule(t *testing.T) {
	hours := BusinessHours{Timezone: "Europe/Moscow", Weekly: map[time.Weekday][]BusinessPeriod{
		time.Monday: {{Open: Clock(9, 0), Close: Clock(12, 0)}, {Open: Clock(11, 0), Close: Clock(13, 0)}},
	}}
	_, err := hours.AddBusinessTime(localTime(t, "2026-08-17 09:00"), time.Minute)
	if !errors.Is(err, ErrInvalidBusinessHours) {
		t.Fatalf("err = %v", err)
	}
}

func TestBusinessHoursRespectTimezoneDST(t *testing.T) {
	hours := BusinessHours{Timezone: "Europe/Berlin", Weekly: map[time.Weekday][]BusinessPeriod{
		time.Sunday: {{Open: Clock(1, 0), Close: Clock(4, 0)}},
	}}
	loc, _ := time.LoadLocation("Europe/Berlin")
	from := time.Date(2026, 3, 29, 1, 30, 0, 0, loc)
	due, err := hours.AddBusinessTime(from, 90*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if due.Hour() != 4 || due.Minute() != 0 {
		t.Fatalf("due = %v; want 04:00 after DST jump", due)
	}
}
