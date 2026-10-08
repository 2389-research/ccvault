// ABOUTME: Tests that writing turns keeps turns_fts consistent on any connection
// ABOUTME: Runs the same re-parse twice, once with recursive_triggers off

package db

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/2389-research/ccvault/pkg/models"
)

// openArchiveWithRecursiveTriggers opens a temp archive forcing
// recursive_triggers to the given value, skipping Open's verification.
//
// recursive_triggers=0 is not a hypothetical. It is SQLite's default, so it is
// what every connection that does not opt in gets: the `sqlite3` CLI, any
// other tool pointed at the archive, and — the case that produced issue #93 —
// a ccvault binary built before the setting was added to the DSN, which is
// still what `ccvault` resolves to on a machine with an older install on PATH.
// The archive's search index has to survive a writer that lacks the pragma,
// because the file cannot make a writer take it.
func openArchiveWithRecursiveTriggers(t *testing.T, on int) *DB {
	t.Helper()

	dir := t.TempDir()
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(%d)&_pragma=recursive_triggers(%d)",
		filepath.Join(dir, dbFileName), busyTimeoutMS, on)

	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })

	var got int
	if err := sqlDB.QueryRow("PRAGMA recursive_triggers").Scan(&got); err != nil {
		t.Fatalf("read back recursive_triggers: %v", err)
	}
	if got != on {
		t.Fatalf("recursive_triggers is %d, want %d — the fixture is not testing what it claims", got, on)
	}

	db := &DB{DB: sqlDB, path: filepath.Join(dir, dbFileName)}
	if err := db.init(); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	return db
}

// assertFTSSound checks the two things that see index drift and the two that
// do not.
//
// COUNT(*) FROM turns_fts resolves through the content table and so agrees
// with itself no matter how far the index has drifted, and PRAGMA
// integrity_check reports a drifted index as clean. Only the %_docsize count
// and FTS5's own strict 'integrity-check' can fail here, which is why a test
// that asserts anything else measures nothing.
func assertFTSSound(t *testing.T, db *DB, when string) {
	t.Helper()

	integrity, err := db.CheckFTSIntegrity()
	if err != nil {
		t.Fatalf("%s: CheckFTSIntegrity: %v", when, err)
	}
	if integrity.Indexed != integrity.Turns {
		t.Errorf("%s: turns_fts holds %d documents for %d turns (%d orphaned, %d unindexed)",
			when, integrity.Indexed, integrity.Turns, integrity.Orphaned, integrity.Missing())
	}
	if _, err := db.Exec("INSERT INTO turns_fts(turns_fts, rank) VALUES('integrity-check', 1)"); err != nil {
		t.Errorf("%s: FTS5 strict integrity-check on turns_fts: %v", when, err)
	}
	if _, err := db.Exec("INSERT INTO tool_uses_fts(tool_uses_fts, rank) VALUES('integrity-check', 1)"); err != nil {
		t.Errorf("%s: FTS5 strict integrity-check on tool_uses_fts: %v", when, err)
	}
}

// sessionTurns builds one session's worth of turns, numbered from 0, with
// content that changes per generation so a stale index entry is identifiable.
func sessionTurns(sessionID string, n, generation int) []models.Turn {
	turns := make([]models.Turn, n)
	for i := range turns {
		turns[i] = models.Turn{
			ID:        fmt.Sprintf("%s-turn-%d", sessionID, i),
			SessionID: sessionID,
			Type:      "assistant",
			Timestamp: time.Date(2026, 10, 1, 12, 0, i, 0, time.UTC),
			Ordinal:   i,
			// Single tokens rather than "generation 1": the assertions match
			// against the index, so each value has to be a term FTS5 can be
			// asked for on its own.
			Content: fmt.Sprintf("gen%dcontent pos%dcontent", generation, i),
		}
	}
	return turns
}

