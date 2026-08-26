package domain

import (
	"encoding/json"
	"testing"
	"time"
)

func TestMoneyIsExactAndRejectsUnsafeValues(t *testing.T) {
	for raw, expected := range map[string]string{
		"1": "1.00", "47.1": "47.10", "999999999999.99": "999999999999.99",
	} {
		money, err := ParseMoney(raw)
		if err != nil || money.String() != expected {
			t.Errorf("ParseMoney(%q) = %s, %v", raw, money.String(), err)
		}
		encoded, err := json.Marshal(money)
		if err != nil || string(encoded) != `"`+expected+`"` {
			t.Errorf("JSON(%q) = %s, %v", raw, encoded, err)
		}
	}
	for _, raw := range []string{"", "0", "-1", "+1", "1.001", "1000000000000", "NaN", "1e3"} {
		if _, err := ParseMoney(raw); err == nil {
			t.Errorf("небезопасная сумма %q принята", raw)
		}
	}
	zero, err := ParseNonNegativeMoney("0.00")
	if err != nil || zero.String() != "0.00" {
		t.Fatalf("нулевой итог = %s, %v", zero.String(), err)
	}
}

func TestConfirmationCarriesSourceAndFormalChain(t *testing.T) {
	now := time.Date(2026, 8, 26, 9, 0, 0, 0, time.UTC)
	event, err := NewConfirmedEvent("event", "tenant", "opportunity", "47000", "rub", "actor", now)
	if err != nil || event.Status != StatusConfirmed || event.Source != SourceUser || event.Currency != "RUB" {
		t.Fatalf("событие = %#v, ошибка = %v", event, err)
	}
	if _, err := NewAttribution("attribution", event, AttributionRecovered, "risk", "action", "outcome", now); err != nil {
		t.Fatal(err)
	}
	if _, err := NewAttribution("attribution", event, AttributionRecovered, "", "action", "outcome", now); err == nil {
		t.Fatal("неполная цепочка RECOVERED принята")
	}
	if _, err := NewAttribution("attribution", event, AttributionOrganic, "risk", "", "", now); err == nil {
		t.Fatal("ORGANIC с цепочкой принят")
	}
}
