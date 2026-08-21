// Package architecture contains the executable dependency rules for the
// backend. Keeping these rules in the repository makes the architecture check
// part of the ordinary Go test suite rather than a developer-specific linter.
package architecture

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Violation describes one import that crosses a forbidden dependency boundary.
type Violation struct {
	File       string
	Line       int
	Source     string
	ImportPath string
	Reason     string
}

func (v Violation) String() string {
	return fmt.Sprintf("%s:%d: %s package must not import %q (%s)",
		v.File, v.Line, v.Source, v.ImportPath, v.Reason)
}

var forbiddenExternalImports = map[string]map[string]string{
	"domain": {
		"github.com/jackc/pgx": "pgx is a persistence detail",
	},
	"application": {
		"github.com/jackc/pgx": "pgx is a persistence detail",
	},
}

var forbiddenLocalLayers = map[string]map[string]string{
	"domain": {
		"application":    "dependencies point toward domain, not away from it",
		"delivery":       "delivery is an outer layer",
		"infrastructure": "infrastructure is an outer layer",
	},
	"application": {
		"delivery":       "delivery is an outer layer",
		"infrastructure": "infrastructure is an outer layer",
	},
}

// Check walks root and returns every forbidden import in a domain or
// application directory. Layer names are directory segments, so the rules work
// for both top-level layers and layers nested inside individual modules.
func Check(root string) ([]Violation, error) {
	modulePath, err := readModulePath(filepath.Join(root, "go.mod"))
	if err != nil {
		return nil, err
	}

	var violations []Violation
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != root && (entry.Name() == ".git" || entry.Name() == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		source := layer(filepath.ToSlash(filepath.Dir(rel)))
		if source == "" {
			return nil
		}

		fileSet := token.NewFileSet()
		file, err := parser.ParseFile(fileSet, path, nil, parser.ImportsOnly)
		if err != nil {
			return fmt.Errorf("parse %s: %w", rel, err)
		}
		for _, spec := range file.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return fmt.Errorf("parse import in %s: %w", rel, err)
			}
			if reason := externalImportReason(source, importPath); reason != "" {
				violations = append(violations, newViolation(fileSet, spec, rel, source, importPath, reason))
				continue
			}
			if importPath == modulePath || strings.HasPrefix(importPath, modulePath+"/") {
				target := layer(strings.TrimPrefix(importPath, modulePath))
				if reason := forbiddenLocalLayers[source][target]; reason != "" {
					violations = append(violations, newViolation(fileSet, spec, rel, source, importPath, reason))
				}
			}
		}
		return nil
	})
	return violations, err
}

func newViolation(fileSet *token.FileSet, spec *ast.ImportSpec, file, source, importPath, reason string) Violation {
	return Violation{File: filepath.ToSlash(file), Line: fileSet.Position(spec.Pos()).Line, Source: source, ImportPath: importPath, Reason: reason}
}

func externalImportReason(source, importPath string) string {
	for prefix, reason := range forbiddenExternalImports[source] {
		if importPath == prefix || strings.HasPrefix(importPath, prefix+"/") {
			return reason
		}
	}
	return ""
}

func layer(path string) string {
	for _, segment := range strings.FieldsFunc(path, func(r rune) bool { return r == '/' || r == '\\' }) {
		switch segment {
		case "domain", "application", "delivery", "infrastructure":
			return segment
		}
	}
	return ""
}

func readModulePath(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open go.mod: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 && fields[0] == "module" {
			return fields[1], nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read go.mod: %w", err)
	}
	return "", fmt.Errorf("go.mod has no module directive")
}
