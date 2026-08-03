// ABOUTME: Tests for the ccvault TUI models — first coverage per issue #9.
// ABOUTME: Drives each model's Update/View directly against a real SQLite fixture.

package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/2389-research/ccvault/internal/db"
	"github.com/2389-research/ccvault/internal/search"
	"github.com/2389-research/ccvault/pkg/models"
)

// openTUITestDB opens a real SQLite database under a temp dir with all
// migrations applied. TUI models want *db.DB (not *sql.DB) so we use the
// package's own constructor rather than a bare in-memory helper.
func openTUITestDB(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

// seedProjectAndSessions inserts a project plus one session per given source.
// Returns the project ID and the seeded session IDs. Uses time.Time bindings
// consistently with production code to avoid modernc/sqlite datetime string
// format mismatches.
func seedProjectAndSessions(t *testing.T, d *db.DB, sources []string) (projectID int64, sessionIDs []string) {
	t.Helper()
	now := time.Now().UTC()

	res, err := d.Exec(`INSERT INTO projects
		(path, display_name, first_seen_at, last_activity_at, session_count, total_tokens, source)
		VALUES ('/tmp/tui-test', 'tui-test', ?, ?, ?, 0, ?)`,
		now, now, len(sources), sources[0])
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	projectID, _ = res.LastInsertId()

	for i, src := range sources {
		id := fmt.Sprintf("sess-%s-%d", src, i)
		_, err := d.Exec(`INSERT INTO sessions
			(id, project_id, model, git_branch, started_at, ended_at, turn_count, input_tokens, output_tokens, source_file, source)
			VALUES (?, ?, 'claude-sonnet-4', 'main', ?, ?, 2, 10, 5, '/tmp/tui-test/x.jsonl', ?)`,
			id, projectID, now, now, src)
		if err != nil {
			t.Fatalf("insert session %s: %v", id, err)
		}

		// Add one turn so search has something to find.
		_, err = d.Exec(`INSERT INTO turns
			(id, session_id, type, timestamp, content, raw_json)
			VALUES (?, ?, 'user', ?, ?, '{}')`,
			fmt.Sprintf("turn-%s-1", src), id, now,
			fmt.Sprintf("hello from %s", src))
		if err != nil {
			t.Fatalf("insert turn: %v", err)
		}
		sessionIDs = append(sessionIDs, id)
	}
	return projectID, sessionIDs
}

// --- AnalyticsModel ---

// TestAnalyticsModel_LoadTriggersRebuildOnMissingCache exercises the
// auto-rebuild branch added in PR #6 (called out by name in issue #9). With
// no parquet file in the cache dir, loadAnalytics should build one from the
// SQLite source and return a populated summary rather than an error.
func TestAnalyticsModel_LoadTriggersRebuildOnMissingCache(t *testing.T) {
	database := openTUITestDB(t)
	_, _ = seedProjectAndSessions(t, database, []string{"claude-code", "codex"})

	cacheDir := t.TempDir()
	// Guarantee no parquet exists.
	if _, err := os.Stat(filepath.Join(cacheDir, "sessions.parquet")); !os.IsNotExist(err) {
		t.Fatalf("cache dir should start empty: %v", err)
	}

	m := NewAnalyticsModel(database, cacheDir)
	msg := m.loadAnalytics()

	loaded, ok := msg.(analyticsLoadedMsg)
	if !ok {
		t.Fatalf("expected analyticsLoadedMsg, got %T: %+v", msg, msg)
	}
	if loaded.err != nil {
		t.Fatalf("loadAnalytics err: %v", loaded.err)
	}
	if loaded.summary == nil {
		t.Fatal("summary is nil after rebuild")
	}

	// Rebuild should have produced a parquet file.
	if _, err := os.Stat(filepath.Join(cacheDir, "sessions.parquet")); err != nil {
		t.Errorf("expected sessions.parquet to exist after rebuild: %v", err)
	}
}

// --- ConversationModel ---

// TestConversationModel_ViewShowsAllSubtitleParts guards the subtitle
// join behavior called out in issue #9. When Model and Source are both
// populated, both should appear in the rendered subtitle.
func TestConversationModel_ViewShowsAllSubtitleParts(t *testing.T) {
	database := openTUITestDB(t)
	m := NewConversationModel(database)
	m.session = &models.Session{
		ID:        "sess-x",
		StartedAt: time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC),
		Model:     "claude-sonnet-4",
		Source:    "codex",
	}
	m.loading = false
	m.ready = true

	view := m.View()

	for _, want := range []string{"claude-sonnet-4", "codex", "2026-08-03 10:00", "0 turns"} {
		if !strings.Contains(view, want) {
			t.Errorf("View missing %q; got:\n%s", want, view)
		}
	}
}

