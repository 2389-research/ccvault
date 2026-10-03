// ABOUTME: Tests that a text search reaches tool inputs and results, not just turn content.
// ABOUTME: This is the behaviour change issue #28 exists for — finding a command you ran or an error a tool printed.

package search

import (
	"strings"
	"testing"
	"time"

	"github.com/2389-research/ccvault/internal/db"
	"github.com/2389-research/ccvault/pkg/models"
	"github.com/2389-research/ccvault/pkg/toolpayload"
)

// setupPayloadSearchDB builds an archive where the searchable material is
// deliberately split: the turn's own content says nothing a caller would
// search for, and everything interesting is in a tool payload.
func setupPayloadSearchDB(t *testing.T) *db.DB {
	t.Helper()

	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	p := &models.Project{Path: "/test/payloads", DisplayName: "payloads"}
	if err := database.UpsertProject(p); err != nil {
		t.Fatalf("upsert project: %v", err)
	}
	s := &models.Session{ID: "session-p", ProjectID: p.ID, StartedAt: time.Now(), SourceFile: "/p.jsonl"}
	if err := database.UpsertSession(s); err != nil {
		t.Fatalf("upsert session: %v", err)
	}

	now := time.Now()
	turns := []models.Turn{
		{ID: "turn-cmd", SessionID: "session-p", Type: "assistant", Timestamp: now, Ordinal: 0,
			Content: "[Tool: Bash]"},
		{ID: "turn-err", SessionID: "session-p", Type: "assistant", Timestamp: now.Add(time.Second), Ordinal: 1,
			Content: "[Tool: Bash]"},
		{ID: "turn-read", SessionID: "session-p", Type: "assistant", Timestamp: now.Add(2 * time.Second), Ordinal: 2,
			Content: "[Tool: Read]"},
		{ID: "turn-plain", SessionID: "session-p", Type: "user", Timestamp: now.Add(3 * time.Second), Ordinal: 3,
			Content: "please look at the deploy script"},
	}
	if err := database.InsertTurns(turns); err != nil {
		t.Fatalf("insert turns: %v", err)
	}

	toolUses := []models.ToolUse{
		{TurnID: "turn-cmd", SessionID: "session-p", ToolName: "Bash", Timestamp: now,
			ToolUseID: "toolu_CMD", InputJSON: `{"command":"kubectl rollout restart deployment/api"}`,
			InputLength: 52, HasResult: true, ResultContent: "deployment.apps/api restarted", ResultLength: 29},
		{TurnID: "turn-err", SessionID: "session-p", ToolName: "Bash", Timestamp: now.Add(time.Second),
			ToolUseID: "toolu_ERR", InputJSON: `{"command":"go build ./..."}`,
			InputLength: 28, HasResult: true,
			ResultContent: "cannot use warning (variable of type string) as []string value", ResultLength: 61},
		{TurnID: "turn-read", SessionID: "session-p", ToolName: "Read", Timestamp: now.Add(2 * time.Second),
			ToolUseID: "toolu_READ", FilePath: "/etc/secrets.conf",
			InputJSON: `{"file_path":"/etc/secrets.conf"}`, InputLength: 33,
			HasResult: true, ResultLength: 400000, ResultOmittedReason: toolpayload.OmitBulkRead},
	}
	if err := database.InsertToolUses(toolUses); err != nil {
		t.Fatalf("insert tool uses: %v", err)
	}

	return database
}

func resultIDs(results []Result) []string {
	ids := make([]string, 0, len(results))
	for _, r := range results {
		ids = append(ids, r.Turn.ID)
	}
	return ids
}

// TestSearch_FindsACommandYouRan is the headline of issue #28. The command
// text exists nowhere in turns.content — the turn summarises itself as
// "[Tool: Bash]" — so before this change the query could not match anything.
func TestSearch_FindsACommandYouRan(t *testing.T) {
	searcher := New(setupPayloadSearchDB(t).DB)

	results, err := searcher.Search(Parse(`"kubectl rollout"`), 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results %v, want 1 (turn-cmd)", len(results), resultIDs(results))
	}
	if results[0].Turn.ID != "turn-cmd" {
		t.Errorf("matched %q, want turn-cmd", results[0].Turn.ID)
	}
}

