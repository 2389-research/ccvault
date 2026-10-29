// ABOUTME: Tests the Tx-scoped accessors honour the caller's transaction, plus the archive counters.
// ABOUTME: Covers rollback visibility, the absent-vs-NULL distinctions, and the report accessors.

package db

import (
	"database/sql"
	"testing"
	"time"

	"github.com/2389-research/ccvault/pkg/models"
)

func seedProjectRow(t *testing.T, database *DB, path string) *models.Project {
	t.Helper()

	p := &models.Project{Path: path, DisplayName: path}
	if err := database.UpsertProject(p); err != nil {
		t.Fatalf("upsert project %s: %v", path, err)
	}
	return p
}

func seedSessionRow(t *testing.T, database *DB, id string, projectID int64) *models.Session {
	t.Helper()

	s := &models.Session{
		ID:         id,
		ProjectID:  projectID,
		StartedAt:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		SourceFile: "/tmp/" + id + ".jsonl",
	}
	if err := database.UpsertSession(s); err != nil {
		t.Fatalf("upsert session %s: %v", id, err)
	}
	return s
}

// --- the Tx variants are genuinely transactional ------------------------

// The whole reason UpsertSessionTx exists beside UpsertSession is that the
// caller supplies the transaction, so the write must be undone when that
// transaction rolls back. If it quietly opened its own, a sync that failed
// halfway would leave the session row behind -- a half-written archive, which
// is the failure mode this repo has been bitten by (#87, #92, #172).
//
// Asserted in both directions: invisible after a rollback, present after a
// commit. A test that only committed would pass against a function that
// ignored the transaction entirely.
func TestUpsertSessionTx_RollbackLeavesNoRow(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	tx, err := database.BeginTx()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	err = database.UpsertSessionTx(tx, &models.Session{
		ID:         "rolled-back",
		StartedAt:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		SourceFile: "/tmp/rolled-back.jsonl",
	})
	if err != nil {
		t.Fatalf("UpsertSessionTx: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	got, err := database.GetSession("rolled-back")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got != nil {
		t.Error("the session survived a rolled-back transaction, so the write did not " +
			"participate in the caller's transaction")
	}
}

func TestUpsertSessionTx_CommitPersistsTheRow(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	tx, err := database.BeginTx()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := database.UpsertSessionTx(tx, &models.Session{
		ID:         "committed",
		StartedAt:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		SourceFile: "/tmp/committed.jsonl",
		TurnCount:  5,
	}); err != nil {
		t.Fatalf("UpsertSessionTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	got, err := database.GetSession("committed")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got == nil {
		t.Fatal("the committed session is absent")
	}
	if got.TurnCount != 5 {
		t.Errorf("turn_count = %d, want 5", got.TurnCount)
	}
}

func TestUpsertSourceFileMtimeTx_RollbackLeavesNoRow(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	mtime := time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC)

	tx, err := database.BeginTx()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := database.UpsertSourceFileMtimeTx(tx, "/tmp/rolled.jsonl", mtime); err != nil {
		t.Fatalf("UpsertSourceFileMtimeTx: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	all, err := database.GetAllSourceMtimes()
	if err != nil {
		t.Fatalf("GetAllSourceMtimes: %v", err)
	}
	if _, present := all["/tmp/rolled.jsonl"]; present {
		t.Error("the source_files row survived a rolled-back transaction")
	}
}

// The source defaults to claude-code when none is given, and an explicit one
// is keyed separately -- the table is UNIQUE(path, source), so one path can
// legitimately hold a row per source. Writing both and seeing two rows is
// what proves the source reached the key rather than being ignored.
func TestUpsertSourceFileMtimeTx_SourceDefaultsAndKeysSeparately(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	mtime := time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC)

	if err := database.WithTx(func(tx *sql.Tx) error {
		if err := database.UpsertSourceFileMtimeTx(tx, "/tmp/both.jsonl", mtime); err != nil {
			return err
		}
		return database.UpsertSourceFileMtimeTx(tx, "/tmp/both.jsonl", mtime, "codex")
	}); err != nil {
		t.Fatalf("WithTx: %v", err)
	}

	defaulted, err := database.GetAllSourceMtimes("claude-code")
	if err != nil {
		t.Fatalf("GetAllSourceMtimes(claude-code): %v", err)
	}
	if _, ok := defaulted["/tmp/both.jsonl"]; !ok {
		t.Error("the no-source write was not filed under claude-code")
	}

	codex, err := database.GetAllSourceMtimes("codex")
	if err != nil {
		t.Fatalf("GetAllSourceMtimes(codex): %v", err)
	}
	if _, ok := codex["/tmp/both.jsonl"]; !ok {
		t.Error("the explicit-source write was not filed under codex")
	}
}

// --- absent vs NULL vs set ----------------------------------------------

// The documented three-state contract: a session that never held a turn is a
// different thing from one whose tail is unknown, and the caller must not
// read the first as a mismatch. So "no row" and "NULL" both report ok=false
// with an empty string, and only a stored value reports ok=true. Checking
// the string alone would conflate all three.
func TestSessionLastEntryUUIDTx_DistinguishesAbsentNullAndSet(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	p := seedProjectRow(t, database, "/proj/tails")
	// seedSessionRow writes no last_entry_uuid, so this row holds NULL.
	seedSessionRow(t, database, "null-tail", p.ID)

	withTail := &models.Session{
		ID:            "has-tail",
		ProjectID:     p.ID,
		StartedAt:     time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		SourceFile:    "/tmp/has-tail.jsonl",
		LastEntryUUID: "turn-uuid-9",
	}
	if err := database.UpsertSession(withTail); err != nil {
		t.Fatalf("upsert has-tail: %v", err)
	}

	for _, tc := range []struct {
		name      string
		sessionID string
		wantUUID  string
		wantOK    bool
	}{
		{"no row at all", "never-existed", "", false},
		{"row with a NULL tail", "null-tail", "", false},
		{"row with a stored tail", "has-tail", "turn-uuid-9", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := database.WithTx(func(tx *sql.Tx) error {
				gotUUID, gotOK, err := database.SessionLastEntryUUIDTx(tx, tc.sessionID)
				if err != nil {
					return err
				}
				if gotUUID != tc.wantUUID {
					t.Errorf("uuid = %q, want %q", gotUUID, tc.wantUUID)
				}
				if gotOK != tc.wantOK {
					t.Errorf("ok = %v, want %v -- an absent tail and a stored one must not "+
						"read the same to the caller", gotOK, tc.wantOK)
				}
				return nil
			}); err != nil {
				t.Fatalf("WithTx: %v", err)
			}
		})
	}
}

// Same three-state shape for the project a session is filed under. The join
// means a session whose project row is missing reads back as ("", false) --
// nothing to reconcile -- rather than erroring.
//
// One caveat, recorded so nobody chases it as a coverage gap: the function's
// `path.Valid` cannot be observed as false. projects.path is `TEXT UNIQUE NOT
// NULL` (migration 001), and a session pointing at a project row that does
// not exist makes the JOIN return no row at all, which the ErrNoRows branch
// above it already handles. So replacing `path.Valid` with a literal `true`
// passes every test here, and that is a property of the schema rather than a
// hole in these assertions -- the sql.NullString is defensive plumbing for a
// state the schema forbids. Contrast SessionLastEntryUUIDTx, where
// last_entry_uuid really is nullable (migration 008) and the equivalent
// mutation is caught.
func TestSessionProjectPathTx_DistinguishesAbsentAndFiled(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	p := seedProjectRow(t, database, "/proj/filed-here")
	seedSessionRow(t, database, "filed", p.ID)
	// project_id 9999 has no projects row, so the join finds nothing.
	seedSessionRow(t, database, "orphaned", 9999)

	for _, tc := range []struct {
		name      string
		sessionID string
		wantPath  string
		wantOK    bool
	}{
		{"no session row", "never-existed", "", false},
		{"session whose project row is missing", "orphaned", "", false},
		{"session filed under a real project", "filed", "/proj/filed-here", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := database.WithTx(func(tx *sql.Tx) error {
				gotPath, gotOK, err := database.SessionProjectPathTx(tx, tc.sessionID)
				if err != nil {
					return err
				}
				if gotPath != tc.wantPath {
					t.Errorf("path = %q, want %q", gotPath, tc.wantPath)
				}
				if gotOK != tc.wantOK {
					t.Errorf("ok = %v, want %v", gotOK, tc.wantOK)
				}
				return nil
			}); err != nil {
				t.Fatalf("WithTx: %v", err)
			}
		})
	}
}

