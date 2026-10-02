// ABOUTME: Tests that get_turns paginates a deterministic order and reports each
// ABOUTME: turn's ordinal, so a caller has a cursor that survives between calls.

package mcp

import (
	"testing"
	"time"

	"github.com/2389-research/ccvault/pkg/models"
)

// seedSkewedSession writes a session in position order whose turn timestamps
// both tie in pairs and run backwards across those pairs — the two shapes the
// real archive has, where 19,160 adjacent turn pairs share a timestamp exactly
// and 6,597 carry one earlier than the turn before them.
//
// Every competing ordering is therefore wrong in a way the assertions can see:
// timestamp ASC reverses the pairs, id ASC reverses outright (the ids descend
// as positions ascend), and timestamp-then-id reverses too. A test seeded with
// ties alone would not distinguish them, because SQLite is free to break a tie
// in whatever order it likes — which is the defect, not a property to assert
// against.
func seedSkewedSession(t *testing.T, s *Server, sessionID string, n int) []string {
	t.Helper()

	session := &models.Session{
		ID:         sessionID,
		StartedAt:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		SourceFile: "/tmp/" + sessionID + ".jsonl",
	}
	if err := s.db.UpsertSession(session); err != nil {
		t.Fatalf("upsert session: %v", err)
	}

	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	ids := make([]string, n)
	turns := make([]models.Turn, n)
	for i := range turns {
		ids[i] = string(rune('z'-i)) + "-turn"
		turns[i] = models.Turn{
			ID:        ids[i],
			SessionID: sessionID,
			Type:      "user",
			Timestamp: base.Add(time.Duration(-(i / 2)) * time.Second),
			Ordinal:   i,
			Content:   "turn at position " + ids[i],
		}
	}
	if err := s.db.InsertTurns(turns); err != nil {
		t.Fatalf("insert turns: %v", err)
	}
	return ids
}

func turnList(t *testing.T, result interface{}) []map[string]interface{} {
	t.Helper()
	raw, ok := resultMap(t, result)["turns"].([]map[string]interface{})
	if !ok {
		t.Fatalf("get_turns result has no turns list: %#v", resultMap(t, result))
	}
	return raw
}

// TestGetTurnsPaginatesWithoutSkippingOrRepeating is the pagination property
// that matters: walking a session a page at a time visits every turn exactly
// once, in order. Offset pagination over a sort with ties cannot promise that
// — the same hazard PR #35 hit on the sessions listing and fixed with an
// `s.id ASC` tiebreaker. get_turns had the same exposure, through a GetTurns
// that sorted on timestamp alone.
func TestGetTurnsPaginatesWithoutSkippingOrRepeating(t *testing.T) {
	s, _ := newTestServer(t)
	want := seedSkewedSession(t, s, "session-paged", 12)

	var got []string
	for offset := 0; offset < len(want)+5; offset += 5 {
		result, err := s.getTurns(map[string]interface{}{
			"session_id": "session-paged",
			"offset":     float64(offset),
			"limit":      float64(5),
		})
		if err != nil {
			t.Fatalf("getTurns(offset=%d): %v", offset, err)
		}
		for _, turn := range turnList(t, result) {
			id, ok := turn["id"].(string)
			if !ok {
				t.Fatalf("turn has no id: %#v", turn)
			}
			got = append(got, id)
		}
	}

	if len(got) != len(want) {
		t.Fatalf("paging returned %d turns across pages, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("paged position %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestGetTurnsReportsOrdinal covers the cursor a caller actually resumes
// from. offset counts rows in whatever the call returned, so it shifts under
// a type filter; the ordinal names the turn's place in the session and does
// not.
func TestGetTurnsReportsOrdinal(t *testing.T) {
	s, _ := newTestServer(t)
	seedSkewedSession(t, s, "session-ordinals", 6)

	result, err := s.getTurns(map[string]interface{}{
		"session_id": "session-ordinals",
		"offset":     float64(2),
		"limit":      float64(3),
	})
	if err != nil {
		t.Fatalf("getTurns: %v", err)
	}

	turns := turnList(t, result)
	if len(turns) != 3 {
		t.Fatalf("got %d turns, want 3", len(turns))
	}
	for i, turn := range turns {
		ordinal, ok := turn["ordinal"].(int)
		if !ok {
			t.Fatalf("turn %d has no int ordinal: %#v", i, turn)
		}
		// Page starting at offset 2, so positions 2, 3, 4.
		if want := i + 2; ordinal != want {
			t.Errorf("turn %d: ordinal = %d, want %d", i, ordinal, want)
		}
	}
}
