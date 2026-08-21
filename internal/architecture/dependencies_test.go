package architecture

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRepositoryDependencies(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate architecture test")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", ".."))

	violations, err := Check(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, violation := range violations {
		t.Error(violation)
	}
}

func TestCheckRejectsPersistenceImportFromDomain(t *testing.T) {
	root := testRepository(t, map[string]string{
		"internal/catalog/domain/product.go": "package domain\nimport _ \"github.com/jackc/pgx/v5\"\n",
	})

	violations, err := Check(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 1 {
		t.Fatalf("got %d violations, want 1: %v", len(violations), violations)
	}
	if got := violations[0].String(); !strings.Contains(got, "domain package must not import \"github.com/jackc/pgx/v5\"") {
		t.Fatalf("unexpected violation: %s", got)
	}
}

func TestCheckEnforcesLayerDirection(t *testing.T) {
	root := testRepository(t, map[string]string{
		"internal/catalog/domain/product.go":      "package domain\nimport _ \"example.com/lidradar/internal/catalog/infrastructure\"\n",
		"internal/catalog/application/service.go": "package application\nimport _ \"example.com/lidradar/internal/catalog/delivery\"\n",
		"internal/catalog/delivery/http.go":       "package delivery\nimport _ \"example.com/lidradar/internal/catalog/application\"\n",
	})

	violations, err := Check(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 2 {
		t.Fatalf("got %d violations, want 2: %v", len(violations), violations)
	}
}

func testRepository(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	files["go.mod"] = "module example.com/lidradar\n\ngo 1.26\n"
	for name, contents := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}
