// ABOUTME: End-to-end tests that a synced session's turns carry file-order ordinals
// ABOUTME: and that last_entry_uuid is written and used to spot a rewritten transcript.

package sync

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/2389-research/ccvault/internal/config"
	"github.com/2389-research/ccvault/internal/db"
)

// fixtureTurn is one line of a hand-written transcript.
type fixtureTurn struct {
	uuid string
	// stamp is an RFC3339Nano instant. These are deliberately not in
	// ascending order in the fixtures below.
	stamp string
}

// writeTranscript writes the given turns as JSONL, in slice order, to the
// path a claude-code source scan expects. Called twice on the same path by
// the rewrite test, so it truncates.
func writeTranscript(t *testing.T, claudeHome, projectDir, sessionID string, turns []fixtureTurn) string {
	t.Helper()

	projDir := filepath.Join(claudeHome, "projects", projectDir)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(projDir, sessionID+".jsonl")

	f, err := os.Create(path) //nolint:gosec // path is built from t.TempDir
	if err != nil {
		t.Fatalf("create transcript: %v", err)
	}
	defer func() { _ = f.Close() }()

	enc := json.NewEncoder(f)
	for _, ft := range turns {
		line := map[string]any{
			"uuid":      ft.uuid,
			"sessionId": sessionID,
			"type":      "user",
			"timestamp": ft.stamp,
			"cwd":       "/Users/test/myproject",
			"message": map[string]any{
				"role":    "user",
				"content": "content of " + ft.uuid,
			},
		}
		if err := enc.Encode(line); err != nil {
			t.Fatalf("encode %s: %v", ft.uuid, err)
		}
	}
	return path
}

func runSync(t *testing.T, database *db.DB, claudeHome string) *Stats {
	t.Helper()

	syncer := New(database,
		[]config.SourceConfig{{Name: "claude-code", Type: "claude-code", Path: claudeHome}},
		WithFullSync(true),
	)
	stats, err := syncer.Run(context.Background())
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(stats.Errors) > 0 {
		t.Fatalf("sync errors: %v", stats.Errors)
	}
	return stats
}

const ordinalSessionID = "a1b2c3d4-e5f6-7890-abcd-ef1234567890"

// skewedTranscript is written in one order and timestamped in another: line 0
// carries the latest instant, and lines 1 and 2 tie to the millisecond. Both
// shapes are drawn from the real archive, where 6,597 adjacent turn pairs
// carry a timestamp earlier than the turn before them and 19,160 tie.
var skewedTranscript = []fixtureTurn{
	{"turn-late", "2026-08-04T10:00:09.000Z"},
	{"turn-tie-1", "2026-08-04T10:00:01.000Z"},
	{"turn-tie-2", "2026-08-04T10:00:01.000Z"},
	{"turn-last", "2026-08-04T10:00:03.000Z"},
}

// TestSyncAssignsOrdinalsInFileOrder is the end-to-end proof: a transcript
// whose timestamps disagree with its line order comes back out of the
// database in line order, numbered from 0.
func TestSyncAssignsOrdinalsInFileOrder(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	claudeHome := t.TempDir()
	writeTranscript(t, claudeHome, "-Users-test-myproject", ordinalSessionID, skewedTranscript)

	runSync(t, database, claudeHome)

	turns, err := database.GetTurns(ordinalSessionID)
	if err != nil {
		t.Fatalf("GetTurns: %v", err)
	}
	if len(turns) != len(skewedTranscript) {
		t.Fatalf("got %d turns, want %d", len(turns), len(skewedTranscript))
	}
	for i, ft := range skewedTranscript {
		if turns[i].ID != ft.uuid {
			t.Errorf("position %d holds %q, want %q", i, turns[i].ID, ft.uuid)
		}
		if turns[i].Ordinal != i {
			t.Errorf("turn %q: ordinal = %d, want %d", turns[i].ID, turns[i].Ordinal, i)
		}
	}
}

