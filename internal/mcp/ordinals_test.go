// ABOUTME: Tests that get_turns paginates a deterministic order, reports each turn's
// ABOUTME: ordinal, and can resume from one — a cursor that survives between calls.

package mcp

import (
	"fmt"
	"slices"
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

// TestGetTurnsResumesFromAfterOrdinal covers the cursor the tool description
// advertises. Walking a session by feeding next_after_ordinal back must visit
// every turn exactly once, in order — the same property the offset walk has,
// but expressed as a position rather than a row count.
func TestGetTurnsResumesFromAfterOrdinal(t *testing.T) {
	s, _ := newTestServer(t)
	want := seedSkewedSession(t, s, "session-resume", 11)

	var got []string
	args := map[string]interface{}{"session_id": "session-resume", "limit": float64(4)}
	for range want {
		result, err := s.getTurns(args)
		if err != nil {
			t.Fatalf("getTurns(%v): %v", args, err)
		}
		m := resultMap(t, result)
		page := turnList(t, result)
		if len(page) == 0 {
			break
		}
		for _, turn := range page {
			id, ok := turn["id"].(string)
			if !ok {
				t.Fatalf("turn has no id: %#v", turn)
			}
			got = append(got, id)
		}
		next, ok := m["next_after_ordinal"].(int)
		if !ok {
			t.Fatalf("response has no next_after_ordinal: %#v", m)
		}
		if more, _ := m["has_more"].(bool); !more {
			break
		}
		args["after_ordinal"] = float64(next)
	}

	if len(got) != len(want) {
		t.Fatalf("resuming by ordinal returned %d turns, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("position %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestGetTurnsAfterOrdinalSurvivesATypeFilter is why a position beats a row
// count. offset indexes the filtered list, so the same offset means a
// different place once a filter is applied; an ordinal names the turn.
func TestGetTurnsAfterOrdinalSurvivesATypeFilter(t *testing.T) {
	s, _ := newTestServer(t)

	session := &models.Session{
		ID:         "session-filtered",
		StartedAt:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		SourceFile: "/tmp/session-filtered.jsonl",
	}
	if err := s.db.UpsertSession(session); err != nil {
		t.Fatalf("upsert session: %v", err)
	}

	// Alternating types, so a type filter halves the list and every surviving
	// turn's ordinal is twice its index in the filtered one.
	var turns []models.Turn
	for i := 0; i < 8; i++ {
		kind := "user"
		if i%2 == 1 {
			kind = "assistant"
		}
		turns = append(turns, models.Turn{
			ID:        fmt.Sprintf("f-%d", i),
			SessionID: "session-filtered",
			Type:      kind,
			Timestamp: time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC),
			Ordinal:   i,
			Content:   fmt.Sprintf("turn %d", i),
		})
	}
	if err := s.db.InsertTurns(turns); err != nil {
		t.Fatalf("insert turns: %v", err)
	}

	ids := func(page []map[string]interface{}) []string {
		out := make([]string, 0, len(page))
		for _, turn := range page {
			out = append(out, fmt.Sprint(turn["id"]))
		}
		return out
	}
	call := func(key string, value int) []string {
		t.Helper()
		result, err := s.getTurns(map[string]interface{}{
			"session_id": "session-filtered",
			"type":       "user",
			key:          float64(value),
		})
		if err != nil {
			t.Fatalf("getTurns(%s=%d): %v", key, value, err)
		}
		return ids(turnList(t, result))
	}

	// Position 3 is an assistant turn, so it is not in the filtered list at
	// all — and "after position 3" still means exactly one thing: the user
	// turns at 4 and 6.
	byOrdinal := call("after_ordinal", 3)
	if want := []string{"f-4", "f-6"}; !slices.Equal(byOrdinal, want) {
		t.Errorf("after_ordinal=3 under a user filter = %v, want %v", byOrdinal, want)
	}

	// offset 3 counts rows of the filtered list — f-0, f-2, f-4 — and lands
	// somewhere else entirely. Asserted rather than merely described, because
	// this divergence is the reason the ordinal exists as a separate cursor.
	byOffset := call("offset", 3)
	if want := []string{"f-6"}; !slices.Equal(byOffset, want) {
		t.Errorf("offset=3 under a user filter = %v, want %v", byOffset, want)
	}
	if slices.Equal(byOrdinal, byOffset) {
		t.Error("the fixture does not distinguish a position from a row count, so it proves nothing")
	}
}

// TestGetTurnsClampsHostileBounds covers the lower bound on arguments that
// arrive from an MCP caller. A negative value used to reach the slice
// expressions and panic the server; limit 0 reported has_more with no turns,
// which walks a caller into an endless loop.
func TestGetTurnsClampsHostileBounds(t *testing.T) {
	s, _ := newTestServer(t)
	seedSkewedSession(t, s, "session-hostile", 6)

	cases := []struct {
		name string
		args map[string]interface{}
	}{
		{"negative offset", map[string]interface{}{"offset": float64(-5)}},
		{"negative limit", map[string]interface{}{"limit": float64(-5)}},
		{"zero limit", map[string]interface{}{"limit": float64(0)}},
		{"both negative", map[string]interface{}{"offset": float64(-1), "limit": float64(-1)}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.args["session_id"] = "session-hostile"
			result, err := s.getTurns(tc.args)
			if err != nil {
				t.Fatalf("getTurns(%v): %v", tc.args, err)
			}
			// Falls back to the defaults, which return the whole short session.
			if n := len(turnList(t, result)); n != 6 {
				t.Errorf("returned %d turns, want all 6", n)
			}
		})
	}
}

// TestGetSessionSummaryReportsTheCursor covers the cheap way to find where a
// session ends: the summary names the last position and the turn at it,
// without the caller paging through the conversation to find out.
func TestGetSessionSummaryReportsTheCursor(t *testing.T) {
	s, _ := newTestServer(t)
	ids := seedSkewedSession(t, s, "session-summary", 7)

	result, err := s.getSessionSummary(map[string]interface{}{"session_id": "session-summary"})
	if err != nil {
		t.Fatalf("getSessionSummary: %v", err)
	}
	m := resultMap(t, result)

	if got := m["last_ordinal"]; got != 6 {
		t.Errorf("last_ordinal = %v, want 6", got)
	}
	if got := m["last_entry_uuid"]; got != ids[6] {
		t.Errorf("last_entry_uuid = %v, want %q", got, ids[6])
	}
}