// A source file nobody has synced is not an error -- it is a file with no
// recorded mtime, and sync has to be able to ask about one without handling
// a failure. The zero time is the answer, nil is the error.
func TestGetSourceMtime_UnknownPathIsZeroTimeNotAnError(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	got, err := database.GetSourceMtime("/never/synced.jsonl")
	if err != nil {
		t.Fatalf("an unsynced path produced an error: %v", err)
	}
	if !got.IsZero() {
		t.Errorf("mtime = %v, want the zero time", got)
	}
}

// A synced file reads back the mtime that was stored for it.
func TestGetSourceMtime_ReturnsTheStoredMtime(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	p := seedProjectRow(t, database, "/proj/mtimes")
	seedSessionRow(t, database, "has-mtime", p.ID)

	// upsertSession stamps source_mtime with time.Now(), so pin it to a
	// known value to assert on something other than "not zero".
	stored := time.Date(2026, 4, 1, 9, 30, 0, 0, time.UTC)
	if _, err := database.Exec(
		"UPDATE sessions SET source_mtime = ? WHERE source_file = ?",
		FormatTimestamp(stored), "/tmp/has-mtime.jsonl"); err != nil {
		t.Fatalf("pin source_mtime: %v", err)
	}

	got, err := database.GetSourceMtime("/tmp/has-mtime.jsonl")
	if err != nil {
		t.Fatalf("GetSourceMtime: %v", err)
	}
	if !got.Equal(stored) {
		t.Errorf("mtime = %v, want the stored %v", got, stored)
	}
}

