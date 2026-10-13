// ABOUTME: Pins writeTestSession's turn shape against what the claude-code adapter actually parses.
// ABOUTME: Exists because the fixture's "type":"human" parsed to empty content, so user assertions were vacuous (#107).

package sync

import (
	"context"
	"os"
	"testing"

	"github.com/2389-research/ccvault/internal/config"

	// Register the claude-code adapter.
	_ "github.com/2389-research/ccvault/pkg/adapter/claudecode"
)

// The guard this package was missing. writeTestSession wrote the user turn as
// "type": "human", a type the claude-code adapter does not recognise, so the
// turn landed with empty content and every assertion about user text in this
// package was matching nothing against nothing.
//
// Asserting on the stored content — not on a row count — is what makes that
// visible: the row existed all along.
func TestWriteTestSession_UserTurnLandsWithItsContent(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	claudeHome, err := os.MkdirTemp("", "claude-home-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(claudeHome) }()

	sessionID := "9f8e7d6c-5b4a-3928-1716-050403020100"
	writeTestSession(t, claudeHome, sessionID, "-Users-test-fixtureshape")

	syncer := New(database, []config.SourceConfig{
		{Name: "claude-code", Type: "claude-code", Path: claudeHome},
	}, WithFullSync(true))
	if _, err := syncer.Run(context.Background()); err != nil {
		t.Fatalf("Run(): %v", err)
	}

	var gotType, gotContent string
	row := database.QueryRow(
		`SELECT type, content FROM turns WHERE id = ?`, sessionID+"-turn-1")
	if err := row.Scan(&gotType, &gotContent); err != nil {
		t.Fatalf("scan user turn: %v", err)
	}

	if gotType != "user" {
		t.Errorf("user turn type = %q, want \"user\" — the type real Claude Code transcripts use", gotType)
	}
	if gotContent != "Hello world" {
		t.Errorf("user turn content = %q, want %q; an empty value means the adapter dropped it and user-content assertions are vacuous", gotContent, "Hello world")
	}
}

// Full-text search is the surface the vacuity hid behind: an empty-content
// turn produces no FTS row, so a search assertion over fixture user text
// could only ever have matched the assistant turn.
func TestWriteTestSession_UserTurnIsSearchable(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	claudeHome, err := os.MkdirTemp("", "claude-home-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(claudeHome) }()

	sessionID := "1a2b3c4d-5e6f-7a8b-9c0d-1e2f3a4b5c6d"
	writeTestSession(t, claudeHome, sessionID, "-Users-test-fixturefts")

	syncer := New(database, []config.SourceConfig{
		{Name: "claude-code", Type: "claude-code", Path: claudeHome},
	}, WithFullSync(true))
	if _, err := syncer.Run(context.Background()); err != nil {
		t.Fatalf("Run(): %v", err)
	}

	var hits int
	if err := database.QueryRow(
		`SELECT count(*) FROM turns_fts WHERE turns_fts MATCH 'Hello'`).Scan(&hits); err != nil {
		t.Fatalf("query fts: %v", err)
	}
	if hits != 1 {
		t.Errorf("FTS matches for the fixture's user text = %d, want 1", hits)
	}
}
