package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckTreeAllowsCanonicalDirection(t *testing.T) {
	root := t.TempDir()
	writeGoFile(t, root, "internal/risk/domain/risk.go", `package domain`)
	writeGoFile(t, root, "internal/risk/application/usecase.go", `package application
import "example/internal/risk/domain"
var _ domain.Risk`)

	violations, err := checkTree(root)
	if err != nil {
		t.Fatalf("checkTree() error = %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("checkTree() violations = %v", violations)
	}
}

func TestCheckTreeRejectsDomainPGXImport(t *testing.T) {
	root := t.TempDir()
	writeGoFile(t, root, "internal/risk/domain/repository.go", `package domain
import "github.com/jackc/pgx/v5"
var _ pgx.Tx`)

	violations, err := checkTree(root)
	if err != nil {
		t.Fatalf("checkTree() error = %v", err)
	}
	if len(violations) != 1 || !strings.Contains(violations[0], "github.com/jackc/pgx/v5") {
		t.Fatalf("checkTree() violations = %v, want pgx violation", violations)
	}
}

func TestCheckTreeRejectsDomainProviderSDK(t *testing.T) {
	root := t.TempDir()
	writeGoFile(t, root, "internal/risk/domain/provider.go", `package domain
import "example.com/provider/sdk"
var _ sdk.Client`)

	violations, err := checkTree(root)
	if err != nil {
		t.Fatalf("checkTree() error = %v", err)
	}
	if len(violations) != 1 || !strings.Contains(violations[0], "external SDK") {
		t.Fatalf("checkTree() violations = %v, want external SDK violation", violations)
	}
}

func TestCheckTreeRejectsReverseLayerDependency(t *testing.T) {
	root := t.TempDir()
	writeGoFile(t, root, "internal/risk/domain/risk.go", `package domain
import "example/internal/risk/transport"
var _ transport.Handler`)

	violations, err := checkTree(root)
	if err != nil {
		t.Fatalf("checkTree() error = %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("checkTree() violations = %v, want one violation", violations)
	}
}

func writeGoFile(t *testing.T, root, name, contents string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
}
