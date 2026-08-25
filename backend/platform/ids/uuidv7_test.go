package ids

import (
	"regexp"
	"testing"
)

func TestGeneratorCreatesUUIDv7(t *testing.T) {
	first, err := (Generator{}).NewID()
	if err != nil {
		t.Fatalf("NewID() error = %v", err)
	}
	second, err := (Generator{}).NewID()
	if err != nil {
		t.Fatalf("NewID() error = %v", err)
	}
	pattern := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !pattern.MatchString(first) || !pattern.MatchString(second) {
		t.Fatalf("generated IDs are not UUIDv7: %q %q", first, second)
	}
	if first == second {
		t.Fatal("generated duplicate UUIDv7 identifiers")
	}
}

func TestValidAcceptsCanonicalUUIDAndRejectsGarbage(t *testing.T) {
	for _, value := range []string{
		"01890f3a-6c20-7d5b-8c43-123456789abc",
		"550e8400-e29b-41d4-a716-446655440000",
	} {
		if !Valid(value) {
			t.Errorf("Valid(%q) = false", value)
		}
	}
	for _, value := range []string{"", "' OR 1=1", "550e8400e29b41d4a716446655440000", "550e8400-e29b-41d4-a716-44665544000z"} {
		if Valid(value) {
			t.Errorf("Valid(%q) = true", value)
		}
	}
}
