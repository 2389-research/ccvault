// ABOUTME: Tests the has:toolerror filter — finding the calls that failed, at per-call granularity (#83).
// ABOUTME: Real SQLite in a temp dir, seeded so the only thing separating the turns is whether their call failed.

package search

import (
	"slices"
	"testing"
	"time"

	"github.com/2389-research/ccvault/internal/db"
	"github.com/2389-research/ccvault/pkg/models"
)

// setupToolErrorSearchDB builds one session holding three calls: one that
// failed, one that succeeded, and one nothing answered. All three turns carry
// the same unremarkable content, so nothing but the flag can tell them apart —
// which is the point of the column.
func setupToolErrorSearchDB(t *testing.T) *db.DB {
	t.Helper()

	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	p := &models.Project{Path: "/test/errors", DisplayName: "errors"}
	if err := database.UpsertProject(p); err != nil {
		t.Fatalf("upsert project: %v", err)
	}
	s := &models.Session{ID: "session-e", ProjectID: p.ID, StartedAt: time.Now(), SourceFile: "/e.jsonl"}
	if err := database.UpsertSession(s); err != nil {
		t.Fatalf("upsert session: %v", err)
	}

	now := time.Now()
	turns := []models.Turn{
		{ID: "turn-failed", SessionID: "session-e", Type: "assistant", Timestamp: now, Ordinal: 0,
			Content: "[Tool: Bash]"},
		{ID: "turn-ok", SessionID: "session-e", Type: "assistant", Timestamp: now.Add(time.Second), Ordinal: 1,
			Content: "[Tool: Bash]"},
		{ID: "turn-unanswered", SessionID: "session-e", Type: "assistant", Timestamp: now.Add(2 * time.Second), Ordinal: 2,
			Content: "[Tool: Bash]"},
	}
	if err := database.InsertTurns(turns); err != nil {
		t.Fatalf("insert turns: %v", err)
	}

	yes, no := true, false
	toolUses := []models.ToolUse{
		{TurnID: "turn-failed", SessionID: "session-e", ToolName: "Bash", Timestamp: now,
			ToolUseID: "toolu_F", InputJSON: `{"command":"psql -c 'select 1'"}`, InputLength: 31,
			HasResult: true, ResultContent: "could not connect to server", ResultLength: 27, IsError: &yes},
		{TurnID: "turn-ok", SessionID: "session-e", ToolName: "Bash", Timestamp: now.Add(time.Second),
			ToolUseID: "toolu_O", InputJSON: `{"command":"psql -c 'select 2'"}`, InputLength: 31,
			HasResult: true, ResultContent: "could not be clearer: it worked", ResultLength: 31, IsError: &no},
		{TurnID: "turn-unanswered", SessionID: "session-e", ToolName: "Bash", Timestamp: now.Add(2 * time.Second),
			ToolUseID: "toolu_U", InputJSON: `{"command":"psql -c 'select 3'"}`, InputLength: 31,
			HasResult: false},
	}
	if err := database.InsertToolUses(toolUses); err != nil {
		t.Fatalf("insert tool uses: %v", err)
	}

	return database
}

// TestParse_HasToolError covers the operator reaching the Query, in the
// spellings someone would actually type. A filter that is silently ignored is
// worse than one that does not exist: the results look like an answer.
func TestParse_HasToolError(t *testing.T) {
	for _, value := range []string{"toolerror", "tool-error", "tool_error", "TOOLERROR"} {
		q := Parse("has:" + value)
		if !q.HasToolError {
			t.Errorf("has:%s did not set HasToolError", value)
		}
		if q.HasError {
			t.Errorf("has:%s also set HasError — the session-level flag is a different question", value)
		}
		if !q.HasFilters() {
			t.Errorf("has:%s is not reported as a filter, so an empty-text search would be rejected", value)
		}
		if q.IsEmpty() {
			t.Errorf("has:%s is reported as an empty query", value)
		}
	}

	// The session-level filter must not have acquired the per-call meaning.
	if q := Parse("has:error"); q.HasToolError {
		t.Error("has:error set HasToolError — that would silently change what an existing query returns")
	}
}

// TestSearch_HasToolErrorSelectsOnlyTheFailedCall is the question issue #83
// says the archive cannot answer. All three turns say "[Tool: Bash]" and all
// three ran psql; only one failed.
func TestSearch_HasToolErrorSelectsOnlyTheFailedCall(t *testing.T) {
	database := setupToolErrorSearchDB(t)
	s := New(database.DB)

	results, err := s.Search(Parse("has:toolerror"), 50)
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	got, want := resultIDs(results), []string{"turn-failed"}
	if !slices.Equal(got, want) {
		t.Errorf("has:toolerror returned %v, want %v", got, want)
	}
}

// TestSearch_HasToolErrorIsPerCallNotPerSession pins the granularity. All
// three turns belong to one session, so a session-scoped filter — which is
// what has:error is, and what the pre-existing tool: join does — would return
// every one of them. Getting three turns back here means the filter is
// answering "this conversation broke something" when it was asked "this call
// broke".
func TestSearch_HasToolErrorIsPerCallNotPerSession(t *testing.T) {
	database := setupToolErrorSearchDB(t)
	if _, err := database.Exec(`UPDATE sessions SET has_error = 1 WHERE id = 'session-e'`); err != nil {
		t.Fatalf("set session has_error: %v", err)
	}
	s := New(database.DB)

	perSession, err := s.Search(Parse("has:error"), 50)
	if err != nil {
		t.Fatalf("search has:error: %v", err)
	}
	if len(perSession) != 3 {
		t.Fatalf("has:error returned %d turns, want all 3 — the session is flagged", len(perSession))
	}

	perCall, err := s.Search(Parse("has:toolerror"), 50)
	if err != nil {
		t.Fatalf("search has:toolerror: %v", err)
	}
	if len(perCall) != 1 {
		t.Errorf("has:toolerror returned %d turns, want 1 — it is per call, not per session", len(perCall))
	}
}

// TestSearch_HasToolErrorCombinesWithText is the shape the issue describes an
// agent wanting: the failures, narrowed to the thing it is debugging. "could
// not" appears in both results' text, so the text alone matches two turns and
// only the flag separates them.
func TestSearch_HasToolErrorCombinesWithText(t *testing.T) {
	database := setupToolErrorSearchDB(t)
	s := New(database.DB)

	textOnly, err := s.Search(Parse(`"could not"`), 50)
	if err != nil {
		t.Fatalf("search text: %v", err)
	}
	if len(textOnly) != 2 {
		t.Fatalf(`"could not" matched %d turns, want 2 — the fixture needs both to match for this test to mean anything`, len(textOnly))
	}

	narrowed, err := s.Search(Parse(`"could not" has:toolerror`), 50)
	if err != nil {
		t.Fatalf("search text + filter: %v", err)
	}
	got, want := resultIDs(narrowed), []string{"turn-failed"}
	if !slices.Equal(got, want) {
		t.Errorf("narrowed search returned %v, want %v", got, want)
	}
}

// TestSearch_HasToolErrorExcludesUnansweredCalls holds the NULL end of the
// convention at the search surface. An interrupted call did not fail; it was
// never answered, and reporting it as a failure would send a reader looking
// for a problem that was never reported.
func TestSearch_HasToolErrorExcludesUnansweredCalls(t *testing.T) {
	database := setupToolErrorSearchDB(t)
	s := New(database.DB)

	results, err := s.Search(Parse("has:toolerror"), 50)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	for _, r := range results {
		if r.Turn.ID == "turn-unanswered" {
			t.Error("has:toolerror returned the turn whose call was never answered")
		}
	}
}
