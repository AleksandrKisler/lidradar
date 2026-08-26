package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCorrectiveFactsNormalizeNotesAndRejectUnsupportedValues(t *testing.T) {
	at := time.Date(2026, 8, 26, 9, 0, 0, 0, time.UTC)
	action, err := NewAction("action", "tenant", "risk", "actor", ActionCall, "  Позвонить  ", at)
	if err != nil || action.Note != "Позвонить" || !action.CreatedAt.Equal(at) {
		t.Fatalf("действие = %#v, ошибка = %v", action, err)
	}
	outcome, err := NewOutcome("outcome", "tenant", "opportunity", "actor", OutcomeBooked, "  Записан  ", at)
	if err != nil || outcome.Note != "Записан" {
		t.Fatalf("исход = %#v, ошибка = %v", outcome, err)
	}
	if _, err := NewAction("action", "tenant", "risk", "actor", ActionType("DELETE_ALL"), "", at); !errors.Is(err, ErrInvalid) {
		t.Fatalf("неизвестное действие принято: %v", err)
	}
	if _, err := NewOutcome("outcome", "tenant", "opportunity", "actor", OutcomeStatus("REFUND"), "", at); !errors.Is(err, ErrInvalid) {
		t.Fatalf("неизвестный исход принят: %v", err)
	}
	if _, err := NewOutcome(
		"outcome", "tenant", "opportunity", "actor", OutcomePaid,
		strings.Repeat("я", maximumTextLength+1), at,
	); !errors.Is(err, ErrInvalid) {
		t.Fatalf("слишком длинное примечание принято: %v", err)
	}
}

func TestRecommendationRequiresUsefulBoundedText(t *testing.T) {
	at := time.Now()
	if _, err := NewRecommendation("id", "tenant", "risk", "   ", at); !errors.Is(err, ErrInvalid) {
		t.Fatalf("пустая рекомендация принята: %v", err)
	}
	recommendation, err := NewRecommendation("id", "tenant", "risk", "  Ответить сейчас.  ", at)
	if err != nil || recommendation.Text != "Ответить сейчас." || recommendation.Source != "TEMPLATE" {
		t.Fatalf("рекомендация = %#v, ошибка = %v", recommendation, err)
	}
}
