// ABOUTME: End-to-end cover for #92 — syncing a resumed transcript must not delete the session it resumed
// ABOUTME: And for step three of its order: turn_count is reconciled after the re-parse, never before

package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/2389-research/ccvault/internal/config"
	"github.com/2389-research/ccvault/internal/db"
)

// The session ids these tests use. Uuid-shaped because the claude-code
// scanner only discovers files whose name is a uuid, which is how real Claude
// Code names a transcript.
const (
	sessionEarlier   = "aaaaaaaa-1111-4111-8111-111111111111"
	sessionLater     = "bbbbbbbb-2222-4222-8222-222222222222"
	sessionVanished  = "cccccccc-3333-4333-8333-333333333333"
	sessionUntouched = "dddddddd-4444-4444-8444-444444444444"
	sessionPresent   = "eeeeeeee-5555-4555-8555-555555555555"
)

// transcriptLine is one JSONL entry, written with the uuid the caller names so
// a test can make one transcript repeat another's uuids — which is what a
// resumed Claude Code session does.
type transcriptLine struct {
	uuid string
	text string
	tool string
}

// writeResumableTranscript writes a claude-code transcript holding exactly
// these lines, in order, all carrying sessionID as their sessionId. Separate
// from ordinals_test.go's writeTranscript because the lines here carry text
// and tool calls, which is what makes a shared turn's calls visible.
//
// A resumed transcript stamps the *new* session's id on the lines it copied
// while leaving their uuids alone — which is how session f3806a3c on the
// author's archive came to hold 1,098 turns whose uuids belong to 873d77ef.
func writeResumableTranscript(t *testing.T, root, projectDir, sessionID string, lines []transcriptLine) string {
	t.Helper()

	projPath := filepath.Join(root, "projects", projectDir)
	if err := os.MkdirAll(projPath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(projPath, sessionID+".jsonl")

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create transcript: %v", err)
	}
	defer func() { _ = f.Close() }()

	base := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	enc := json.NewEncoder(f)
	for i, line := range lines {
		content := []map[string]any{{"type": "text", "text": line.text}}
		if line.tool != "" {
			content = append(content, map[string]any{
				"type":  "tool_use",
				"id":    "toolu_" + line.uuid,
				"name":  line.tool,
				"input": map[string]any{"file_path": "/tmp/" + line.uuid},
			})
		}
		entry := map[string]any{
			"uuid":      line.uuid,
			"sessionId": sessionID,
			"type":      "assistant",
			"cwd":       "/tmp/" + projectDir,
			"timestamp": base.Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano),
			"message": map[string]any{
				"id":      "msg-" + line.uuid,
				"model":   "claude-sonnet-4-20250514",
				"role":    "assistant",
				"content": content,
				"usage":   map[string]any{"input_tokens": 10, "output_tokens": 5},
			},
		}
		if err := enc.Encode(entry); err != nil {
			t.Fatalf("encode line %d: %v", i, err)
		}
	}
	return path
}

func claudeCodeSource(root string) []config.SourceConfig {
	return []config.SourceConfig{{Name: "claude-code", Type: "claude-code", Path: root}}
}

func sessionTurnCount(t *testing.T, database *db.DB, sessionID string) int {
	t.Helper()
	var n int
	if err := database.QueryRow(
		"SELECT COALESCE(turn_count, 0) FROM sessions WHERE id = ?", sessionID).Scan(&n); err != nil {
		t.Fatalf("read turn_count for %s: %v", sessionID, err)
	}
	return n
}

func rowsForSession(t *testing.T, database *db.DB, sessionID string) int {
	t.Helper()
	var n int
	if err := database.QueryRow(
		"SELECT COUNT(*) FROM turns WHERE session_id = ?", sessionID).Scan(&n); err != nil {
		t.Fatalf("count turns of %s: %v", sessionID, err)
	}
	return n
}

