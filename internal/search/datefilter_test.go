// ABOUTME: Tests that a date-filtered search prunes the index and returns exactly the same rows
// ABOUTME: The pruning assertions are counted hits; the correctness assertions are row sets

package search

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/2389-research/ccvault/internal/db"
	"github.com/2389-research/ccvault/pkg/models"
)

// windowFixture is an archive with the shape issue #80 is about: a long tail of
// matching turns outside the window and a handful inside it.
//
// Every turn and every tool payload holds the same needle, so the query's own
// selectivity cannot be what prunes — only the date can.
type windowFixture struct {
	database *db.DB
	outside  int // matching turns before the window
	inside   int // matching turns within it
}

// windowFirstDay opens the window the fixture's "inside" turns live in, and is
// what the date filter under test asks for.
const windowFirstDay = "2026-09-28"

func setupWindowFixture(t *testing.T) windowFixture {
	t.Helper()

	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	p := &models.Project{Path: "/test/window", DisplayName: "window"}
	if err := database.UpsertProject(p); err != nil {
		t.Fatalf("upsert project: %v", err)
	}
	s := &models.Session{ID: "session-w", ProjectID: p.ID, StartedAt: time.Now(), SourceFile: "/w.jsonl"}
	if err := database.UpsertSession(s); err != nil {
		t.Fatalf("upsert session: %v", err)
	}

	var turns []models.Turn
	var toolUses []models.ToolUse
	add := func(id string, ts time.Time, ordinal int) {
		turns = append(turns, models.Turn{
			ID: id, SessionID: "session-w", Type: "assistant", Timestamp: ts,
			Ordinal: ordinal, Content: "windowneedle in turn " + id,
		})
		toolUses = append(toolUses, models.ToolUse{
			TurnID: id, SessionID: "session-w", ToolName: "Bash", Timestamp: ts,
			ToolUseID: "toolu_" + id, InputJSON: `{"command":"echo windowneedle"}`, InputLength: 30,
			HasResult: true, ResultContent: "windowneedle printed", ResultLength: 20,
		})
	}

	// Eight months of matching history, one turn a day, ending well before the
	// window opens.
	outside := 0
	day := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for ; day.Before(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)); day = day.AddDate(0, 0, 1) {
		add(fmt.Sprintf("turn-old-%s", day.Format("20060102")), day, outside)
		outside++
	}

	// Three days inside the window.
	inside := 0
	for d := 28; d <= 30; d++ {
		ts := time.Date(2026, 9, d, 9, 0, 0, 0, time.UTC)
		add(fmt.Sprintf("turn-new-%02d", d), ts, outside+inside)
		inside++
	}

	if err := database.InsertTurns(turns); err != nil {
		t.Fatalf("insert turns: %v", err)
	}
	if err := database.InsertToolUses(toolUses); err != nil {
		t.Fatalf("insert tool uses: %v", err)
	}

	return windowFixture{database: database, outside: outside, inside: inside}
}

// matchExprs pulls the two MATCH expressions a query sends to the two indexes
// out of the built statement.
//
// Positional, and deliberately so: $1 and $2 are the first two parameters
// buildQuery binds for a query with text, in that order. Reading them here is
// what lets a test count what the index is asked for rather than what the
// query returns — a count of returned rows is the same before and after this
// change, so it can only prove correctness, never pruning.
func matchExprs(t *testing.T, s *Searcher, q *Query) (turnsMatch, payloadMatch string) {
	t.Helper()

	periods, err := s.periodTermsFor(q)
	if err != nil {
		t.Fatalf("resolve period terms: %v", err)
	}
	_, args := s.buildQuery(q, 20, periods)
	if len(args) < 2 {
		t.Fatalf("buildQuery bound %d args, want at least the two MATCH expressions", len(args))
	}
	turnsMatch, ok := args[0].(string)
	if !ok {
		t.Fatalf("arg 1 is %T, want the turns_fts MATCH expression", args[0])
	}
	payloadMatch, ok = args[1].(string)
	if !ok {
		t.Fatalf("arg 2 is %T, want the tool_uses_fts MATCH expression", args[1])
	}
	return turnsMatch, payloadMatch
}

func matchCount(t *testing.T, database *db.DB, table, expr string) int {
	t.Helper()

	var n int
	if err := database.QueryRow(
		fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE %s MATCH ?", table, table), expr).Scan(&n); err != nil {
		t.Fatalf("count %s for %q: %v", table, expr, err)
	}
	return n
}

