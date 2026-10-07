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

// boundaryZone is the zone the bare dates in a query are parsed in while the
// boundary test runs.
//
// Pinned, because a bare before:/after: date is parsed in the caller's zone
// while the fixtures below stamp their turns in UTC — so left to the machine's
// own zone, these assertions would be answering a slightly different question
// on every developer's laptop and in CI.
//
// Deliberately not UTC. Pinning to UTC would make the two sides agree by
// construction and the test would stop saying anything about the zone at all;
// an offset is what makes it assert that a locally-parsed bound and a
// UTC-stamped row still land on the same side of a boundary. A half-hour
// offset rather than a whole-hour one because it also catches an arithmetic
// slip that rounds to the hour.
//
// A fixed offset rather than a named zone so the test does not depend on the
// machine carrying a zoneinfo database.
var boundaryZone = time.FixedZone("UTC+05:30", 5*60*60+30*60)

// pinLocalZone fixes time.Local for the duration of one test.
//
// Assigning it is safe here: nothing in this package runs its tests in
// parallel, and the original is put back before the next test starts.
func pinLocalZone(t *testing.T, loc *time.Location) {
	t.Helper()

	original := time.Local
	time.Local = loc
	t.Cleanup(func() { time.Local = original })
}

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
	pinLocalZone(t, boundaryZone)

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
// Asserted on the index, because that is where the sentinel lives, and then
// through Search, because the row now survives the trip back out. Scanning
// turns.timestamp straight into a time.Time used to fail for text no date
// format parses and take the whole query down with it — one bad row made every
// search that reached it an error, which is issue #85.
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

	searcher := New(database.DB)
	filtered, _ := matchExprs(t, searcher, Parse("unreadableneedle after:2026-09-30"))

	if got := matchCount(t, database, "turns_fts", filtered); got != 2 {
		t.Errorf("the date-filtered expression matches %d turns, want both — the dated one, and the one whose period the column could not read", got)
	}

	// And the search completes rather than erroring on the way past the row.
	// An unreadable date reads back as the zero time, which is the honest
	// answer for a timestamp nothing can parse; losing every other result to it
	// is not.
	results, err := searcher.Search(Parse("unreadableneedle after:2026-09-30"), 50)
	if err != nil {
		t.Fatalf("search across the row with the unreadable timestamp: %v", err)
	}
	var undated *Result
	for i := range results {
		if results[i].Turn.ID == "turn-undated" {
			undated = &results[i]
		}
	}
	if undated == nil {
		t.Fatalf("search returned %v, want the row with the unreadable timestamp among them", resultIDs(results))
	}
	if !undated.Turn.Timestamp.IsZero() {
		t.Errorf("the unreadable timestamp read back as %v, want the zero time", undated.Turn.Timestamp)
	}
}

// TestSearch_PayloadAttributionDrivesFromTheTurn pins the query plan of the two
// subqueries that label a payload hit with its tool name and its snippet.
//
// They are correlated to one turn, so their work ought to be bounded by that
// turn's tool uses. Written the other way round — the FTS table first and
// turn_id as a filter on its output — SQLite walks the whole all-time match set
// looking for a row belonging to this turn. That measured 1.08s of a 1.19s
// date-filtered search on the author's archive, and it does not shrink when the
// window does, so it is the half of issue #80 that pruning the index cannot
// reach.
//
// A plan assertion rather than a timing one, because the defect is invisible in
// everything except the wall clock: both shapes return identical rows, and the
// difference is 1.08s against 0.001s on a real archive and nothing at all on a
// fixture. What the plan has to show is the index on turn_id driving each
// subquery, which is what makes the lookup a seek instead of a scan. Dropping
// either the index or the CROSS JOIN that pins the join order puts the scan
// back, and SQLite picks the scan on its own when left to choose.
func TestSearch_PayloadAttributionDrivesFromTheTurn(t *testing.T) {
	fixture := setupWindowFixture(t)
	searcher := New(fixture.database.DB)

	q := Parse("windowneedle after:" + windowFirstDay)
	periods, err := searcher.periodTermsFor(q)
	if err != nil {
		t.Fatalf("resolve period terms: %v", err)
	}
	query, args := searcher.buildQuery(q, 20, periods)

	plan := queryPlan(t, fixture.database, query, args)

	// Migration 011 replaced the standalone index on turn_id with the unique
	// index that keys the table, (turn_id, turn_ordinal). turn_id leads it, so
	// it answers this lookup with the same seek; the standalone index was
	// dropped as redundant rather than kept alongside it.
	const idx = "idx_tool_uses_turn_ordinal"
	if got := strings.Count(plan, idx); got != 2 {
		t.Errorf("plan uses %s %d times, want 2 — one per attribution subquery.\nplan:\n%s", idx, got, plan)
	}
}

// queryPlan returns EXPLAIN QUERY PLAN's detail column, one line per step.
func queryPlan(t *testing.T, database *db.DB, query string, args []interface{}) string {
	t.Helper()

	rows, err := database.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var plan strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatalf("scan plan row: %v", err)
		}
		plan.WriteString(detail)
		plan.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate plan: %v", err)
	}
	return plan.String()
}
