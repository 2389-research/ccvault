// ABOUTME: Tests for issue #141 — the tool-name read behind get_turns must be bounded by the page, not the session.
// ABOUTME: Boundedness is asserted by which rows the query returns, never by how long it takes.

package mcp

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/2389-research/ccvault/internal/db"
	"github.com/2389-research/ccvault/pkg/models"
)

// seedAssistantTurnsWithToolCalls appends n assistant turns to a session,
// each recording callsPerTurn tool calls in tool_uses and naming none of them
// in raw_json. That is the shape #100 was about: codex, hex and jeff record a
// turn's calls on transcript lines of their own, so the stored rows are the
// only place the names exist. Returns the turn ids in session order.
//
// No call is marked is_error, which keeps GetFailedToolCalls out of the way of
// the tests below: it reads the same table and would otherwise have to be
// reasoned about alongside the read under test.
func seedAssistantTurnsWithToolCalls(t *testing.T, database *db.DB, sessionID string, n, callsPerTurn int) []string {
	t.Helper()

	base := time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC)
	turns := make([]models.Turn, n)
	uses := make([]models.ToolUse, 0, n*callsPerTurn)
	ids := make([]string, n)

	for i := range turns {
		id := fmt.Sprintf("%s-assistant-%d", sessionID, i)
		ids[i] = id
		turns[i] = models.Turn{
			ID:        id,
			SessionID: sessionID,
			Type:      "assistant",
			Timestamp: base.Add(time.Duration(i) * time.Second),
			Ordinal:   i + 1,
			Content:   fmt.Sprintf("assistant turn %d", i),
			// Deliberately no `message` object: raw_json names no tools, so a
			// turn's tools can only come from the stored rows.
			RawJSON: []byte(`{"type":"assistant"}`),
		}
		for c := 0; c < callsPerTurn; c++ {
			uses = append(uses, models.ToolUse{
				TurnID:    id,
				SessionID: sessionID,
				ToolName:  fmt.Sprintf("tool_%d_%d", i, c),
				Timestamp: turns[i].Timestamp,
			})
		}
	}

	if err := database.InsertTurns(turns); err != nil {
		t.Fatalf("insert %d assistant turns: %v", n, err)
	}
	if err := database.InsertToolUses(uses); err != nil {
		t.Fatalf("insert %d tool uses: %v", len(uses), err)
	}
	return ids
}

// shadowToolUsesOutsideThePage makes every tool_uses row belonging to a turn
// outside `page` unreadable: its tool_name comes back NULL, which fails the
// scan in any reader that returns it. A read bounded by the page never
// returns one and never notices; a session-wide read fails outright.
//
// That is how "this query read only the page's rows" becomes an assertion
// rather than a stopwatch. The mechanism is a TEMP view, which shadows a
// main-schema object of the same name for unqualified references — so the
// real table, its indexes and the FTS content table behind it are all left
// alone. The column list is read back from the schema so adding a column to
// tool_uses does not quietly break the shadow.
// toolUsesSelectList reads tool_uses' columns back from the schema and
// returns them as a select list with tool_name replaced by the expression
// that hides it for off-page turns. Reading the columns rather than listing
// them means adding one to tool_uses cannot quietly break the shadow.
//
// Its own function so the cursor is closed before the caller runs anything
// else: the archive pool is a single connection, and a statement left open
// across an Exec would wait for itself.
func toolUsesSelectList(t *testing.T, database *db.DB) []string {
	t.Helper()

	rows, err := database.Query(`SELECT name FROM pragma_table_info('tool_uses')`)
	if err != nil {
		t.Fatalf("read tool_uses columns: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var columns []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan column name: %v", err)
		}
		if name == "tool_name" {
			columns = append(columns, "CASE WHEN turn_id IN (SELECT turn_id FROM page_turns) THEN tool_name END AS tool_name")
			continue
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read column names: %v", err)
	}
	if len(columns) == 0 {
		t.Fatal("tool_uses reported no columns")
	}
	return columns
}

func shadowToolUsesOutsideThePage(t *testing.T, database *db.DB, page []string) {
	t.Helper()

	// The TEMP view lives on one connection. db.Open already pins the pool to
	// a single connection; saying so here keeps the test from depending on
	// that silently.
	database.SetMaxOpenConns(1)

	columns := toolUsesSelectList(t, database)

	if _, err := database.Exec(`CREATE TEMP TABLE page_turns (turn_id TEXT PRIMARY KEY)`); err != nil {
		t.Fatalf("create page_turns: %v", err)
	}
	for _, id := range page {
		if _, err := database.Exec(`INSERT INTO page_turns (turn_id) VALUES (?)`, id); err != nil {
			t.Fatalf("record page turn %s: %v", id, err)
		}
	}

	view := "CREATE TEMP VIEW tool_uses AS SELECT " + strings.Join(columns, ", ") + " FROM main.tool_uses"
	if _, err := database.Exec(view); err != nil {
		t.Fatalf("shadow tool_uses: %v", err)
	}

	// Self-check: a shadow that did not take effect would make every
	// assertion below pass for the wrong reason.
	var poisoned int
	if err := database.QueryRow(`SELECT count(*) FROM tool_uses WHERE tool_name IS NULL`).Scan(&poisoned); err != nil {
		t.Fatalf("probe the shadow: %v", err)
	}
	if poisoned == 0 {
		t.Fatal("the TEMP view did not shadow tool_uses, so nothing here would detect an off-page read")
	}
}

