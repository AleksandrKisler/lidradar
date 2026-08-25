package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

var domainForbiddenImports = []string{
	"lidradar/backend/platform",
	"net/http",
}

var domainAllowedExternalImports = []string{
	"github.com/shopspring/decimal",
}

func checkTree(root string) ([]string, error) {
	var violations []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if isRuntimeCommand(path) {
			constructors, err := memoryAdapterReferences(path)
			if err != nil {
				return err
			}
			for _, constructor := range constructors {
				relative, relErr := filepath.Rel(root, path)
				if relErr != nil {
					return relErr
				}
				violations = append(violations, fmt.Sprintf(
					"%s: runtime command references %s (in-memory adapters are test-only)",
					relative, constructor,
				))
			}
		}

		layer := packageLayer(path)
		if layer == "" {
			return nil
		}
		imports, err := fileImports(path)
		if err != nil {
			return err
		}
		for _, imported := range imports {
			if reason := forbiddenImport(layer, imported); reason != "" {
				relative, relErr := filepath.Rel(root, path)
				if relErr != nil {
					return relErr
				}
				violations = append(violations, fmt.Sprintf("%s: %s imports %q (%s)", relative, layer, imported, reason))
			}
		}
		return nil
	})
	return violations, err
}

func isRuntimeCommand(path string) bool {
	parts := strings.Split(filepath.ToSlash(path), "/")
	for _, part := range parts {
		if part == "cmd" {
			return true
		}
	}
	return false
}

func memoryAdapterReferences(path string) ([]string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	seen := make(map[string]struct{})
	ast.Inspect(file, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		name := selector.Sel.Name
		if strings.HasPrefix(name, "NewMemory") || strings.HasPrefix(name, "NewTestMemory") ||
			name == "MemoryStore" || name == "MemoryRepository" {
			seen[name] = struct{}{}
		}
		return true
	})
	references := make([]string, 0, len(seen))
	for name := range seen {
		references = append(references, name)
	}
	sort.Strings(references)
	return references, nil
}

func packageLayer(path string) string {
	parts := strings.Split(filepath.ToSlash(path), "/")
	for _, part := range parts {
		switch part {
		case "domain", "application", "infrastructure", "transport":
			return part
		}
	}
	return ""
}

func fileImports(path string) ([]string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	imports := make([]string, 0, len(file.Imports))
	for _, specification := range file.Imports {
		imported, err := strconv.Unquote(specification.Path.Value)
		if err != nil {
			return nil, fmt.Errorf("parse import in %s: %w", path, err)
		}
		imports = append(imports, imported)
	}
	return imports, nil
}

func forbiddenImport(layer, imported string) string {
	importedLayer := importLayer(imported)
	switch layer {
	case "domain":
		for _, prefix := range domainForbiddenImports {
			if imported == prefix || strings.HasPrefix(imported, prefix+"/") {
				return "domain must not depend on transport or persistence technology"
			}
		}
		if importedLayer != "" {
			return "domain must not depend on another module layer"
		}
		if isExternalImport(imported) && !hasImportPrefix(imported, domainAllowedExternalImports) {
			return "domain must not depend on an external SDK or adapter"
		}
	case "application":
		if importedLayer == "infrastructure" || importedLayer == "transport" {
			return "application may depend on domain, not adapters"
		}
	case "infrastructure":
		if importedLayer == "transport" {
			return "infrastructure must not depend on transport"
		}
	}
	return ""
}

func isExternalImport(imported string) bool {
	first, _, _ := strings.Cut(imported, "/")
	return strings.Contains(first, ".")
}

func hasImportPrefix(imported string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if imported == prefix || strings.HasPrefix(imported, prefix+"/") {
			return true
		}
	}
	return false
}

func importLayer(imported string) string {
	return packageLayer(filepath.FromSlash(imported))
}
