// ABOUTME: Tests that a payload hit resolves to the turn in its own session, not to every copy of the uuid.
// ABOUTME: A turn's identity is (session_id, id) since migration 012, so the uuid alone no longer picks one turn.

package search

import (
	"testing"
	"time"

	"github.com/2389-research/ccvault/internal/db"
	"github.com/2389-research/ccvault/pkg/models"
)

// sharedPayloadUUID is the turn uuid two sessions hold a copy of. A resumed
// Claude Code transcript repeats the earlier session's lines verbatim, uuids
// included, so this is the ordinary case rather than a corruption.
const sharedPayloadUUID = "11111111-2222-3333-4444-555555555555"

// setupSharedTurnSearchDB builds the state a resumed transcript leaves: one
// turn uuid in two sessions, each copy issuing a call of its own.
//
// The two calls are deliberately different — a different tool, and an input
// carrying a term unique to that session alongside one term they share. A
// fixture whose two payloads look alike cannot tell a hit attributed to the
// right session from one attributed to the other, and the UNION that drives
// the search dedupes turn rowids, so matching payloads hide the fan-out
// entirely.
func setupSharedTurnSearchDB(t *testing.T) *db.DB {
	t.Helper()

	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	p := &models.Project{Path: "/test/shared", DisplayName: "shared"}
	if err := database.UpsertProject(p); err != nil {
		t.Fatalf("upsert project: %v", err)
	}

	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	for i, c := range []struct {
		sessionID string
		tool      string
		own       string
	}{
		{"session-earlier", "Bash", "earlierprobe"},
		{"session-later", "Grep", "laterprobe"},
	} {
		at := now.Add(time.Duration(i) * time.Hour)
		s := &models.Session{
			ID: c.sessionID, ProjectID: p.ID, StartedAt: at,
			SourceFile: "/" + c.sessionID + ".jsonl",
		}
		if err := database.UpsertSession(s); err != nil {
			t.Fatalf("upsert session %s: %v", c.sessionID, err)
		}
		if err := database.InsertTurns([]models.Turn{{
			ID: sharedPayloadUUID, SessionID: c.sessionID, Type: "assistant",
			Timestamp: at, Ordinal: 0,
			// Says nothing a caller would search for, which is what makes
			// this a payload hit: a turn that issued a call summarises itself
			// as "[Tool: X]".
			Content: "[Tool: " + c.tool + "]",
		}}); err != nil {
			t.Fatalf("insert %s's copy of the shared turn: %v", c.sessionID, err)
		}
		if err := database.InsertToolUses([]models.ToolUse{{
			TurnID: sharedPayloadUUID, SessionID: c.sessionID, ToolName: c.tool,
			Timestamp: at,
			ToolUseID: "toolu_" + c.sessionID,
			InputJSON: `{"command":"` + c.own + ` sharedprobe"}`,
			HasResult: true,
		}}); err != nil {
			t.Fatalf("insert %s's tool use: %v", c.sessionID, err)
		}
	}

	return database
}

// TestSearch_PayloadHitStaysInItsOwnSession is the defect the identity change
// opens up in search if the join is left alone.
//
// "earlierprobe" is in one session's tool input and nowhere else. The payload
// branch of the text UNION joins tool_uses back to turns to find the turn the
// matching call belongs to; matched on the uuid alone that join yields both
// sessions' copies, so a search for a command only one session ran returns the
// other session's turn too — a hit whose session does not contain the query
// anywhere.
func TestSearch_PayloadHitStaysInItsOwnSession(t *testing.T) {
	database := setupSharedTurnSearchDB(t)

	for _, c := range []struct{ term, session string }{
		{"earlierprobe", "session-earlier"},
		{"laterprobe", "session-later"},
	} {
		results, err := New(database.DB).Search(Parse(c.term), 20)
		if err != nil {
			t.Fatalf("search %q: %v", c.term, err)
		}
		if len(results) != 1 {
			t.Errorf("search %q returned %d rows, want 1 — only %s's call holds that term",
				c.term, len(results), c.session)
			for _, r := range results {
				t.Logf("  turn=%s session=%s tool=%s", r.Turn.ID, r.Turn.SessionID, r.MatchedToolName)
			}
			continue
		}
		if results[0].Turn.SessionID != c.session {
			t.Errorf("search %q returned %s's turn, want %s's",
				c.term, results[0].Turn.SessionID, c.session)
		}
	}
}

// TestSearch_SharedTurnIsAttributedItsOwnSessionsCall covers the other half. A
// result's tool name and snippet come from a correlated subquery over
// tool_uses with LIMIT 1, so matched on the uuid alone both copies of the turn
// are labelled with whichever call the index yielded first — one of them with
// another session's tool.
func TestSearch_SharedTurnIsAttributedItsOwnSessionsCall(t *testing.T) {
	database := setupSharedTurnSearchDB(t)

	results, err := New(database.DB).Search(Parse("sharedprobe"), 20)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("search returned %d rows, want 2 — both sessions' calls hold that term", len(results))
	}

	want := map[string]string{"session-earlier": "Bash", "session-later": "Grep"}
	seen := map[string]bool{}
	for _, r := range results {
		if r.Turn.ID != sharedPayloadUUID {
			t.Errorf("unexpected turn %q in the results", r.Turn.ID)
			continue
		}
		seen[r.Turn.SessionID] = true
		if got := r.MatchedToolName; got != want[r.Turn.SessionID] {
			t.Errorf("%s's turn is attributed tool %q, want %q",
				r.Turn.SessionID, got, want[r.Turn.SessionID])
		}
	}
	for session := range want {
		if !seen[session] {
			t.Errorf("%s's copy of the shared turn is missing from the results", session)
		}
	}
}