// TestGetTurns_ToolNameReadIsBoundedByThePage covers issue #141: get_turns
// read the whole session's tool_uses to label one page, so the cost of a page
// followed the session rather than the page.
//
// Every off-page row here is unreadable, so the handler completing at all is
// the proof that it stayed inside the page.
func TestGetTurns_ToolNameReadIsBoundedByThePage(t *testing.T) {
	s, database := newTestServer(t)
	p := seedProject(t, database, "/test/proj")
	seedSession(t, database, "session-1", p.ID)
	ids := seedAssistantTurnsWithToolCalls(t, database, "session-1", 6, 3)

	// The first two assistant turns are the page; the other four turns' rows
	// — twelve of the session's eighteen — must not be read.
	shadowToolUsesOutsideThePage(t, database, ids[:2])

	result, err := s.getTurns(map[string]interface{}{
		"session_id": "session-1",
		"type":       "assistant",
		"limit":      float64(2),
	})
	if err != nil {
		t.Fatalf("get_turns read tool_uses rows belonging to turns outside the page it was rendering (#141): %v", err)
	}

	m := resultMap(t, result)
	turns := mustField[[]map[string]interface{}](t, m, "turns")
	if len(turns) != 2 {
		t.Fatalf("got %d turns, want 2", len(turns))
	}
	for i, turn := range turns {
		tools := turn["tools"]
		names, ok := tools.([]string)
		if !ok {
			t.Fatalf("turn %d has tools %#v, want the three stored names", i, tools)
		}
		if len(names) != 3 {
			t.Errorf("turn %d has %d tool names, want 3: bounding the read dropped labels", i, len(names))
		}
	}
}

// TestGetTurns_EveryPageLabelsItsOwnTurns walks a session in pages the way an
// agent resuming from an ordinal does (#79) and checks every assistant turn
// gets its stored names. A read bounded by the page is only correct if the
// bound is the page the handler is actually returning, and an off-by-one at a
// page edge would otherwise show up as a turn that silently lost its tools.
func TestGetTurns_EveryPageLabelsItsOwnTurns(t *testing.T) {
	s, database := newTestServer(t)
	p := seedProject(t, database, "/test/proj")
	seedSession(t, database, "session-1", p.ID)
	ids := seedAssistantTurnsWithToolCalls(t, database, "session-1", 7, 2)

	labelled := map[string][]string{}
	after := -1
	for page := 0; page < len(ids); page++ {
		result, err := s.getTurns(map[string]interface{}{
			"session_id":    "session-1",
			"type":          "assistant",
			"limit":         float64(2),
			"after_ordinal": float64(after),
		})
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		m := resultMap(t, result)
		turns := mustField[[]map[string]interface{}](t, m, "turns")
		if len(turns) == 0 {
			break
		}
		for _, turn := range turns {
			id, _ := turn["id"].(string)
			names, _ := turn["tools"].([]string)
			labelled[id] = names
		}
		next, ok := m["next_after_ordinal"].(int)
		if !ok {
			t.Fatalf("page %d reported no next_after_ordinal", page)
		}
		after = next
		if _, more := m["has_more"]; !more {
			break
		}
	}

	if len(labelled) != len(ids) {
		t.Fatalf("paging covered %d of %d assistant turns", len(labelled), len(ids))
	}
	for i, id := range ids {
		want := []string{fmt.Sprintf("tool_%d_0", i), fmt.Sprintf("tool_%d_1", i)}
		got := labelled[id]
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Errorf("turn %s labelled %v, want %v", id, got, want)
		}
	}
}

