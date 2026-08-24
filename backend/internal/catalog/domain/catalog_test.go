package domain

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestServiceCatalogItemNormalizesNameAndSerializesExactPrices(t *testing.T) {
	from, err := ParsePrice("1200")
	if err != nil {
		t.Fatal(err)
	}
	to, err := ParsePrice("1499.9")
	if err != nil {
		t.Fatal(err)
	}
	item, err := NewServiceCatalogItem("item", "tenant", "  Ceramic   Coating ", nil, &from, &to, "rub", time.Now())
	if err != nil {
		t.Fatalf("NewServiceCatalogItem() error = %v", err)
	}
	if item.Name != "Ceramic Coating" || item.NormalizedName != "ceramic coating" || item.Currency != "RUB" {
		t.Fatalf("normalized item = %#v", item)
	}
	encoded, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	want := `"priceFrom":"1200.00"`
	if !contains(string(encoded), want) || !contains(string(encoded), `"priceTo":"1499.90"`) {
		t.Fatalf("JSON = %s", encoded)
	}
}

func TestPriceAndRangeValidation(t *testing.T) {
	for _, input := range []string{"-0.01", "1.001", "1000000000000.00", "1e2", "", "NaN"} {
		if _, err := ParsePrice(input); !errors.Is(err, ErrInvalid) {
			t.Errorf("ParsePrice(%q) error = %v", input, err)
		}
	}
	zero, err := ParsePrice("0")
	if err != nil || zero.String() != "0.00" {
		t.Fatalf("zero price = %q, %v", zero.String(), err)
	}
	from, _ := ParsePrice("2.00")
	to, _ := ParsePrice("1.99")
	if _, err := NewServiceCatalogItem("item", "tenant", "Service", nil, &from, &to, "RUB", time.Now()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("reversed range error = %v", err)
	}
}

func TestMissingPricesRemainNil(t *testing.T) {
	item, err := NewServiceCatalogItem("item", "tenant", "Consultation", nil, nil, nil, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if item.PriceFrom != nil || item.PriceTo != nil {
		t.Fatalf("prices were invented: %#v", item)
	}
	encoded, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(string(encoded), `"priceFrom":null`) || !contains(string(encoded), `"priceTo":null`) {
		t.Fatalf("JSON = %s", encoded)
	}
}

func contains(value, fragment string) bool {
	for index := 0; index+len(fragment) <= len(value); index++ {
		if value[index:index+len(fragment)] == fragment {
			return true
		}
	}
	return false
}