// TestSearch_DateFilterPrunesInsideTheIndex is the measurement issue #80 is
// about. It is not a correctness test: the same rows come back either way.
// What it asserts is how much of the index the scan touches, which is the cost
// that was tracking archive size instead of window size.
//
// The control in the same fixture is the unfiltered query, which still sees
// every matching document. Without the period terms the two counts are equal,
// which is the state this change exists to leave behind.
func TestSearch_DateFilterPrunesInsideTheIndex(t *testing.T) {
	fixture := setupWindowFixture(t)
	searcher := New(fixture.database.DB)

	allTime, allTimePayload := matchExprs(t, searcher, Parse("windowneedle"))
	filtered, filteredPayload := matchExprs(t, searcher, Parse("windowneedle after:"+windowFirstDay))

	total := fixture.outside + fixture.inside
	if got := matchCount(t, fixture.database, "turns_fts", allTime); got != total {
		t.Fatalf("the unfiltered query matches %d turns, want all %d — the fixture is not what the rest of this test assumes", got, total)
	}
	if got := matchCount(t, fixture.database, "tool_uses_fts", allTimePayload); got != total {
		t.Fatalf("the unfiltered query matches %d payloads, want all %d", got, total)
	}

	// after:2026-09-28 compiles to the three days the window covers, and the
	// newest row in the archive is what bounds it above. So the index is asked
	// for the window's own documents and nothing else.
	wantIndexed := fixture.inside
	if got := matchCount(t, fixture.database, "turns_fts", filtered); got != wantIndexed {
		t.Errorf("the date-filtered query asks turns_fts for %d documents, want %d — the window's own days, not the archive's %d",
			got, wantIndexed, total)
	}
	if got := matchCount(t, fixture.database, "tool_uses_fts", filteredPayload); got != wantIndexed {
		t.Errorf("the date-filtered query asks tool_uses_fts for %d documents, want %d of %d",
			got, wantIndexed, total)
	}
}

// TestSearch_DateFilterScopesItsTermsToThePeriodColumn pins the two halves of
// the collision guarantee onto the expression the searcher actually builds.
func TestSearch_DateFilterScopesItsTermsToThePeriodColumn(t *testing.T) {
	fixture := setupWindowFixture(t)
	searcher := New(fixture.database.DB)

	turnsMatch, payloadMatch := matchExprs(t, searcher, Parse("windowneedle after:"+windowFirstDay))

	for _, expr := range []string{turnsMatch, payloadMatch} {
		if want := "{" + periodColumn + "}"; !strings.Contains(expr, want) {
			t.Errorf("expression %q does not scope its period terms to %s", expr, want)
		}
	}
	if !strings.Contains(turnsMatch, "{content}") {
		t.Errorf("turns_fts expression %q does not scope the caller's text to the content column", turnsMatch)
	}
	if !strings.Contains(payloadMatch, "{input_json result_content}") {
		t.Errorf("tool_uses_fts expression %q does not scope the caller's text to the payload columns", payloadMatch)
	}

	// And the scoping has the effect it is there for: searching the literal
	// token finds the turns whose text holds it, not the month.
	if err := fixture.database.InsertTurns([]models.Turn{{
		ID: "turn-literal", SessionID: "session-w", Type: "user", Ordinal: 9999,
		Timestamp: time.Date(2026, 2, 2, 12, 0, 0, 0, time.UTC),
		Content:   "the september token is ccvym202609",
	}}); err != nil {
		t.Fatalf("insert literal-token turn: %v", err)
	}

	results, err := searcher.Search(Parse("ccvym202609"), 50)
	if err != nil {
		t.Fatalf("search the literal token: %v", err)
	}
	if len(results) != 1 || results[0].Turn.ID != "turn-literal" {
		t.Errorf("searching the literal period token returned %v, want just the turn whose content holds it",
			resultIDs(results))
	}
}

