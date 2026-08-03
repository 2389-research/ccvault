// ABOUTME: Tests for the shared Claude Code session-file scanner.
// ABOUTME: Focus on error semantics that adapter callers depend on.

package parser

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestScanClaudeHome_MissingProjectsDirWrapsErrNotExist guards issue #13:
// callers (nanoclaw specifically) rely on errors.Is(err, os.ErrNotExist) to
// distinguish "user hasn't created any sessions yet" from real stat failures.
// Prior to the fix ScanClaudeHome returned a plain formatted string that did
// not chain the underlying ErrNotExist, so callers had to swallow all errors
// or use fragile string matching.
func TestScanClaudeHome_MissingProjectsDirWrapsErrNotExist(t *testing.T) {
	// TempDir with no projects/ subdir.
	claudeHome := t.TempDir()

	_, err := ScanClaudeHome(claudeHome)
	if err == nil {
		t.Fatal("expected error for missing projects dir, got nil")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected err to chain os.ErrNotExist, got %v (type %T)", err, err)
	}
}

// TestScanClaudeHome_ProjectsIsFileNotDir verifies the pathological case
// where <claudeHome>/projects exists but is a regular file. This should
// NOT match ErrNotExist so callers surface the error rather than silently
// pretend the dir is empty.
func TestScanClaudeHome_ProjectsIsFileNotDir(t *testing.T) {
	claudeHome := t.TempDir()
	projectsPath := filepath.Join(claudeHome, "projects")
	if err := os.WriteFile(projectsPath, []byte("not a directory"), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := ScanClaudeHome(claudeHome)
	if err == nil {
		t.Fatal("expected error for projects-as-file, got nil")
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Errorf("projects-as-file err should NOT chain ErrNotExist; got %v", err)
	}
}

// TestScanClaudeHome_HappyPath sanity-checks that a well-formed layout still
// scans without error.
func TestScanClaudeHome_HappyPath(t *testing.T) {
	claudeHome := t.TempDir()
	projDir := filepath.Join(claudeHome, "projects", "-Users-test-example")
	if err := os.MkdirAll(projDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Empty projects dir — no sessions, but no error either.
	files, err := ScanClaudeHome(claudeHome)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("expected 0 files in empty projects dir, got %d", len(files))
	}
}