// reparseSession is the shape sync writes a session in: delete the turns it
// holds, then insert the ones just parsed, in one transaction.
func reparseSession(t *testing.T, db *DB, turns []models.Turn, sessionID string) {
	t.Helper()
	if err := db.WithTx(func(tx *sql.Tx) error {
		if err := db.DeleteTurnsForSessionTx(tx, sessionID); err != nil {
			return err
		}
		return db.InsertTurnsTx(tx, turns)
	}); err != nil {
		t.Fatalf("re-parse session %s: %v", sessionID, err)
	}
}

// Re-parsing a session must leave turns_fts describing exactly the turns the
// archive holds, on a connection that has recursive_triggers and on one that
// does not.
//
// This is issue #93. `sync --full` re-parsed 14,475 sessions of the author's
// archive and left 19,023 index documents with no turns row behind them,
// while tool_uses_fts came through untouched. The asymmetry is the whole
// clue: tool_uses is written with a plain INSERT after an explicit DELETE, so
// its triggers fire under SQLite's own rules, whereas turns was written with
// INSERT OR REPLACE and leaned on recursive_triggers to route the REPLACE's
// implicit DELETE through turns_ad.
func TestInsertTurns_ReparseKeepsFTSConsistent(t *testing.T) {
	for _, recursiveTriggers := range []int{1, 0} {
		t.Run(fmt.Sprintf("recursive_triggers=%d", recursiveTriggers), func(t *testing.T) {
			db := openArchiveWithRecursiveTriggers(t, recursiveTriggers)
			seedTurnSession(t, db, "session-reparsed")

			reparseSession(t, db, sessionTurns("session-reparsed", 6, 1), "session-reparsed")
			assertFTSSound(t, db, "after the first parse")

			reparseSession(t, db, sessionTurns("session-reparsed", 6, 2), "session-reparsed")
			assertFTSSound(t, db, "after re-parsing the same session")

			// The content of the first parse must be gone from the index, not
			// merely outnumbered by the second parse's, and the second
			// parse's must be there.
			if got := ftsMatchCount(t, db, "{content} : gen1content"); got != 0 {
				t.Errorf("index still answers for the replaced generation with %d documents", got)
			}
			if got := ftsMatchCount(t, db, "{content} : gen2content"); got != 6 {
				t.Errorf("index answers for the current generation with %d documents, want 6", got)
			}
		})
	}
}

// A turn id that arrives under a second session is a REPLACE whose conflict
// is the primary key rather than the position, and it is the one real Claude
// Code data produces: a resumed transcript copies the earlier session's lines
// verbatim, uuids included, so the later parse claims rows the earlier
// session's parse wrote.
func TestInsertTurns_StolenTurnIDKeepsFTSConsistent(t *testing.T) {
	for _, recursiveTriggers := range []int{1, 0} {
		t.Run(fmt.Sprintf("recursive_triggers=%d", recursiveTriggers), func(t *testing.T) {
			db := openArchiveWithRecursiveTriggers(t, recursiveTriggers)
			seedTurnSession(t, db, "session-origin")
			seedTurnSession(t, db, "session-resumed")

			shared := func(sessionID string) []models.Turn {
				turns := sessionTurns("shared", 5, 1)
				for i := range turns {
					turns[i].SessionID = sessionID
				}
				return turns
			}

			reparseSession(t, db, shared("session-origin"), "session-origin")
			assertFTSSound(t, db, "after the origin session")

			reparseSession(t, db, shared("session-resumed"), "session-resumed")
			assertFTSSound(t, db, "after the resumed session claimed the same turn ids")
		})
	}
}