// TestSyncOfAResumedTranscriptKeepsBothSessionsTurns is #92 end to end, over
// real transcripts and the real adapter.
//
// session-earlier holds three turns. session-later is a resume of it: its file
// repeats the first two lines verbatim, uuids included, and adds two of its
// own. Keyed on the uuid alone, whichever of the two sync reached second
// deleted two of the other's turns and kept the turn_count from its own parse
// — the exact shape that cost the author's archive 31,864 turns across 201
// sessions.
func TestSyncOfAResumedTranscriptKeepsBothSessionsTurns(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	root := t.TempDir()
	shared := []transcriptLine{
		{uuid: "turn-shared-0", text: "sharedalpha", tool: "Read"},
		{uuid: "turn-shared-1", text: "sharedbeta", tool: "Bash"},
	}
	writeResumableTranscript(t, root, "-tmp-proj", sessionEarlier, append(append([]transcriptLine{}, shared...),
		transcriptLine{uuid: "turn-earlier-2", text: "earliergamma", tool: "Edit"}))
	writeResumableTranscript(t, root, "-tmp-proj", sessionLater, append(append([]transcriptLine{}, shared...),
		transcriptLine{uuid: "turn-later-2", text: "laterdelta", tool: "Grep"},
		transcriptLine{uuid: "turn-later-3", text: "laterepsilon", tool: "Glob"}))

	syncer := New(database, claudeCodeSource(root))
	stats, err := syncer.Run(context.Background())
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(stats.Errors) > 0 {
		t.Fatalf("sync reported errors: %v", stats.Errors)
	}

	for _, want := range []struct {
		session string
		turns   int
	}{
		{sessionEarlier, 3},
		{sessionLater, 4},
	} {
		if got := rowsForSession(t, database, want.session); got != want.turns {
			t.Errorf("%s holds %d turns, want %d", want.session, got, want.turns)
		}
		if got := sessionTurnCount(t, database, want.session); got != want.turns {
			t.Errorf("%s reports turn_count %d, want %d", want.session, got, want.turns)
		}
	}

	// The archive-wide figure, which is what `stats` and `orient --json`
	// report and what #91 is about.
	_, totalTurns, _, err := database.GetSessionStats()
	if err != nil {
		t.Fatalf("GetSessionStats: %v", err)
	}
	if totalTurns != 7 {
		t.Errorf("the archive reports %d turns, want 7", totalTurns)
	}
	drift, err := database.TurnCountDrift()
	if err != nil {
		t.Fatalf("TurnCountDrift: %v", err)
	}
	if drift.Drifted() {
		t.Errorf("counters disagree with the rows after a clean sync: %+v", drift)
	}

	// Each session's copy of a shared turn keeps the calls it issued.
	for _, want := range []struct {
		session string
		calls   int
	}{
		{sessionEarlier, 3},
		{sessionLater, 4},
	} {
		var n int
		if err := database.QueryRow(
			"SELECT COUNT(*) FROM tool_uses WHERE session_id = ?", want.session).Scan(&n); err != nil {
			t.Fatalf("count tool uses of %s: %v", want.session, err)
		}
		if n != want.calls {
			t.Errorf("%s holds %d tool uses, want %d", want.session, n, want.calls)
		}
	}

	assertSearchIndexSound(t, database, "after syncing a resumed transcript")

	// A re-sync must be stable: nothing changes and nothing is lost.
	resync := New(database, claudeCodeSource(root), WithFullSync(true))
	if _, err := resync.Run(context.Background()); err != nil {
		t.Fatalf("re-sync: %v", err)
	}
	if got := rowsForSession(t, database, sessionEarlier); got != 3 {
		t.Errorf("session-earlier holds %d turns after a --full re-sync, want 3", got)
	}
	if got := rowsForSession(t, database, sessionLater); got != 4 {
		t.Errorf("session-later holds %d turns after a --full re-sync, want 4", got)
	}
	assertSearchIndexSound(t, database, "after a --full re-sync")
}

