// ABOUTME: Tests that every DATETIME column holds an instant SQLite can read, on write and after migration 014
// ABOUTME: The write-path half is the regression guard; the migration half is #33 against a legacy fixture

package db

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/2389-research/ccvault/pkg/models"
)

// datetimeColumns names every column the schema declares DATETIME, read out of
// the schema rather than listed here.
//
// Discovered rather than hardcoded for the reason listFTSTables is: a DATETIME
// column added by a later migration is covered by everything that covers
// DATETIME columns, instead of by whichever call sites someone remembered.
func datetimeColumns(t *testing.T, database *DB) map[string][]string {
	t.Helper()

	var tables []string
	eachRow(t, database,
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`,
		func(rows *sql.Rows) {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatalf("scan table name: %v", err)
			}
			tables = append(tables, name)
		})

	found := make(map[string][]string)
	for _, table := range tables {
		eachRow(t, database, "PRAGMA table_info("+quoteSQLiteIdent(table)+")", func(rows *sql.Rows) {
			var cid, notNull, pk int
			var name, colType string
			var dflt sql.NullString
			if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
				t.Fatalf("scan table_info for %s: %v", table, err)
			}
			if strings.EqualFold(colType, "DATETIME") {
				found[table] = append(found[table], name)
			}
		})
	}
	return found
}

// eachRow runs a query and hands each row to visit, closing the cursor on the
// way out. Factored so the Close can be deferred: these helpers read several
// tables in a loop, and a deferred Close inside a loop body holds every cursor
// open until the function returns.
func eachRow(t *testing.T, database *DB, query string, visit func(rows *sql.Rows)) {
	t.Helper()

	rows, err := database.Query(query)
	if err != nil {
		t.Fatalf("query (%s): %v", query, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		visit(rows)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate (%s): %v", query, err)
	}
}

// seedEveryDatetimeColumn writes one row into every table that has a DATETIME
// column, through the public write path each of those columns actually uses.
func seedEveryDatetimeColumn(t *testing.T, database *DB, instant time.Time) {
	t.Helper()

	project := &models.Project{
		Path:           "/Users/someone/work/ccvault",
		DisplayName:    "ccvault",
		FirstSeenAt:    instant,
		LastActivityAt: instant.Add(time.Hour),
		SessionCount:   1,
	}
	if err := database.UpsertProject(project); err != nil {
		t.Fatalf("upsert project: %v", err)
	}

	session := &models.Session{
		ID:         "sess-1",
		ProjectID:  project.ID,
		StartedAt:  instant,
		EndedAt:    instant.Add(time.Hour),
		Model:      "claude-opus-5",
		TurnCount:  1,
		SourceFile: "/Users/someone/.claude/projects/x/sess-1.jsonl",
	}
	if err := database.UpsertSession(session); err != nil {
		t.Fatalf("upsert session: %v", err)
	}

	if err := database.InsertTurns([]models.Turn{{
		ID: "turn-1", SessionID: session.ID, Type: "assistant",
		Timestamp: instant, Ordinal: 0, Content: "a turn to index",
	}}); err != nil {
		t.Fatalf("insert turn: %v", err)
	}

	if err := database.InsertToolUses([]models.ToolUse{{
		TurnID: "turn-1", SessionID: session.ID, ToolName: "Bash",
		Timestamp: instant, ToolUseID: "toolu_1",
		InputJSON: `{"command":"echo hi"}`, InputLength: 20,
		HasResult: true, ResultContent: "hi", ResultLength: 2,
	}}); err != nil {
		t.Fatalf("insert tool use: %v", err)
	}

	if err := database.UpsertSourceFileMtime(session.SourceFile, instant, "claude-code"); err != nil {
		t.Fatalf("upsert source file mtime: %v", err)
	}

	// UpsertProjectTx is the one write path here that is a second copy of its
	// statement rather than a wrapper around a shared one, so it is the one
	// that can drift. The others — UpsertSessionTx, InsertTurnsTx,
	// InsertToolUsesTx, UpsertSourceFileMtimeTx — all run the same code as the
	// calls above.
	if err := database.WithTx(func(tx *sql.Tx) error {
		return database.UpsertProjectTx(tx, &models.Project{
			Path:           "/Users/someone/work/other",
			DisplayName:    "other",
			FirstSeenAt:    instant,
			LastActivityAt: instant.Add(time.Hour),
		})
	}); err != nil {
		t.Fatalf("upsert project in a transaction: %v", err)
	}
}

// TestDatetimeColumns_WritePathStoresReadableInstants is the test that stops
// #33 coming back. A migration that converts the archive and a write path that
// keeps producing time.Time.String() leaves the next sync to undo the
// migration one row at a time.
func TestDatetimeColumns_WritePathStoresReadableInstants(t *testing.T) {
	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = database.Close() }()

	// Deliberately not UTC: the write path has to normalise, not just reformat.
	chicago := time.FixedZone("CDT", -5*3600)
	seedEveryDatetimeColumn(t, database, time.Date(2026, 8, 14, 15, 15, 0, 134000000, chicago))

	columns := datetimeColumns(t, database)
	if len(columns) == 0 {
		t.Fatal("no DATETIME columns found; the discovery is broken, not the schema")
	}

	for table, cols := range columns {
		for _, col := range cols {
			quoted := quoteSQLiteIdent(table) + "." + quoteSQLiteIdent(col)

			var total int
			if err := database.QueryRow(
				fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE %s IS NOT NULL",
					quoteSQLiteIdent(table), quoteSQLiteIdent(col)),
			).Scan(&total); err != nil {
				t.Fatalf("count %s: %v", quoted, err)
			}
			if total == 0 {
				t.Errorf("%s has no rows to check; the fixture does not exercise it", quoted)
				continue
			}

			var unreadable int
			if err := database.QueryRow(fmt.Sprintf(
				"SELECT COUNT(*) FROM %s WHERE %s IS NOT NULL AND date(%s) IS NULL",
				quoteSQLiteIdent(table), quoteSQLiteIdent(col), quoteSQLiteIdent(col)),
			).Scan(&unreadable); err != nil {
				t.Fatalf("count unreadable %s: %v", quoted, err)
			}
			if unreadable != 0 {
				var sample string
				_ = database.QueryRow(fmt.Sprintf(
					"SELECT CAST(%s AS TEXT) FROM %s WHERE date(%s) IS NULL LIMIT 1",
					quoteSQLiteIdent(col), quoteSQLiteIdent(table), quoteSQLiteIdent(col)),
				).Scan(&sample)
				t.Errorf("%s: %d of %d rows are not a date SQLite can read, e.g. %q",
					quoted, unreadable, total, sample)
				continue
			}

			var offLayout int
			if err := database.QueryRow(fmt.Sprintf(
				"SELECT COUNT(*) FROM %s WHERE %s IS NOT NULL AND %s NOT GLOB ?",
				quoteSQLiteIdent(table), quoteSQLiteIdent(col), quoteSQLiteIdent(col)),
				canonicalTimestampGlob).Scan(&offLayout); err != nil {
				t.Fatalf("count off-layout %s: %v", quoted, err)
			}
			if offLayout != 0 {
				var sample string
				_ = database.QueryRow(fmt.Sprintf(
					"SELECT CAST(%s AS TEXT) FROM %s WHERE %s NOT GLOB ? LIMIT 1",
					quoteSQLiteIdent(col), quoteSQLiteIdent(table), quoteSQLiteIdent(col)),
					canonicalTimestampGlob).Scan(&sample)
				t.Errorf("%s: %d of %d rows are not in the canonical layout, e.g. %q",
					quoted, offLayout, total, sample)
			}
		}
	}
}

// canonicalTimestampGlob matches the canonical layout and nothing shorter or
// longer. Kept beside the test rather than exported, because production code
// should be producing the layout, not pattern-matching it.
const canonicalTimestampGlob = "[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T" +
	"[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z"

// legacyRendering is what every DATETIME column held before migration 014:
// Go's time.Time.String(), which SQLite's date functions cannot parse.
func legacyRendering(instant time.Time) string { return instant.String() }

// storedText reads a DATETIME column as the bytes on disk.
//
// The CAST is what makes this say anything. modernc's driver parses any column
// whose declared type is DATE, DATETIME or TIMESTAMP into a time.Time, and
// scanning that into a string re-renders it as RFC3339Nano — so a plain
// `SELECT timestamp` scanned into a string reports the *driver's* rendering of
// the value, not the archive's, and a migration that had done nothing at all
// would look like it had worked. The note on issue #33 says it plainly: assert
// on what sqlite3 sees, not only on what Go scans back. A CAST expression has
// no declared column type, so the driver hands the text through untouched.
func storedText(t *testing.T, database *DB, query string, args ...any) string {
	t.Helper()

	var got string
	if err := database.QueryRow(query, args...).Scan(&got); err != nil {
		t.Fatalf("read stored text (%s): %v", query, err)
	}
	return got
}

// rewindTo rolls schema_version back so RunMigrations re-applies everything
// above it against whatever the test has seeded. The pattern migrator_test
// established for migration 005.
func rewindTo(t *testing.T, database *DB, version int) {
	t.Helper()
	if _, err := database.Exec("DELETE FROM schema_version WHERE version > ?", version); err != nil {
		t.Fatalf("rewind schema_version to %d: %v", version, err)
	}
}

// TestMigration014_ConvertsALegacyArchive is #33 against a fixture seeded the
// way the author's archive actually is: time.Time.String() in every DATETIME
// column, UTC on the conversation columns and a local offset with a monotonic
// tail on the bookkeeping ones.
func TestMigration014_ConvertsALegacyArchive(t *testing.T) {
	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = database.Close() }()

	utc := time.Date(2026, 8, 14, 20, 15, 0, 134000000, time.UTC)
	seedEveryDatetimeColumn(t, database, utc)

	// Put the archive back the way migration 014 will find it.
	rewindTo(t, database, 13)

	chicago := time.FixedZone("CDT", -5*3600)
	mtime := time.Date(2026, 8, 25, 12, 25, 12, 952000000, chicago)
	// A monotonic tail is what time.Now().String() produced for synced_at.
	syncedAt := legacyRendering(time.Date(2026, 9, 5, 9, 5, 43, 170121000, chicago)) + " m=+48.861813543"

	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{"UPDATE projects SET first_seen_at = ?, last_activity_at = ?",
			[]any{legacyRendering(utc), legacyRendering(utc.Add(time.Hour))}},
		{"UPDATE sessions SET started_at = ?, ended_at = ?, source_mtime = ?",
			[]any{legacyRendering(utc), legacyRendering(utc.Add(time.Hour)), syncedAt}},
		{"UPDATE turns SET timestamp = ?", []any{legacyRendering(utc)}},
		{"UPDATE tool_uses SET timestamp = ?", []any{legacyRendering(utc)}},
		{"UPDATE source_files SET mtime = ?, synced_at = ?",
			[]any{legacyRendering(mtime), syncedAt}},
	} {
		if _, err := database.Exec(stmt.sql, stmt.args...); err != nil {
			t.Fatalf("seed legacy value (%s): %v", stmt.sql, err)
		}
	}

	// What the index holds before the conversion. It must hold the same after.
	periodsBefore := indexedPeriods(t, database)

	if err := RunMigrations(database.DB); err != nil {
		t.Fatalf("re-run migrations: %v", err)
	}

	// 1. Every DATETIME column is now something SQLite can read.
	for table, cols := range datetimeColumns(t, database) {
		for _, col := range cols {
			var unreadable int
			if err := database.QueryRow(fmt.Sprintf(
				"SELECT COUNT(*) FROM %s WHERE %s IS NOT NULL AND date(%s) IS NULL",
				quoteSQLiteIdent(table), quoteSQLiteIdent(col), quoteSQLiteIdent(col)),
			).Scan(&unreadable); err != nil {
				t.Fatalf("count unreadable %s.%s: %v", table, col, err)
			}
			if unreadable != 0 {
				t.Errorf("%s.%s still holds %d values SQLite cannot read", table, col, unreadable)
			}
		}
	}

	// 2. The instants survived, offsets and all.
	gotTurn := storedText(t, database, "SELECT CAST(timestamp AS TEXT) FROM turns WHERE id = 'turn-1'")
	if want := FormatTimestamp(utc); gotTurn != want {
		t.Errorf("turns.timestamp = %q, want %q", gotTurn, want)
	}

	gotMtime := storedText(t, database, "SELECT CAST(mtime AS TEXT) FROM source_files")
	if want := FormatTimestamp(mtime); gotMtime != want {
		t.Errorf("source_files.mtime = %q, want %q — the -0500 offset was not applied", gotMtime, want)
	}

	gotSynced := storedText(t, database, "SELECT CAST(synced_at AS TEXT) FROM source_files")
	if want := FormatTimestamp(time.Date(2026, 9, 5, 9, 5, 43, 170121000, chicago)); gotSynced != want {
		t.Errorf("source_files.synced_at = %q, want %q — the monotonic tail was not dropped", gotSynced, want)
	}

	// 3. SQLite's date functions all work on the converted column.
	var date, dt, ym any
	var julian any
	if err := database.QueryRow(
		`SELECT date(timestamp), datetime(timestamp), strftime('%Y-%m', timestamp), julianday(timestamp)
		 FROM turns WHERE id = 'turn-1'`).Scan(&date, &dt, &ym, &julian); err != nil {
		t.Fatalf("date functions on turns.timestamp: %v", err)
	}
	if date != "2026-08-14" || ym != "2026-08" {
		t.Errorf("date()=%v strftime()=%v, want 2026-08-14 / 2026-08", date, ym)
	}
	if dt == nil || julian == nil {
		t.Errorf("datetime()=%v julianday()=%v, want non-NULL", dt, julian)
	}

	// 4. search_period is byte-identical, so both FTS indexes still describe
	//    the rows they described before.
	if after := indexedPeriods(t, database); !equalPeriods(periodsBefore, after) {
		t.Errorf("search_period changed across the migration:\n before: %v\n after:  %v", periodsBefore, after)
	}

	// 5. And FTS5 itself agrees, which is the only verdict that counts.
	report, err := database.CheckFTSStrict()
	if err != nil {
		t.Fatalf("run strict FTS check: %v", err)
	}
	if !report.OK() {
		t.Errorf("strict FTS check failed after the migration: %v", report.Err())
	}
}

// indexedPeriods reads the generated period column off both indexed tables.
func indexedPeriods(t *testing.T, database *DB) map[string][]string {
	t.Helper()

	out := make(map[string][]string)
	for _, table := range []string{"turns", "tool_uses"} {
		var got []string
		eachRow(t, database, fmt.Sprintf("SELECT id, search_period FROM %s ORDER BY id", table),
			func(rows *sql.Rows) {
				var id, period string
				if err := rows.Scan(&id, &period); err != nil {
					t.Fatalf("scan search_period: %v", err)
				}
				got = append(got, id+"="+period)
			})
		sort.Strings(got)
		out[table] = got
	}
	return out
}

func equalPeriods(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for table, want := range a {
		got, ok := b[table]
		if !ok || len(got) != len(want) {
			return false
		}
		for i := range want {
			if want[i] != got[i] {
				return false
			}
		}
	}
	return true
}

// TestMigration014_MovesTheDayWhenTheOffsetDoes covers the case the author's
// archive does not have and some other archive might: a conversation timestamp
// written by a machine at a non-UTC offset, where converting to UTC lands on a
// different calendar day.
//
// That changes search_period, which the FTS indexes hold — so unlike the UTC
// rows, these have to go through the update triggers. Getting this wrong would
// not fail loudly; it would leave the index describing the old day, and only
// FTS5's strict check would see it.
func TestMigration014_MovesTheDayWhenTheOffsetDoes(t *testing.T) {
	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = database.Close() }()

	seedEveryDatetimeColumn(t, database, time.Date(2026, 8, 14, 20, 15, 0, 0, time.UTC))
	rewindTo(t, database, 13)

	// 22:30 on the 14th at -05:00 is 03:30 on the 15th in UTC.
	chicago := time.FixedZone("CDT", -5*3600)
	instant := time.Date(2026, 8, 14, 22, 30, 0, 500000000, chicago)
	if _, err := database.Exec("UPDATE turns SET timestamp = ?", legacyRendering(instant)); err != nil {
		t.Fatalf("seed offset turn: %v", err)
	}
	if _, err := database.Exec("UPDATE tool_uses SET timestamp = ?", legacyRendering(instant)); err != nil {
		t.Fatalf("seed offset tool use: %v", err)
	}

	if err := RunMigrations(database.DB); err != nil {
		t.Fatalf("re-run migrations: %v", err)
	}

	got := storedText(t, database, "SELECT CAST(timestamp AS TEXT) FROM turns WHERE id = 'turn-1'")
	period := storedText(t, database, "SELECT search_period FROM turns WHERE id = 'turn-1'")
	if want := FormatTimestamp(instant); got != want {
		t.Errorf("turns.timestamp = %q, want %q", got, want)
	}
	if want := "ccvd20260815 ccvym202608"; period != want {
		t.Errorf("search_period = %q, want %q", period, want)
	}

	// The index has to agree with the column it describes.
	report, err := database.CheckFTSStrict()
	if err != nil {
		t.Fatalf("run strict FTS check: %v", err)
	}
	if !report.OK() {
		t.Errorf("strict FTS check failed after a day-moving conversion: %v", report.Err())
	}
}

// TestMigration014_LeavesAValueItCannotReadAlone is the blast-radius guard.
//
// The conversion is substr arithmetic over an assumed shape, and the only
// archive it was written against is one where every value has that shape. A
// value that does not — a hand-repaired row, something another tool wrote —
// must come out the other side unchanged, not NULL and not scrambled.
//
// The probes are the shapes that get past the cheap half of the guard.
// "whenever" is not a date at all. The other two have no space before the
// offset, which is what the `instr(…) > 0` clause is for, and they fail
// differently: without the clause the first is rewritten by luck, because its
// digits happen to line up, and the second runs the offset arithmetic over its
// fractional digits and lands on NULL — which is what COALESCE is for. One
// outcome for all three: the row is left exactly as it was found.
func TestMigration014_LeavesAValueItCannotReadAlone(t *testing.T) {
	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = database.Close() }()

	seedEveryDatetimeColumn(t, database, time.Date(2026, 8, 14, 20, 15, 0, 0, time.UTC))
	rewindTo(t, database, 13)

	for _, odd := range []string{"2026-01-14 20:15:00+0000", "2026-01-14 20:15:00.134+0000", "whenever"} {
		t.Run(odd, func(t *testing.T) {
			if _, err := database.Exec("UPDATE source_files SET synced_at = ?", odd); err != nil {
				t.Fatalf("seed odd value: %v", err)
			}
			rewindTo(t, database, 13)
			if err := RunMigrations(database.DB); err != nil {
				t.Fatalf("migrate over an unreadable value: %v", err)
			}
			if got := storedText(t, database, "SELECT CAST(synced_at AS TEXT) FROM source_files"); got != odd {
				t.Errorf("synced_at = %q, want it left as %q — the conversion must not touch a shape it does not know", got, odd)
			}
		})
	}
}

// TestMigration014_IsIdempotent guards the replay path: anything that rewinds
// schema_version re-runs 014 over an archive it has already converted.
func TestMigration014_IsIdempotent(t *testing.T) {
	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = database.Close() }()

	instant := time.Date(2026, 8, 14, 20, 15, 0, 134000000, time.UTC)
	seedEveryDatetimeColumn(t, database, instant)

	before := allDatetimeValues(t, database)
	rewindTo(t, database, 13)
	if err := RunMigrations(database.DB); err != nil {
		t.Fatalf("replay migrations: %v", err)
	}
	after := allDatetimeValues(t, database)

	if strings.Join(before, "\n") != strings.Join(after, "\n") {
		t.Errorf("replaying migration 014 changed already-converted values:\n before: %v\n after:  %v", before, after)
	}
}

func allDatetimeValues(t *testing.T, database *DB) []string {
	t.Helper()

	var out []string
	for table, cols := range datetimeColumns(t, database) {
		for _, col := range cols {
			eachRow(t, database,
				fmt.Sprintf("SELECT CAST(%s AS TEXT) FROM %s", quoteSQLiteIdent(col), quoteSQLiteIdent(table)),
				func(rows *sql.Rows) {
					var v sql.NullString
					if err := rows.Scan(&v); err != nil {
						t.Fatalf("scan %s.%s: %v", table, col, err)
					}
					out = append(out, fmt.Sprintf("%s.%s=%v", table, col, v.String))
				})
		}
	}
	sort.Strings(out)
	return out
}