// A row whose source_mtime is NULL must read back as the zero time, not as a
// scan failure. The write path always stamps time.Now(), so NULL is not
// reachable through UpsertSession -- it is the state a row migrated from an
// older schema can be in, which is what this defensive branch is for. The
// NULL is therefore written directly, because that is the only honest way to
// put the archive in the state the branch exists to handle.
func TestGetSourceMtime_NullMtimeIsZeroTimeNotAnError(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	p := seedProjectRow(t, database, "/proj/mtimes")
	seedSessionRow(t, database, "null-mtime", p.ID)
	if _, err := database.Exec(
		"UPDATE sessions SET source_mtime = NULL WHERE source_file = ?",
		"/tmp/null-mtime.jsonl"); err != nil {
		t.Fatalf("null out source_mtime: %v", err)
	}

	got, err := database.GetSourceMtime("/tmp/null-mtime.jsonl")
	if err != nil {
		t.Fatalf("a NULL source_mtime produced an error: %v", err)
	}
	if !got.IsZero() {
		t.Errorf("mtime = %v, want the zero time", got)
	}
}

// --- archive counters ---------------------------------------------------

// COALESCE(SUM(...), 0) is the point: SUM over no rows is NULL, which would
// fail the scan into int64. An empty archive must answer (0, 0) rather than
// erroring, because `ccvault stats` runs before anything is synced.
func TestGetProjectStats_EmptyArchiveIsZeroNotAnError(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	count, tokens, err := database.GetProjectStats()
	if err != nil {
		t.Fatalf("an empty archive produced an error: %v", err)
	}
	if count != 0 || tokens != 0 {
		t.Errorf("got (%d, %d), want (0, 0)", count, tokens)
	}
}

