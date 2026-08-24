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
