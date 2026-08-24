package postgres

import (
	"strings"
	"testing"
)

func TestEmbeddedMigrationsAreOrderedAndChecksummed(t *testing.T) {
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations() error = %v", err)
	}
	if len(migrations) == 0 {
		t.Fatal("loadMigrations() returned no migrations")
	}
	for index, item := range migrations {
		if len(item.checksum) != 64 || strings.TrimSpace(item.sql) == "" {
			t.Fatalf("migration %d = %#v", index, item)
		}
		if index > 0 && migrations[index-1].version >= item.version {
			t.Fatalf("migrations are not ordered: %s then %s", migrations[index-1].version, item.version)
		}
	}
}