// TestConversationModel_ViewOmitsEmptyModelAndSource is the regression
// guard for the "no dangling separators" fix. Empty Model + empty Source
// must not leave a trailing " • " in the subtitle line.
func TestConversationModel_ViewOmitsEmptyModelAndSource(t *testing.T) {
	database := openTUITestDB(t)
	m := NewConversationModel(database)
	m.session = &models.Session{
		ID:        "sess-y",
		StartedAt: time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC),
		// Model + Source deliberately empty
	}
	m.loading = false
	m.ready = true

	view := m.View()

	// Isolate the subtitle line — it's the second line inside the header
	// (title on line 1, subtitle on line 2). Grep for a line containing
	// the date and check its shape.
	var subtitle string
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, "2026-08-03 10:00") {
			subtitle = line
			break
		}
	}
	if subtitle == "" {
		t.Fatal("subtitle line not found")
	}
	// Should be exactly "<date> • <turns>" — no trailing separator.
	if strings.HasSuffix(strings.TrimRight(subtitle, " "), "•") {
		t.Errorf("subtitle has dangling separator: %q", subtitle)
	}
	// Should NOT contain the placeholder strings we didn't set.
	for _, unexpected := range []string{"claude-sonnet", "codex"} {
		if strings.Contains(subtitle, unexpected) {
			t.Errorf("subtitle contains unexpected %q: %s", unexpected, subtitle)
		}
	}
}

// --- SessionsModel ---

// TestSessionsModel_ViewShowsSourceColumn verifies the SOURCE column on the
// sessions list renders each session's source correctly.
func TestSessionsModel_ViewShowsSourceColumn(t *testing.T) {
	database := openTUITestDB(t)
	_, _ = seedProjectAndSessions(t, database, []string{"claude-code", "codex"})

	m := NewSessionsModel(database)
	// Drive loading via the model's own message pathway rather than direct
	// state assignment — exercises the message handling too.
	msg := m.loadSessions()
	loaded, ok := msg.(sessionsLoadedMsg)
	if !ok {
		t.Fatalf("expected sessionsLoadedMsg, got %T", msg)
	}
	m.Update(loaded)
	m.SetSize(120, 30)

	view := m.View()

	if !strings.Contains(view, "SOURCE") {
		t.Errorf("view missing SOURCE header:\n%s", view)
	}
	// "claude-code" (11 chars) gets truncated to "claude-cod.." by
	// sessions.go's 10-char cap; assert on the visible prefix so the test
	// verifies the source column is populated correctly without pinning
	// exact truncation behavior.
	for _, wantPrefix := range []string{"claude-cod", "codex"} {
		if !strings.Contains(view, wantPrefix) {
			t.Errorf("view missing source prefix %q:\n%s", wantPrefix, view)
		}
	}
}

// --- ProjectsModel ---

// TestProjectsModel_ViewShowsSourceColumn verifies projects list renders
// the SOURCE column with the project's source value.
func TestProjectsModel_ViewShowsSourceColumn(t *testing.T) {
	database := openTUITestDB(t)
	// Two projects with different sources so we can spot mislabeling.
	now := time.Now().UTC()
	for _, src := range []string{"claude-code", "codex"} {
		_, err := database.Exec(`INSERT INTO projects
			(path, display_name, first_seen_at, last_activity_at, session_count, total_tokens, source)
			VALUES (?, ?, ?, ?, 3, 100, ?)`,
			"/tmp/"+src, src+"-proj", now, now, src)
		if err != nil {
			t.Fatalf("insert project: %v", err)
		}
	}

	m := NewProjectsModel(database)
	msg := m.loadProjects()
	loaded, ok := msg.(projectsLoadedMsg)
	if !ok {
		t.Fatalf("expected projectsLoadedMsg, got %T", msg)
	}
	m.Update(loaded)
	m.SetSize(120, 30)

	view := m.View()

	if !strings.Contains(view, "SOURCE") {
		t.Errorf("view missing SOURCE header:\n%s", view)
	}
	for _, src := range []string{"claude-code", "codex"} {
		if !strings.Contains(view, src) {
			t.Errorf("view missing source %q:\n%s", src, view)
		}
	}
}

// --- SearchModel ---

// TestSearchModel_ViewShowsResultsHeader exercises the search results
// rendering path once a search has returned matches. Provides basic
// coverage of the empty→loading→results state transitions.
//
// Note: the search view does NOT render a per-result source column today
// (see search.go's headerLine format). Adding one would be a feature
// change; this test verifies existing rendering only.
func TestSearchModel_ViewShowsResultsHeader(t *testing.T) {
	database := openTUITestDB(t)
	_, _ = seedProjectAndSessions(t, database, []string{"claude-code"})

	m := NewSearchModel(database)
	m.SetSize(120, 30)

	// Simulate having typed a query and received results.
	searcher := search.New(database.DB)
	results, err := searcher.Search(search.Parse("hello"), 100)
	if err != nil {
		t.Fatal(err)
	}
	m.Update(searchResultsMsg{results: results})

	view := m.View()

	if !strings.Contains(view, "results found") {
		t.Errorf("view missing results header:\n%s", view)
	}
	// Result entries should have the project path fragment rendered.
	if !strings.Contains(view, "tui-test") {
		t.Errorf("view missing seeded project path 'tui-test':\n%s", view)
	}
}