// TestFullSyncReconcilesTurnCounts is step three of #92's order, and the only
// place it is allowed to happen: after every transcript on disk has been
// re-read.
//
// The drifted counter here belongs to a session whose transcript is gone, which
// is the case that cannot be repaired by re-parsing — three of the author's 201
// are in exactly that state. Its turns are genuinely lost, so the counter has
// to come down to what the archive holds rather than keep claiming turns that
// no source can supply.
func TestFullSyncReconcilesTurnCounts(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	root := t.TempDir()
	writeResumableTranscript(t, root, "-tmp-proj", sessionPresent, []transcriptLine{
		{uuid: "turn-present-0", text: "presentalpha"},
	})

	// A session whose file is no longer on disk, claiming turns it does not
	// hold. --full preserves such a session's rows, so nothing else will ever
	// correct its counter.
	if _, err := database.Exec(`INSERT INTO sessions (id, started_at, source_file, source, turn_count)
		VALUES ('cccccccc-3333-4333-8333-333333333333', ?, ?, 'claude-code', 1462)`,
		time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC),
		filepath.Join(root, "projects", "-tmp-proj", "session-vanished.jsonl")); err != nil {
		t.Fatalf("seed vanished session: %v", err)
	}
	if _, err := database.Exec(`INSERT INTO turns (id, session_id, type, timestamp, content, ordinal)
		VALUES ('turn-vanished-0', 'cccccccc-3333-4333-8333-333333333333', 'user', ?, 'survivorcontent', 0)`,
		time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("seed vanished session's survivor: %v", err)
	}

	syncer := New(database, claudeCodeSource(root), WithFullSync(true))
	stats, err := syncer.Run(context.Background())
	if err != nil {
		t.Fatalf("full sync: %v", err)
	}
	if len(stats.Errors) > 0 {
		t.Fatalf("sync reported errors: %v", stats.Errors)
	}
	if stats.TurnCountsReconciled != 1 {
		t.Errorf("sync reconciled %d session counters, want 1", stats.TurnCountsReconciled)
	}

	if got := sessionTurnCount(t, database, sessionVanished); got != 1 {
		t.Errorf("the vanished session reports %d turns, want the 1 the archive holds", got)
	}
	if got := rowsForSession(t, database, sessionVanished); got != 1 {
		t.Errorf("the vanished session's turn was dropped by --full; it holds %d rows", got)
	}
	drift, err := database.TurnCountDrift()
	if err != nil {
		t.Fatalf("TurnCountDrift: %v", err)
	}
	if drift.Drifted() {
		t.Errorf("counters still disagree after a --full sync: %+v", drift)
	}
}

// TestIncrementalSyncLeavesTurnCountsAlone is the other half of the ordering
// rule. An incremental run re-parses whatever files changed and nothing else,
// so reconciling archive-wide from it would make the counter agree with a
// database whose missing turns have not been re-read — which is what #92 warns
// against and the state migration 012 deliberately leaves visible.
func TestIncrementalSyncLeavesTurnCountsAlone(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	root := t.TempDir()
	writeResumableTranscript(t, root, "-tmp-proj", sessionPresent, []transcriptLine{
		{uuid: "turn-present-0", text: "presentalpha"},
	})

	if _, err := database.Exec(`INSERT INTO sessions (id, started_at, source_file, source, turn_count)
		VALUES ('dddddddd-4444-4444-8444-444444444444', ?, '/gone/session-untouched.jsonl', 'claude-code', 1462)`,
		time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("seed untouched session: %v", err)
	}

	syncer := New(database, claudeCodeSource(root))
	stats, err := syncer.Run(context.Background())
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if stats.TurnCountsReconciled != 0 {
		t.Errorf("an incremental sync reconciled %d counters; it must reconcile none",
			stats.TurnCountsReconciled)
	}
	if got := sessionTurnCount(t, database, sessionUntouched); got != 1462 {
		t.Errorf("the untouched session reports %d turns, want the 1462 it claimed — "+
			"the evidence has to survive an incremental run", got)
	}
}

// assertSearchIndexSound asks FTS5 itself. The cheap orphan count cannot see a
// stranded entry whose rowid a later insert reused, which is the normal outcome
// of a re-parse, so only the strict check is a verdict.
func assertSearchIndexSound(t *testing.T, database *db.DB, when string) {
	t.Helper()
	for _, table := range []string{"turns_fts", "tool_uses_fts"} {
		stmt := fmt.Sprintf("INSERT INTO %s(%s, rank) VALUES('integrity-check', 1)", table, table)
		if _, err := database.Exec(stmt); err != nil {
			t.Errorf("%s: strict integrity-check on %s: %v", when, table, err)
		}
	}
}