// TestGetTurns_RawJSONFallbackSurvivesThePageBound pins the other half of
// #140: a turn with no stored rows still gets its tools from whatever its
// raw_json names. Scoping the stored-row query must not turn "this turn has
// no rows on this page" into "this turn called nothing".
func TestGetTurns_RawJSONFallbackSurvivesThePageBound(t *testing.T) {
	s, database := newTestServer(t)
	p := seedProject(t, database, "/test/proj")
	seedSession(t, database, "session-1", p.ID)

	// One turn with stored rows, one with none but a raw_json that names a
	// call — the claude-code shape the fallback exists for.
	seedAssistantTurnsWithToolCalls(t, database, "session-1", 1, 2)
	raw := `{"message":{"role":"assistant","content":[{"type":"tool_use","name":"Grep"}]}}`
	if err := database.InsertTurns([]models.Turn{{
		ID:        "session-1-rawonly",
		SessionID: "session-1",
		Type:      "assistant",
		Timestamp: time.Date(2026, 1, 1, 0, 2, 0, 0, time.UTC),
		Ordinal:   2,
		Content:   "raw only",
		RawJSON:   []byte(raw),
	}}); err != nil {
		t.Fatalf("insert raw-only turn: %v", err)
	}

	result, err := s.getTurns(map[string]interface{}{
		"session_id": "session-1",
		"type":       "assistant",
		"limit":      float64(10),
	})
	if err != nil {
		t.Fatalf("getTurns: %v", err)
	}
	m := resultMap(t, result)
	turns := mustField[[]map[string]interface{}](t, m, "turns")

	tools := map[string][]string{}
	for _, turn := range turns {
		id, _ := turn["id"].(string)
		names, _ := turn["tools"].([]string)
		tools[id] = names
	}
	if got := tools["session-1-assistant-0"]; len(got) != 2 {
		t.Errorf("stored-row turn labelled %v, want its two stored names", got)
	}
	if got := tools["session-1-rawonly"]; len(got) != 1 || got[0] != "Grep" {
		t.Errorf("raw-only turn labelled %v, want [Grep]: the raw_json fallback is gone", got)
	}
}

// TestGetTurns_FailedToolCallReadIsBoundedByThePage covers #157's first half
// at the handler. get_turns labels each turn with what broke in it, and read
// the whole session's failures to do it — so the cost of labelling a page
// followed the session, exactly as the tool-name read did before #141.
//
// Every off-page row here is unreadable, so the handler completing at all is
// the proof the read stayed inside the page.
func TestGetTurns_FailedToolCallReadIsBoundedByThePage(t *testing.T) {
	s, database := newTestServer(t)
	p := seedProject(t, database, "/test/proj")
	seedSession(t, database, "session-1", p.ID)
	ids := seedAssistantTurnsWithToolCalls(t, database, "session-1", 6, 3)

	// The first call of every turn failed. An UPDATE rather than a re-insert:
	// InsertToolUses numbers a turn's calls by their position in the batch and
	// replaces whatever holds that position, so seeding the failing call
	// separately would renumber the turn's other calls over themselves.
	if _, err := database.Exec(
		`UPDATE tool_uses SET is_error = 1 WHERE session_id = 'session-1' AND turn_ordinal = 0`); err != nil {
		t.Fatalf("mark failing calls: %v", err)
	}

	// The first two assistant turns are the page; the other four turns' rows
	// must not be read, failing or otherwise.
	shadowToolUsesOutsideThePage(t, database, ids[:2])

	result, err := s.getTurns(map[string]interface{}{
		"session_id": "session-1",
		"type":       "assistant",
		"limit":      float64(2),
	})
	if err != nil {
		t.Fatalf("get_turns read tool_uses rows belonging to turns outside the page it was rendering (#157): %v", err)
	}

	m := resultMap(t, result)
	turns := mustField[[]map[string]interface{}](t, m, "turns")
	if len(turns) != 2 {
		t.Fatalf("got %d turns, want 2", len(turns))
	}
	for i, turn := range turns {
		names, ok := turn["failed_tools"].([]string)
		if !ok {
			t.Fatalf("turn %d has failed_tools %#v, want the one call that failed", i, turn["failed_tools"])
		}
		if len(names) != 1 {
			t.Errorf("turn %d names %d failed tools, want 1: bounding the read changed the labels", i, len(names))
		}
	}
}

// TestGetSessionSummary_CountsEveryFailureInTheSession is the other side of
// bounding get_turns' read: the summary means the session, so it must keep
// counting failures the page-bounded reader would never see.
func TestGetSessionSummary_CountsEveryFailureInTheSession(t *testing.T) {
	s, database := newTestServer(t)
	p := seedProject(t, database, "/test/proj")
	seedSession(t, database, "session-1", p.ID)
	seedAssistantTurnsWithToolCalls(t, database, "session-1", 6, 3)
	if _, err := database.Exec(
		`UPDATE tool_uses SET is_error = 1 WHERE session_id = 'session-1' AND turn_ordinal = 0`); err != nil {
		t.Fatalf("mark failing calls: %v", err)
	}

	result, err := s.getSessionSummary(map[string]interface{}{"session_id": "session-1"})
	if err != nil {
		t.Fatalf("getSessionSummary: %v", err)
	}
	m := resultMap(t, result)
	if got := m["failed_tool_calls"]; got != 6 {
		t.Errorf("failed_tool_calls = %v, want 6 — the summary is reading a page, not the session", got)
	}
}