// TestSearch_FindsAnErrorAToolPrinted covers the other half: text that only
// ever existed in a tool's output.
func TestSearch_FindsAnErrorAToolPrinted(t *testing.T) {
	searcher := New(setupPayloadSearchDB(t).DB)

	results, err := searcher.Search(Parse(`"variable of type string"`), 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 1 || results[0].Turn.ID != "turn-err" {
		t.Fatalf("got %v, want just turn-err", resultIDs(results))
	}
}

// TestSearch_SnippetShowsTheMatchingPayload covers what the caller sees. A hit
// whose match is in a tool payload has to render the payload — a snippet taken
// from turn content would show "[Tool: Bash]" and leave the user unable to see
// why the row matched.
func TestSearch_SnippetShowsTheMatchingPayload(t *testing.T) {
	searcher := New(setupPayloadSearchDB(t).DB)

	results, err := searcher.Search(Parse(`"kubectl rollout"`), 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if !strings.Contains(results[0].Snippet, "kubectl rollout restart") {
		t.Errorf("Snippet = %q, want it to show the matching command", results[0].Snippet)
	}
	if results[0].MatchedToolName != "Bash" {
		t.Errorf("MatchedToolName = %q, want Bash", results[0].MatchedToolName)
	}
}

// TestSearch_TurnContentStillMatches guards against the widening becoming a
// replacement. Ordinary conversational search has to keep working, and a turn
// with no tool uses at all has to stay findable.
func TestSearch_TurnContentStillMatches(t *testing.T) {
	searcher := New(setupPayloadSearchDB(t).DB)

	results, err := searcher.Search(Parse(`"deploy script"`), 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 1 || results[0].Turn.ID != "turn-plain" {
		t.Fatalf("got %v, want just turn-plain", resultIDs(results))
	}
	if results[0].MatchedToolName != "" {
		t.Errorf("MatchedToolName = %q, want empty for a content match", results[0].MatchedToolName)
	}
	if !strings.Contains(results[0].Snippet, "deploy script") {
		t.Errorf("Snippet = %q, want the turn content", results[0].Snippet)
	}
}

// TestSearch_OmittedContentIsNotSearchable pins the storage policy's effect on
// search. The Read above carries a 400 KB result that was never stored, so its
// body cannot be matched — but the call itself still is, through its input.
func TestSearch_OmittedContentIsNotSearchable(t *testing.T) {
	searcher := New(setupPayloadSearchDB(t).DB)

	results, err := searcher.Search(Parse(`"secrets.conf"`), 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 1 || results[0].Turn.ID != "turn-read" {
		t.Fatalf("got %v, want turn-read found through its input", resultIDs(results))
	}
}

// TestSearch_ResultsAreNotDuplicatedByMultipleToolMatches covers a turn whose
// payloads match the query more than once. The result list is turns, so the
// turn has to appear once however many of its tool uses matched.
func TestSearch_ResultsAreNotDuplicatedByMultipleToolMatches(t *testing.T) {
	database := setupPayloadSearchDB(t)

	now := time.Now()
	extra := []models.ToolUse{
		{TurnID: "turn-cmd", SessionID: "session-p", ToolName: "Grep", Timestamp: now,
			ToolUseID: "toolu_X1", InputJSON: `{"pattern":"duplicateprobe"}`, InputLength: 28},
		{TurnID: "turn-cmd", SessionID: "session-p", ToolName: "Glob", Timestamp: now,
			ToolUseID: "toolu_X2", InputJSON: `{"pattern":"duplicateprobe"}`, InputLength: 28},
	}
	if err := database.InsertToolUses(extra); err != nil {
		t.Fatalf("insert tool uses: %v", err)
	}

	results, err := New(database.DB).Search(Parse("duplicateprobe"), 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("got %d results %v, want 1 — one turn, two matching tool uses", len(results), resultIDs(results))
	}
}

// TestSearch_ToolFilterStillComposes checks the widened text match against the
// other operators, which must keep applying.
//
// tool: is session-scoped, not turn-scoped — it joins tool_uses on session_id,
// so it asks "did this session use the tool" rather than "did this turn call
// it". That predates this change and is left alone here; the filter is
// exercised with a tool the session never used, so composition is what is
// being tested rather than the scope.
func TestSearch_ToolFilterStillComposes(t *testing.T) {
	searcher := New(setupPayloadSearchDB(t).DB)

	results, err := searcher.Search(Parse(`tool:WebFetch "kubectl rollout"`), 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("got %v, want none — the session used no WebFetch", resultIDs(results))
	}

	// And the same text with a tool the session did use still comes back.
	results, err = searcher.Search(Parse(`tool:Bash "kubectl rollout"`), 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 1 || results[0].Turn.ID != "turn-cmd" {
		t.Errorf("got %v, want turn-cmd", resultIDs(results))
	}
}

// TestSearch_ProjectFilterStillComposes covers a filter that is turn-scoped
// through the session join, against a payload-only text match.
func TestSearch_ProjectFilterStillComposes(t *testing.T) {
	searcher := New(setupPayloadSearchDB(t).DB)

	if results, err := searcher.Search(Parse(`project:payloads "kubectl rollout"`), 10); err != nil {
		t.Fatalf("search: %v", err)
	} else if len(results) != 1 {
		t.Errorf("got %v, want 1 for the matching project", resultIDs(results))
	}

	if results, err := searcher.Search(Parse(`project:nosuchproject "kubectl rollout"`), 10); err != nil {
		t.Fatalf("search: %v", err)
	} else if len(results) != 0 {
		t.Errorf("got %v, want none for a non-matching project", resultIDs(results))
	}
}
