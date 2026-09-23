package domain

import (
	"errors"
	"testing"
	"time"
)

// validRisk возвращает риск, проходящий все инварианты; тесты меняют одно поле.
func validRisk() Risk {
	detected := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	return Risk{
		ID: "risk-1", TenantID: "tenant-1", OpportunityID: "opportunity-1", LocationID: "location-1",
		Type: TypeNoResponse, Severity: SeverityHigh, Status: StatusOpen, Source: SourceRule,
		PolicyVersion: "rules/v1", TriggerMessageID: "message-1",
		ReasonCode: "NO_RESPONSE_THRESHOLD_EXCEEDED", Reason: "Бизнес не ответил клиенту",
		DetectedAt: detected, DueAt: detected.Add(-time.Hour), UpdatedAt: detected,
	}
}

func TestRiskValidateSources(t *testing.T) {
	confidence := 0.9
	runID := "run-1"
	cases := []struct {
		name  string
		apply func(*Risk)
		valid bool
	}{
		{"правило без AI-полей", func(r *Risk) {}, true},
		{"правило с confidence", func(r *Risk) { r.Confidence = &confidence }, false},
		{"ручной риск без AI-полей", func(r *Risk) { r.Source = SourceManual }, true},
		{"ручной риск с AI-полями", func(r *Risk) {
			r.Source = SourceManual
			r.Confidence = &confidence
			r.AIRunID = &runID
		}, false},
		{"ручной риск любого поддерживаемого типа", func(r *Risk) {
			r.Source = SourceManual
			r.Type = TypePromiseNotFulfilled
			r.Severity = SeverityHigh
		}, true},
		{"обещание от правила не допускается", func(r *Risk) {
			r.Type = TypePromiseNotFulfilled
			r.Severity = SeverityHigh
		}, false},
		{"гибрид с AI-полями", func(r *Risk) {
			r.Source = SourceHybrid
			r.Type = TypeCustomerSilentAfterPrice
			r.Severity = SeverityMedium
			r.Confidence = &confidence
			r.AIRunID = &runID
		}, true},
		{"гибрид без прогона AI", func(r *Risk) {
			r.Source = SourceHybrid
			r.Type = TypeCustomerSilentAfterPrice
			r.Severity = SeverityMedium
			r.Confidence = &confidence
		}, false},
		{"неизвестный источник", func(r *Risk) { r.Source = Source("IMPORT") }, false},
		{"ручной риск нарушает серьёзность типа", func(r *Risk) {
			r.Source = SourceManual
			r.Severity = SeverityLow
		}, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			risk := validRisk()
			testCase.apply(&risk)
			err := risk.Validate()
			if testCase.valid && err != nil {
				t.Fatalf("ожидался корректный риск, получено %v", err)
			}
			if !testCase.valid && !errors.Is(err, ErrInvalidRisk) {
				t.Fatalf("ожидался ErrInvalidRisk, получено %v", err)
			}
		})
	}
}
