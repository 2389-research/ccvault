// ABOUTME: Tests that a session with no project row is still reachable by search
// ABOUTME: Covers the INNER JOIN on projects that hid such sessions entirely (issue #65)

package search

import (
	"testing"
	"time"

	"github.com/2389-research/ccvault/internal/db"
	"github.com/2389-research/ccvault/pkg/models"
)

// setupProjectlessSearchDB seeds one session carrying a real project and one
// carrying NULL in project_id, each with a turn holding the same searchable
// word, so a search for that word says plainly whether the projectless row is
// returned or dropped.
//
// The NULL is written with raw SQL because UpsertSession cannot produce one —
// models.Session.ProjectID is an int64. db.MergeFrom can and documents that it
// does: a session whose project row is missing from the incoming archive is
// imported with a NULL rather than dropped, because the turns are the part
// worth keeping.
func setupProjectlessSearchDB(t *testing.T) *db.DB {
	t.Helper()

	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	p := &models.Project{Path: "/test/present", DisplayName: "present"}
	if err := database.UpsertProject(p); err != nil {
		t.Fatalf("upsert project: %v", err)
	}

	withProject := &models.Session{
		ID: "with-project", ProjectID: p.ID, StartedAt: time.Now().Add(-time.Hour),
		Model: "claude-opus-5", SourceFile: "/test/present/a.jsonl",
	}
	if err := database.UpsertSession(withProject); err != nil {
		t.Fatalf("upsert session: %v", err)
	}

	if _, err := database.Exec(
		`INSERT INTO sessions (id, project_id, started_at, ended_at, model, git_branch,
			turn_count, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
			source_file, source)
		 VALUES ('orphan-session', NULL, ?, ?, 'claude-opus-5', 'main', 1, 0, 0, 0, 0, ?, 'claude-code')`,
		time.Now().Add(-30*time.Minute), time.Now(), "/test/orphan/b.jsonl",
	); err != nil {
		t.Fatalf("insert orphan session: %v", err)
	}

	turns := []models.Turn{
		{ID: "turn-present", SessionID: "with-project", Type: "user",
			Timestamp: time.Now().Add(-time.Hour), Content: "pelican harvest in the present project"},
		{ID: "turn-orphan", SessionID: "orphan-session", Type: "user",
			Timestamp: time.Now().Add(-30 * time.Minute), Content: "pelican harvest with no project row"},
	}
	if err := database.InsertTurns(turns); err != nil {
		t.Fatalf("insert turns: %v", err)
	}

	toolUses := []models.ToolUse{
		{TurnID: "turn-orphan", SessionID: "orphan-session", ToolName: "Bash",
			Timestamp: time.Now().Add(-30 * time.Minute), InputJSON: `{"command":"pelican --harvest"}`},
	}
	if err := database.InsertToolUses(toolUses); err != nil {
		t.Fatalf("insert tool uses: %v", err)
	}

	return database
}

// TestSearch_FindsSessionWithNoProject is #65: search INNER JOINed projects,
// so a session with no project row was not ranked lower or shown without a
// project name — it was absent from every result set. A row reachable only by
// an id you already know is close to unreachable in an archive of 46,000
// sessions, and finding things is what the archive is for.
func TestSearch_FindsSessionWithNoProject(t *testing.T) {
	database := setupProjectlessSearchDB(t)
	searcher := New(database.DB)

	results, err := searcher.Search(Parse("pelican"), 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	bySession := map[string]Result{}
	for _, r := range results {
		bySession[r.SessionID] = r
	}

	if _, ok := bySession["with-project"]; !ok {
		t.Error("the session with a project is missing from the results")
	}
	orphan, ok := bySession["orphan-session"]
	if !ok {
		t.Fatalf("the projectless session is absent from search results (got %d rows: %v)",
			len(results), keysOf(bySession))
	}
	// The same fallback the session read path uses: GetSessionsPage already
	// COALESCEs the joined path to empty, so a consumer reading both surfaces
	// sees one answer for "no project" rather than two.
	if orphan.ProjectPath != "" {
		t.Errorf("projectless hit ProjectPath = %q, want empty", orphan.ProjectPath)
	}
	// Columns taken from the session, not the project, must survive the
	// outer join — the guard against "fixed the join, dropped the metadata".
	if orphan.Model != "claude-opus-5" {
		t.Errorf("projectless hit Model = %q, want claude-opus-5", orphan.Model)
	}
	if orphan.Source != "claude-code" {
		t.Errorf("projectless hit Source = %q, want claude-code", orphan.Source)
	}
}

// A projectless session's tool payloads have to be reachable too — that branch
// of the UNION joins through the same sessions/projects pair.
func TestSearch_FindsProjectlessSessionByToolPayload(t *testing.T) {
	database := setupProjectlessSearchDB(t)
	searcher := New(database.DB)

	results, err := searcher.Search(Parse("harvest"), 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	for _, r := range results {
		if r.SessionID == "orphan-session" {
			return
		}
	}
	t.Errorf("no hit in the projectless session for a word in its tool payload (got %d rows)", len(results))
}

// A project filter must not start matching projectless sessions once the join
// is outer. "Reachable without a project" and "attributed to every project"
// are different claims, and the LEFT JOIN makes the second one easy to ship
// by accident: p.path is NULL for these rows, and a NULL fails LIKE, so the
// row drops out — this pins that down rather than trusting it.
func TestSearch_ProjectFilterStillExcludesProjectlessSessions(t *testing.T) {
	database := setupProjectlessSearchDB(t)
	searcher := New(database.DB)

	results, err := searcher.Search(Parse("project:present pelican"), 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("project filter returned nothing at all")
	}
	for _, r := range results {
		if r.SessionID == "orphan-session" {
			t.Error("project:present matched a session with no project row")
		}
	}
}

func keysOf(m map[string]Result) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