// A REPLACE can conflict on the primary key and on UNIQUE(session_id,
// ordinal) in the same statement, deleting two rows to insert one. Both
// deletions have to reach turns_ad.
func TestInsertTurns_DoubleConflictKeepsFTSConsistent(t *testing.T) {
	for _, recursiveTriggers := range []int{1, 0} {
		t.Run(fmt.Sprintf("recursive_triggers=%d", recursiveTriggers), func(t *testing.T) {
			db := openArchiveWithRecursiveTriggers(t, recursiveTriggers)
			seedTurnSession(t, db, "session-double")

			if err := db.InsertTurns([]models.Turn{
				{ID: "alpha", SessionID: "session-double", Type: "user", Timestamp: time.Now(), Ordinal: 0, Content: "alphacontent"},
				{ID: "beta", SessionID: "session-double", Type: "user", Timestamp: time.Now(), Ordinal: 1, Content: "betacontent"},
			}); err != nil {
				t.Fatalf("seed two turns: %v", err)
			}
			assertFTSSound(t, db, "after seeding two turns")

			// alpha moves onto beta's position: conflicts with alpha by id and
			// with beta by position.
			if err := db.InsertTurns([]models.Turn{
				{ID: "alpha", SessionID: "session-double", Type: "user", Timestamp: time.Now(), Ordinal: 1, Content: "gammacontent"},
			}); err != nil {
				t.Fatalf("insert the double-conflicting turn: %v", err)
			}
			assertFTSSound(t, db, "after a turn conflicted on both id and position")

			var remaining int
			if err := db.QueryRow("SELECT COUNT(*) FROM turns WHERE session_id = 'session-double'").Scan(&remaining); err != nil {
				t.Fatalf("count remaining turns: %v", err)
			}
			if remaining != 1 {
				t.Errorf("session holds %d turns after a double conflict, want 1", remaining)
			}
			for _, gone := range []string{"alphacontent", "betacontent"} {
				if got := ftsMatchCount(t, db, "{content} : "+gone); got != 0 {
					t.Errorf("index still answers for %s with %d documents", gone, got)
				}
			}
		})
	}
}

// The fixture itself has to be able to fail, or the recursive_triggers=0 runs
// above prove nothing. INSERT OR REPLACE is written out longhand here so the
// test does not depend on InsertTurnsTx keeping the statement that the fix
// removes.
func TestInsertOrReplace_LeavesFTSOrphansWithoutRecursiveTriggers(t *testing.T) {
	db := openArchiveWithRecursiveTriggers(t, 0)
	seedTurnSession(t, db, "session-raw")

	insert := func(id string, ordinal int, content string) {
		t.Helper()
		if _, err := db.Exec(`
			INSERT OR REPLACE INTO turns (id, session_id, type, timestamp, ordinal, content)
			VALUES (?, 'session-raw', 'user', '2026-10-01 00:00:00', ?, ?)`, id, ordinal, content); err != nil {
			t.Fatalf("insert or replace %s: %v", id, err)
		}
	}

	insert("raw", 0, "originalcontent")
	assertFTSSound(t, db, "after the first insert")

	insert("raw", 1, "replacementcontent")

	integrity, err := db.CheckFTSIntegrity()
	if err != nil {
		t.Fatalf("CheckFTSIntegrity: %v", err)
	}
	if integrity.Orphaned == 0 {
		t.Fatal("INSERT OR REPLACE left no orphan with recursive_triggers off; " +
			"this fixture can no longer detect issue #93 and the passing tests above mean nothing")
	}
	if _, err := db.Exec("INSERT INTO turns_fts(turns_fts, rank) VALUES('integrity-check', 1)"); err == nil {
		t.Error("FTS5 strict integrity-check passed on an index with a known orphan")
	}
}