func TestGetProjectStats_CountsProjectsAndSumsTokens(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	for _, p := range []*models.Project{
		{Path: "/a", DisplayName: "a", TotalTokens: 100},
		{Path: "/b", DisplayName: "b", TotalTokens: 250},
	} {
		if err := database.UpsertProject(p); err != nil {
			t.Fatalf("upsert %s: %v", p.Path, err)
		}
	}

	count, tokens, err := database.GetProjectStats()
	if err != nil {
		t.Fatalf("GetProjectStats: %v", err)
	}
	if count != 2 {
		t.Errorf("count = %d, want 2", count)
	}
	// 350, not 100 or 250 -- a sum, not a first or last row.
	if tokens != 350 {
		t.Errorf("tokens = %d, want 350 (the sum of both projects)", tokens)
	}
}

func TestGetTurnCount_CountsTurnsAcrossEverySession(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	if n, err := database.GetTurnCount(); err != nil {
		t.Fatalf("GetTurnCount on an empty archive: %v", err)
	} else if n != 0 {
		t.Errorf("empty archive turn count = %d, want 0", n)
	}

	p := seedProjectRow(t, database, "/proj/turns")
	seedSessionRow(t, database, "sess-a", p.ID)
	seedSessionRow(t, database, "sess-b", p.ID)

	turns := []models.Turn{
		{ID: "t1", SessionID: "sess-a", Type: "user", Ordinal: 0, Content: "one",
			Timestamp: time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)},
		{ID: "t2", SessionID: "sess-a", Type: "user", Ordinal: 1, Content: "two",
			Timestamp: time.Date(2026, 1, 1, 0, 0, 2, 0, time.UTC)},
		{ID: "t3", SessionID: "sess-b", Type: "user", Ordinal: 0, Content: "three",
			Timestamp: time.Date(2026, 1, 1, 0, 0, 3, 0, time.UTC)},
	}
	if err := database.InsertTurns(turns); err != nil {
		t.Fatalf("insert turns: %v", err)
	}

	// 3, spanning two sessions -- a per-session count would report 2 or 1.
	n, err := database.GetTurnCount()
	if err != nil {
		t.Fatalf("GetTurnCount: %v", err)
	}
	if n != 3 {
		t.Errorf("turn count = %d, want 3 across both sessions", n)
	}
}

// Sessions with no model must not become a map entry under the empty string:
// the WHERE clause excludes them, and an empty key would render as a blank
// row in `ccvault stats`. The sum is input+output for the model's sessions.
func TestGetTokensByModel_GroupsByModelAndExcludesBlankModels(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	p := seedProjectRow(t, database, "/proj/models")
	for _, s := range []*models.Session{
		{ID: "m1", ProjectID: p.ID, Model: "opus", InputTokens: 10, OutputTokens: 5,
			StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), SourceFile: "/tmp/m1.jsonl"},
		{ID: "m2", ProjectID: p.ID, Model: "opus", InputTokens: 20, OutputTokens: 1,
			StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), SourceFile: "/tmp/m2.jsonl"},
		{ID: "m3", ProjectID: p.ID, Model: "sonnet", InputTokens: 7, OutputTokens: 3,
			StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), SourceFile: "/tmp/m3.jsonl"},
		{ID: "m4", ProjectID: p.ID, Model: "", InputTokens: 999, OutputTokens: 999,
			StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), SourceFile: "/tmp/m4.jsonl"},
	} {
		if err := database.UpsertSession(s); err != nil {
			t.Fatalf("upsert %s: %v", s.ID, err)
		}
	}

	got, err := database.GetTokensByModel()
	if err != nil {
		t.Fatalf("GetTokensByModel: %v", err)
	}

	if _, present := got[""]; present {
		t.Error(`a blank model became a map entry under ""; it would render as a nameless row`)
	}
	// 10+5 + 20+1 = 36, so both of opus's sessions were summed.
	if got["opus"] != 36 {
		t.Errorf("opus = %d, want 36 (both sessions summed)", got["opus"])
	}
	if got["sonnet"] != 10 {
		t.Errorf("sonnet = %d, want 10", got["sonnet"])
	}
	if len(got) != 2 {
		t.Errorf("got %d models, want exactly 2: %v", len(got), got)
	}
}