// TestSearch_DateFilterReturnsTheSameRowsAtPeriodBoundaries is the correctness
// half, and it is written to pass with or without the period terms. A
// performance change that returns different rows is a bug, and the rows most
// at risk are the ones at the edge of a token: a turn at the last instant of a
// month and one at the first instant of the next belong to different tokens
// but can belong to the same window.
func TestSearch_DateFilterReturnsTheSameRowsAtPeriodBoundaries(t *testing.T) {
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = database.Close() }()

	p := &models.Project{Path: "/test/boundary", DisplayName: "boundary"}
	if err := database.UpsertProject(p); err != nil {
		t.Fatalf("upsert project: %v", err)
	}
	s := &models.Session{ID: "session-b", ProjectID: p.ID, StartedAt: time.Now(), SourceFile: "/b.jsonl"}
	if err := database.UpsertSession(s); err != nil {
		t.Fatalf("upsert session: %v", err)
	}

	// Four instants: the last of August, the last of September to the
	// nanosecond, the first of October, and midday on the first of October.
	instants := map[string]time.Time{
		"turn-aug":       time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC),
		"turn-sep-mid":   time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC),
		"turn-sep-last":  time.Date(2026, 9, 30, 23, 59, 59, 999999999, time.UTC),
		"turn-oct-first": time.Date(2026, 10, 1, 0, 0, 0, 1, time.UTC),
		"turn-oct-noon":  time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
	}
	var turns []models.Turn
	var toolUses []models.ToolUse
	ordinal := 0
	for _, id := range []string{"turn-aug", "turn-sep-mid", "turn-sep-last", "turn-oct-first", "turn-oct-noon"} {
		turns = append(turns, models.Turn{
			ID: id, SessionID: "session-b", Type: "assistant", Timestamp: instants[id],
			Ordinal: ordinal, Content: "boundaryneedle " + id,
		})
		toolUses = append(toolUses, models.ToolUse{
			TurnID: id, SessionID: "session-b", ToolName: "Bash", Timestamp: instants[id],
			ToolUseID: "toolu_" + id, InputJSON: `{"command":"echo payloadneedle"}`, InputLength: 31,
		})
		ordinal++
	}
	if err := database.InsertTurns(turns); err != nil {
		t.Fatalf("insert turns: %v", err)
	}
	if err := database.InsertToolUses(toolUses); err != nil {
		t.Fatalf("insert tool uses: %v", err)
	}

	searcher := New(database.DB)

	cases := []struct {
		query string
		want  []string
	}{
		{
			// The month boundary from below. The September turn is 1 ns short
			// of October and must not come back; the October turn is 1 ns past
			// midnight and must.
			query: "boundaryneedle after:2026-10-01",
			want:  []string{"turn-oct-noon", "turn-oct-first"},
		},
		{
			// The same boundary from above.
			query: "boundaryneedle before:2026-10-01",
			want:  []string{"turn-sep-last", "turn-sep-mid", "turn-aug"},
		},
		{
			// A window whose ends are both inside partial days, spanning the
			// month boundary.
			query: "boundaryneedle after:2026-09-30 before:2026-10-02",
			want:  []string{"turn-oct-noon", "turn-oct-first", "turn-sep-last"},
		},
		{
			// A single day.
			query: "boundaryneedle after:2026-09-30 before:2026-10-01",
			want:  []string{"turn-sep-last"},
		},
		{
			// A whole month, which compiles to a single month term. Its last
			// instant is outside the answer: before: is exclusive at midnight,
			// so turn-sep-last at 23:59:59.999999999 on the 30th is not before
			// the 30th. That is the predicate's own long-standing reading of
			// before:, and the period terms have to agree with the predicate
			// rather than with what the operator sounds like.
			query: "boundaryneedle after:2026-09-01 before:2026-09-30",
			want:  []string{"turn-sep-mid"},
		},
		{
			// Nothing in range.
			query: "boundaryneedle after:2026-10-02",
			want:  nil,
		},
		{
			// The same boundaries reached through a tool payload rather than
			// turn content, which is the branch the period token had to be
			// added to tool_uses for.
			query: "payloadneedle after:2026-10-01",
			want:  []string{"turn-oct-noon", "turn-oct-first"},
		},
		{
			query: "payloadneedle after:2026-09-30 before:2026-10-02",
			want:  []string{"turn-oct-noon", "turn-oct-first", "turn-sep-last"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			results, err := searcher.Search(Parse(tc.query), 50)
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if fmt.Sprint(resultIDs(results)) != fmt.Sprint(tc.want) {
				t.Errorf("got %v, want %v", resultIDs(results), tc.want)
			}
		})
	}
}

// TestSearch_DateFilterKeepsAnUnreadableTimestamp covers the sentinel. A turn
// whose stored timestamp is not a date the period column can read must stay a
// candidate for a date-filtered search, so the timestamp predicate decides it
// exactly as it did before the period column existed. Dropping it inside the
// index would be a wrong answer rather than a slow one.
//
// Asserted on the index rather than through Search, because Search cannot
// return such a row at all: scanning turns.timestamp into a time.Time fails
// for text no date format parses, and the whole query errors with it. That is
// pre-existing and left alone here — and it is also why the sentinel has to be
// checked where it lives.
func TestSearch_DateFilterKeepsAnUnreadableTimestamp(t *testing.T) {
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = database.Close() }()

	p := &models.Project{Path: "/test/unreadable", DisplayName: "unreadable"}
	if err := database.UpsertProject(p); err != nil {
		t.Fatalf("upsert project: %v", err)
	}
	s := &models.Session{ID: "session-u", ProjectID: p.ID, StartedAt: time.Now(), SourceFile: "/u.jsonl"}
	if err := database.UpsertSession(s); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	// A readable turn, so the filter has an archive date range to compile
	// against at all.
	if err := database.InsertTurns([]models.Turn{{
		ID: "turn-dated", SessionID: "session-u", Type: "user", Ordinal: 0,
		Timestamp: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
		Content:   "unreadableneedle dated",
	}}); err != nil {
		t.Fatalf("insert dated turn: %v", err)
	}
	// Written with raw SQL because the Go layer cannot produce a timestamp
	// this shape.
	if _, err := database.Exec(
		`INSERT INTO turns (id, session_id, type, timestamp, content, ordinal)
		 VALUES ('turn-undated', 'session-u', 'user', 'zzz-not-a-date', 'unreadableneedle undated', 1)`,
	); err != nil {
		t.Fatalf("insert undated turn: %v", err)
	}

	filtered, _ := matchExprs(t, New(database.DB), Parse("unreadableneedle after:2026-09-30"))

	if got := matchCount(t, database, "turns_fts", filtered); got != 2 {
		t.Errorf("the date-filtered expression matches %d turns, want both — the dated one, and the one whose period the column could not read", got)
	}
}