// `ccvault import` writes turns too, and it meets the same shared uuid: a
// resumed transcript copies the earlier session's lines verbatim, so a turn
// uuid in the incoming archive can already be in the destination under another
// session. The FTS invariant has to hold for every writer of turns or the next
// one reintroduces the dependency on recursive_triggers.
//
// What the merge must *not* do is treat the two as one row. This test used to
// assert that the import replaced the destination's turn and that the index
// stopped answering for it — the behaviour #92 identified as the defect, and
// the one that cost the author's archive 31,864 turns. Since migration 012 a
// turn is identified by (session_id, id), so both sessions keep their copy and
// the index has to answer for both, exactly once each.
func TestMergeFrom_SharedTurnIDKeepsBothCopiesAndFTSConsistent(t *testing.T) {
	for _, recursiveTriggers := range []int{1, 0} {
		t.Run(fmt.Sprintf("recursive_triggers=%d", recursiveTriggers), func(t *testing.T) {
			dest := openArchiveWithRecursiveTriggers(t, recursiveTriggers)

			now := time.Now().UTC().Truncate(time.Second)
			// The destination holds the turn under one session id; the
			// incoming archive holds the same turn id under another.
			seedTurnSession(t, dest, "session-destination")
			if err := dest.InsertTurns([]models.Turn{{
				ID:        "shared-turn",
				SessionID: "session-destination",
				Type:      "user",
				Timestamp: now,
				Content:   "destinationcontent",
			}}); err != nil {
				t.Fatalf("seed destination turn: %v", err)
			}
			assertFTSSound(t, dest, "after seeding the destination")

			src := newSourceDB(t, func(s *DB) {
				seedSession(t, s, "/work/app", "session-incoming", "incomingcontent", now)
				if _, err := s.Exec("UPDATE turns SET id = 'shared-turn' WHERE session_id = 'session-incoming'"); err != nil {
					t.Fatalf("rename the incoming turn onto the destination's id: %v", err)
				}
			})

			if _, err := dest.MergeFrom(src); err != nil {
				t.Fatalf("MergeFrom: %v", err)
			}

			assertFTSSound(t, dest, "after merging a session that shares a turn id with the destination")
			if got := ftsMatchCount(t, dest, "{content} : destinationcontent"); got != 1 {
				t.Errorf("index answers for the destination's own turn with %d documents, want 1 — "+
					"the import took a turn belonging to a session it never picked", got)
			}
			if got := ftsMatchCount(t, dest, "{content} : incomingcontent"); got != 1 {
				t.Errorf("index answers for the imported turn with %d documents, want 1", got)
			}
			var copies int
			if err := dest.QueryRow(
				"SELECT COUNT(*) FROM turns WHERE id = 'shared-turn'").Scan(&copies); err != nil {
				t.Fatalf("count copies of the shared turn: %v", err)
			}
			if copies != 2 {
				t.Errorf("%d rows hold the shared uuid, want 2 — one per session", copies)
			}
		})
	}
}

// Belt and braces: the repair has to clear orphans that an earlier writer
// already left, because the write-path fix cannot reach a file that is
// already damaged.
func TestRebuildTurnsFTS_ClearsExistingOrphans(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	seedTurnSession(t, db, "session-repair")
	if err := db.InsertTurns(sessionTurns("session-repair", 3, 1)); err != nil {
		t.Fatalf("seed turns: %v", err)
	}

	var freeRowid int64
	if err := db.QueryRow("SELECT COALESCE(MAX(rowid), 0) + 1000 FROM turns").Scan(&freeRowid); err != nil {
		t.Fatalf("find a free rowid: %v", err)
	}
	if _, err := db.Exec("INSERT INTO turns_fts(rowid, content) VALUES (?, 'strandedcontent')", freeRowid); err != nil {
		t.Fatalf("inject orphan: %v", err)
	}

	before, err := db.CheckFTSIntegrity()
	if err != nil {
		t.Fatalf("CheckFTSIntegrity: %v", err)
	}
	if before.Orphaned != 1 {
		t.Fatalf("Orphaned = %d before the repair, want 1", before.Orphaned)
	}

	if err := db.RebuildTurnsFTS(); err != nil {
		t.Fatalf("RebuildTurnsFTS: %v", err)
	}

	assertFTSSound(t, db, "after the repair")
	if got := ftsMatchCount(t, db, "{content} : strandedcontent"); got != 0 {
		t.Errorf("the orphan's content still answers queries (%d documents)", got)
	}

	// The repair must not cost the live turns their index entries.
	if got := ftsMatchCount(t, db, "{content} : gen1content"); got != 3 {
		t.Errorf("the live turns answer with %d documents after the repair, want 3", got)
	}
}
