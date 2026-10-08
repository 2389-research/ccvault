// ABOUTME: End-to-end test that a real sync of a real JSONL transcript lands tool_uses.is_error (#83).
// ABOUTME: Its own fixture writer, because writeTestSession's "type": "human" parses to nothing (#107).

package sync

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/2389-research/ccvault/internal/config"

	// Register the claude-code adapter.
	_ "github.com/2389-research/ccvault/pkg/adapter/claudecode"
)

// writeToolErrorSession writes a Claude Code transcript holding three calls —
// one that failed, one that succeeded, one nothing answered — as the real
// files are written: tool_use blocks in an assistant message, tool_result
// blocks in the user messages that follow.
//
// Written literally rather than built from maps and encoded, so the bytes on
// disk are the bytes a reader of this test can check against a real transcript.
// Its turn type is "user", not the "human" that writeTestSession in this
// package uses: the claude-code adapter parses "human" to empty content (#107),
// so a fixture built on that helper asserts nothing about tool results.
func writeToolErrorSession(t *testing.T, claudeHome, sessionID string) {
	t.Helper()

	projDir := filepath.Join(claudeHome, "projects", "-Users-test-errors")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	lines := `{"uuid":"` + sessionID + `-t1","sessionId":"` + sessionID + `","type":"user","timestamp":"2026-10-01T10:00:00.000Z","message":{"role":"user","content":"push it"}}
{"uuid":"` + sessionID + `-t2","parentUuid":"` + sessionID + `-t1","sessionId":"` + sessionID + `","type":"assistant","timestamp":"2026-10-01T10:00:01.000Z","message":{"id":"msg-1","model":"claude-sonnet-4-20250514","role":"assistant","content":[{"type":"tool_use","id":"toolu_E2E_FAIL","name":"Bash","input":{"command":"git push"}},{"type":"tool_use","id":"toolu_E2E_OK","name":"Bash","input":{"command":"git status"}},{"type":"tool_use","id":"toolu_E2E_NONE","name":"Bash","input":{"command":"sleep 600"}}],"usage":{"input_tokens":10,"output_tokens":5}}}
{"uuid":"` + sessionID + `-t3","parentUuid":"` + sessionID + `-t2","sessionId":"` + sessionID + `","type":"user","timestamp":"2026-10-01T10:00:02.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_E2E_FAIL","is_error":true,"content":"! [rejected] main -> main (fetch first)"}]}}
{"uuid":"` + sessionID + `-t4","parentUuid":"` + sessionID + `-t3","sessionId":"` + sessionID + `","type":"user","timestamp":"2026-10-01T10:00:03.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_E2E_OK","content":"nothing to commit"}]}}
`

	if err := os.WriteFile(filepath.Join(projDir, sessionID+".jsonl"), []byte(lines), 0o644); err != nil {
		t.Fatalf("write session: %v", err)
	}
}

// TestSyncWritesIsError is the write path end to end: a transcript on disk, a
// real sync, the column read back out of SQLite. Everything below it is tested
// in units, and none of those units would catch a field dropped in the three
// conversions between the parser and the INSERT — which is the failure
// pkg/adapter's round-trip test exists for and this one confirms in the whole.
func TestSyncWritesIsError(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	claudeHome := t.TempDir()
	const sessionID = "e2e1b2c3-d4e5-6789-abcd-ef0123456789"
	writeToolErrorSession(t, claudeHome, sessionID)

	syncer := New(database, []config.SourceConfig{
		{Name: "claude-code", Type: "claude-code", Path: claudeHome},
	}, WithFullSync(true))

	if _, err := syncer.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := map[string]string{
		"toolu_E2E_FAIL": "error",
		"toolu_E2E_OK":   "ok",
		"toolu_E2E_NONE": "unknown",
	}
	for id, state := range want {
		var flag sql.NullInt64
		err := database.QueryRow(`SELECT is_error FROM tool_uses WHERE tool_use_id = ?`, id).Scan(&flag)
		if err != nil {
			t.Fatalf("read is_error for %s: %v", id, err)
		}
		got := "unknown"
		if flag.Valid {
			if flag.Int64 == 1 {
				got = "error"
			} else {
				got = "ok"
			}
		}
		if got != state {
			t.Errorf("%s: is_error = %s, want %s", id, got, state)
		}
	}

	// The per-session flag must agree with the per-call one. They are computed
	// independently — sessions.has_error from the adapter's own scan of the
	// turn, is_error from the parser's result join — so this is a real
	// cross-check, and #83 asks for the two not to disagree confusingly.
	var hasError int
	if err := database.QueryRow(`SELECT has_error FROM sessions WHERE id = ?`, sessionID).Scan(&hasError); err != nil {
		t.Fatalf("read sessions.has_error: %v", err)
	}
	if hasError != 1 {
		t.Errorf("sessions.has_error = %d, want 1 — a session holding a failed call must be flagged", hasError)
	}
}