func TestGetTokensByModel_EmptyArchiveIsAnEmptyMapNotNil(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	got, err := database.GetTokensByModel()
	if err != nil {
		t.Fatalf("GetTokensByModel: %v", err)
	}
	if got == nil {
		t.Fatal("got nil, want an empty map so callers can range over it")
	}
	if len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}

// --- report accessors ---------------------------------------------------

// Results hands out a copy. A caller mutating what it got back must not be
// able to rewrite the report's own record of what the check found -- the
// report is the evidence, and `copy` is what makes that true.
func TestFTSStrictReport_ResultsReturnsACopyCallersCannotCorrupt(t *testing.T) {
	report := FTSStrictReport{results: []FTSTableResult{
		{Table: "turns_fts"},
		{Table: "sessions_fts"},
	}}

	got := report.Results()
	if len(got) != 2 {
		t.Fatalf("got %d results, want 2", len(got))
	}
	if got[0].Table != "turns_fts" || got[1].Table != "sessions_fts" {
		t.Fatalf("Results did not return the stored outcomes: %+v", got)
	}

	got[0].Table = "clobbered"

	again := report.Results()
	if again[0].Table != "turns_fts" {
		t.Errorf("mutating the returned slice changed the report to %q; Results must copy",
			again[0].Table)
	}
}

func TestTimestampLayoutReport_RowsTotalsEveryColumn(t *testing.T) {
	report := TimestampLayoutReport{columns: []TimestampLayoutColumn{
		{Rows: 10, OffLayout: 1},
		{Rows: 32, OffLayout: 0},
	}}

	// 42, so both columns were added rather than the first or last taken.
	if got := report.Rows(); got != 42 {
		t.Errorf("Rows = %d, want 42 (10 + 32)", got)
	}
	if got := report.OffLayout(); got != 1 {
		t.Errorf("OffLayout = %d, want 1", got)
	}
	if report.OK() {
		t.Error("OK is true despite an off-layout value")
	}
}

func TestTimestampLayoutReport_NoColumnsIsZeroAndOK(t *testing.T) {
	var report TimestampLayoutReport

	if got := report.Rows(); got != 0 {
		t.Errorf("Rows = %d, want 0", got)
	}
	if !report.OK() {
		t.Error("an empty report is not OK")
	}
}

// Every cause needs a distinct name, and an unrecognised one must say which
// number it was rather than rendering as a neighbour. A switch missing a case
// would otherwise fall through to a plausible-looking label.
func TestDriftCause_StringNamesEveryCauseDistinctly(t *testing.T) {
	for _, tc := range []struct {
		cause DriftCause
		want  string
	}{
		{DriftTranscriptGone, "transcript-gone"},
		{DriftUnstorable, "unstorable"},
		{DriftUnparsed, "unparsed"},
		{DriftUnclassified, "unclassified"},
	} {
		if got := tc.cause.String(); got != tc.want {
			t.Errorf("DriftCause(%d).String() = %q, want %q", int(tc.cause), got, tc.want)
		}
	}
}

func TestDriftCause_UnknownValueReportsItsNumber(t *testing.T) {
	got := DriftCause(99).String()
	if got != "DriftCause(99)" {
		t.Errorf("DriftCause(99).String() = %q, want %q -- an unnamed cause must not "+
			"borrow a named one's label", got, "DriftCause(99)")
	}
}

// --- BeginTx ------------------------------------------------------------

// BeginTx hands back a usable transaction rather than the connection, which
// is what every Tx-scoped accessor above depends on. Proven by writing
// through it and seeing the rollback take effect.
func TestBeginTx_ReturnsAUsableTransaction(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	tx, err := database.BeginTx()
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if tx == nil {
		t.Fatal("BeginTx returned a nil transaction")
	}
	if _, err := tx.Exec("INSERT INTO projects (path, display_name) VALUES (?, ?)",
		"/tx/scratch", "scratch"); err != nil {
		t.Fatalf("write through the transaction: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	count, _, err := database.GetProjectStats()
	if err != nil {
		t.Fatalf("GetProjectStats: %v", err)
	}
	if count != 0 {
		t.Errorf("project count = %d after a rolled-back insert, want 0", count)
	}
}
