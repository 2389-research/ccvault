// ABOUTME: Tests that an inferred source tree which does not exist stops the sync loudly.
// ABOUTME: Without it, --data-dir's implied isolation (#68) would produce a silently empty archive.

package sync

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/2389-research/ccvault/internal/config"

	// Register the claude-code adapter.
	_ "github.com/2389-research/ccvault/pkg/adapter/claudecode"
)

// --data-dir infers its source tree when nothing says otherwise, and the
// inferred directory usually will not exist. Treating that like any other
// unreadable source would exit 0 with "Found 0 session files", so a caller
// who meant to index the real transcripts would get an empty archive and no
// indication why. It has to be a refusal, naming the inference.
func TestImpliedSourceThatDoesNotExistIsARefusal(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	dataDir := t.TempDir()
	inferred := filepath.Join(dataDir, config.ImpliedClaudeHomeDir)

	syncer := New(database, []config.SourceConfig{
		{Name: "claude-code", Type: "claude-code", Path: inferred, Implied: true},
	})

	_, err := syncer.Run(context.Background())
	if err == nil {
		t.Fatal("an inferred source tree that does not exist must stop the sync, not be skipped")
	}
	msg := err.Error()
	for _, want := range []string{inferred, "--claude-home"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error should mention %q so the caller can act on it; got: %v", want, err)
		}
	}
}

// An inferred tree that DOES exist is an ordinary source. A fixture that has
// laid out sessions under <data-dir>/claude needs no flags at all.
func TestImpliedSourceThatExistsSyncsNormally(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	dataDir := t.TempDir()
	inferred := filepath.Join(dataDir, config.ImpliedClaudeHomeDir)
	writeTestSession(t, inferred, "5c5c5c5c-1111-2222-3333-444444444444", "-Users-test-inferred")

	syncer := New(database, []config.SourceConfig{
		{Name: "claude-code", Type: "claude-code", Path: inferred, Implied: true},
	})

	stats, err := syncer.Run(context.Background())
	if err != nil {
		t.Fatalf("Run(): %v", err)
	}
	if stats.SessionsIndexed != 1 {
		t.Errorf("sessions indexed = %d, want 1", stats.SessionsIndexed)
	}
}

// A source the user named themselves keeps the old lenient behaviour: a
// missing tree is recorded and the run continues, so one bad entry in a
// multi-source config does not abandon the others.
func TestNamedSourceThatDoesNotExistIsStillSkipped(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	missing := filepath.Join(t.TempDir(), "nope")

	syncer := New(database, []config.SourceConfig{
		{Name: "claude-code", Type: "claude-code", Path: missing},
	})

	stats, err := syncer.Run(context.Background())
	if err != nil {
		t.Fatalf("a named-but-missing source should not abort the run: %v", err)
	}
	if len(stats.Errors) != 1 {
		t.Errorf("want the missing source recorded in stats.Errors, got %v", stats.Errors)
	}
}
