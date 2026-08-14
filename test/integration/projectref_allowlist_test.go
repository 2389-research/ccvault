// ABOUTME: Enforces that only internal/projectref (and legit persistence/adapter code) touches Project.DisplayName.
// ABOUTME: Every other reader must go through the four surface-class helpers so the discipline stays uniform.

package integration

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// projectRefAllowlist lists the file globs allowed to read (or, for
// persistence + adapter code, also write) Project.DisplayName directly.
// Every other consumer must go through internal/projectref helpers.
// Adding a file here is a code-review decision — reviewers should push
// back and ask "should this use projectref.Label / Inline / Ref /
// ResolveAll instead?" before granting an entry.
var projectRefAllowlist = []string{
	// The helper package itself is the ONLY reader that turns DisplayName
	// into user-facing output.
	"internal/projectref/",

	// Persistence layer: SELECTs scan into DisplayName, INSERT/UPDATE
	// bind it. Migration test verifies the backfill for existing rows.
	"internal/db/",

	// Adapters populate DisplayName at sync time — the write side.
	"pkg/adapter/",

	// Sync reads parsed session metadata (which carries DisplayName from
	// the adapter) and writes to the DB. Write-side; the value is not
	// rendered here.
	"internal/sync/sync.go",

	// GetDisplayName's write-time helper — the function that FILLS the
	// DisplayName column in the first place.
	"pkg/parser/scanner.go",
}

// TestProjectRefEnforcement scans every .go file under the repo root and
// fails if a selector expression `<x>.DisplayName` appears outside the
// allowlist. This catches the whole class of bug where a new project-
// mentioning surface reads DisplayName directly and forgets the four-
// class rendering discipline.
func TestProjectRefEnforcement(t *testing.T) {
	repoRoot := findRepoRoot(t)

	violations := []string{}
	fset := token.NewFileSet()

	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			// Skip common non-source directories.
			if name == ".git" || name == "vendor" || name == "node_modules" ||
				name == ".worktrees" || name == ".scratch" || name == ".intent" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// Test files exercise the field in their own arrangements; the
		// discipline is about production surface consistency. Migration
		// tests and projectref's own tests wouldn't be able to verify
		// anything if the allowlist banned them.
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}

		rel, err := filepath.Rel(repoRoot, path)
		if err != nil {
			return err
		}
		if isAllowlisted(rel) {
			return nil
		}

		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			// Malformed Go isn't this test's job to flag.
			return nil
		}

		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if sel.Sel.Name != "DisplayName" {
				return true
			}
			// AST-level check: identifier ".DisplayName" outside the
			// allowlist. Field name collisions are theoretically possible
			// (another struct named DisplayName) but unlikely in this
			// codebase; false positives are acceptable — they force the
			// author to either rename their unrelated field, use
			// projectref, or add themselves to the allowlist with a
			// justification in review.
			pos := fset.Position(sel.Pos())
			violations = append(violations, rel+":"+strconv.Itoa(pos.Line))
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo: %v", err)
	}

	if len(violations) > 0 {
		t.Errorf(
			"Project.DisplayName is read outside the projectref allowlist. "+
				"Use internal/projectref (Label / Inline / Ref / ResolveAll) "+
				"or, if this is a genuine persistence/adapter site, add the "+
				"file to projectRefAllowlist with a justification.\n\n"+
				"Violations:\n  %s",
			strings.Join(violations, "\n  "),
		)
	}
}

func isAllowlisted(rel string) bool {
	for _, allowed := range projectRefAllowlist {
		if strings.HasPrefix(rel, allowed) {
			return true
		}
	}
	return false
}

// findRepoRoot returns the repo root (parent of test/integration).
func findRepoRoot(t *testing.T) string {
	t.Helper()
	// This file lives at test/integration/. The repo root is two levels up.
	cwd, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("cwd: %v", err)
	}
	return filepath.Dir(filepath.Dir(cwd))
}
