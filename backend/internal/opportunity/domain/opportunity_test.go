package domain

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestStageTransitionMatrix(t *testing.T) {
	tests := []struct {
		name string
		from Stage
		to   Stage
		want bool
	}{
		{"повтор идемпотентен", StageNew, StageNew, true},
		{"следующий этап", StageNew, StageEngaged, true},
		{"пропуск вперёд", StageNew, StagePriceSent, true},
		{"откат запрещён", StagePriceSent, StageEngaged, false},
		{"потеря из активного этапа", StageQualifying, StageLost, true},
		{"победа только после записи", StageQualifying, StageWon, false},
		{"победа после записи", StageBooked, StageWon, true},
		{"архивирование победы", StageWon, StageArchived, true},
		{"архивирование потери", StageLost, StageArchived, true},
		{"повторное открытие запрещено", StageLost, StageNew, false},
		{"архив окончателен", StageArchived, StageLost, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.from.CanTransitionTo(test.to); got != test.want {
				t.Fatalf("CanTransitionTo() = %v, нужно %v", got, test.want)
			}
		})
	}
}

func TestOpportunityDoesNotInventPotentialRevenue(t *testing.T) {
	now := time.Now().UTC()
	opportunity, err := NewOpportunity("opportunity", "tenant", "conversation", nil, nil, nil, "", now)
	if err != nil {
		t.Fatal(err)
	}
	if opportunity.EstimatedAmount != nil || opportunity.EstimatedAmountConfidence != nil || opportunity.Currency != "RUB" {
		t.Fatalf("возможность = %#v", opportunity)
	}
	encoded, err := json.Marshal(opportunity)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) == "" {
		t.Fatal("пустой JSON")
	}
}

func TestPotentialRevenueAndConfidenceValidation(t *testing.T) {
	for _, raw := range []string{"-1", "1.001", "1000000000000.00", "NaN", "1e2", ""} {
		if _, err := ParsePotentialRevenue(raw); !errors.Is(err, ErrInvalid) {
			t.Errorf("ParsePotentialRevenue(%q) = %v", raw, err)
		}
	}
	amount, err := ParsePotentialRevenue("1500")
	if err != nil || amount.String() != "1500.00" {
		t.Fatalf("сумма = %q, %v", amount.String(), err)
	}
	for _, raw := range []string{"-0.001", "1.001", "0.0001", "NaN"} {
		if _, err := ParseConfidence(raw); !errors.Is(err, ErrInvalid) {
			t.Errorf("ParseConfidence(%q) = %v", raw, err)
		}
	}
}

func TestUserHistoryRequiresActor(t *testing.T) {
	_, err := NewHistory("history", "tenant", "opportunity", nil, StageNew, SourceUser, nil, nil, nil, time.Now())
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("ошибка = %v", err)
	}
}