// TestSyncWritesLastEntryUUID covers the session-level cursor: the last turn
// in file order, not the one with the latest timestamp.
func TestSyncWritesLastEntryUUID(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	claudeHome := t.TempDir()
	writeTranscript(t, claudeHome, "-Users-test-myproject", ordinalSessionID, skewedTranscript)

	runSync(t, database, claudeHome)

	session, err := database.GetSession(ordinalSessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if session == nil {
		t.Fatal("session not found after sync")
	}
	// "turn-late" is the latest timestamp in the fixture; "turn-last" is the
	// last line. The cursor must name the line.
	if session.LastEntryUUID != "turn-last" {
		t.Errorf("LastEntryUUID = %q, want %q", session.LastEntryUUID, "turn-last")
	}

	cursor, err := database.SessionTurnCursor(ordinalSessionID)
	if err != nil {
		t.Fatalf("SessionTurnCursor: %v", err)
	}
	if !cursor.Found || cursor.LastOrdinal != 3 || cursor.LastEntryUUID != "turn-last" {
		t.Errorf("cursor = %+v, want {3 turn-last true}", cursor)
	}
}

// TestSyncReportsRewrittenTranscript is what last_entry_uuid is for in code.
//
// A transcript normally grows by appending, so a re-sync still finds the turn
// the previous run left at the end of the sequence. These two runs read a file
// whose contents were replaced wholesale between them — a truncation, a
// restore from backup, or a fork that reused the session id. The replace is
// still correct; the point is that it is now reported instead of silent.
func TestSyncReportsRewrittenTranscript(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	claudeHome := t.TempDir()
	writeTranscript(t, claudeHome, "-Users-test-myproject", ordinalSessionID, skewedTranscript)

	first := runSync(t, database, claudeHome)
	if first.SessionsRewrittenUpstream != 0 {
		t.Fatalf("first sync of a new session reported %d rewritten, want 0",
			first.SessionsRewrittenUpstream)
	}

	// Appending leaves the stored tail in place, so a grown transcript must
	// not be reported.
	appended := append(append([]fixtureTurn{}, skewedTranscript...),
		fixtureTurn{"turn-appended", "2026-08-04T10:00:11.000Z"})
	writeTranscript(t, claudeHome, "-Users-test-myproject", ordinalSessionID, appended)

	grown := runSync(t, database, claudeHome)
	if grown.SessionsRewrittenUpstream != 0 {
		t.Errorf("an appended transcript reported %d rewritten, want 0 — the check "+
			"must not fire on the normal case", grown.SessionsRewrittenUpstream)
	}
	if grown.SessionsIndexed != 1 {
		t.Fatalf("SessionsIndexed = %d after append, want 1", grown.SessionsIndexed)
	}

	// Now replace the file's contents entirely, keeping the session id.
	writeTranscript(t, claudeHome, "-Users-test-myproject", ordinalSessionID, []fixtureTurn{
		{"turn-rewritten-1", "2026-08-05T10:00:00.000Z"},
		{"turn-rewritten-2", "2026-08-05T10:00:01.000Z"},
	})

	rewritten := runSync(t, database, claudeHome)
	if rewritten.SessionsRewrittenUpstream != 1 {
		t.Errorf("SessionsRewrittenUpstream = %d after the transcript was replaced, want 1",
			rewritten.SessionsRewrittenUpstream)
	}

	// The replace still happened, and the new turns are numbered from 0.
	turns, err := database.GetTurns(ordinalSessionID)
	if err != nil {
		t.Fatalf("GetTurns: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("got %d turns after the rewrite, want 2", len(turns))
	}
	if turns[0].ID != "turn-rewritten-1" || turns[0].Ordinal != 0 {
		t.Errorf("first turn after rewrite = (%q, %d), want (turn-rewritten-1, 0)",
			turns[0].ID, turns[0].Ordinal)
	}
	if turns[1].Ordinal != 1 {
		t.Errorf("second turn after rewrite: ordinal = %d, want 1", turns[1].Ordinal)
	}
}
