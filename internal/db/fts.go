// ABOUTME: Integrity reporting for the turns_fts external-content search index
// ABOUTME: Counts index documents that no longer have a turns row behind them

package db

import (
	"context"
	"fmt"
)

// FTSIntegrity compares the turns_fts search index against the turns rows it
// is supposed to describe.
type FTSIntegrity struct {
	// Turns is the number of rows in the turns table.
	Turns int64
	// Indexed is the number of documents in the FTS index.
	Indexed int64
	// Orphaned is the number of indexed documents whose turns row is gone —
	// "ghost" entries. A search matches them, then the join back to turns
	// finds nothing, so they cost index size and recall accuracy without ever
	// surfacing a result.
	Orphaned int64
}

// Missing is the number of turns the index does not describe. Those rows are
// invisible to search even though the archive holds them.
func (f FTSIntegrity) Missing() int64 {
	live := f.Indexed - f.Orphaned
	if missing := f.Turns - live; missing > 0 {
		return missing
	}
	return 0
}

// Consistent reports whether the index and the turns table agree exactly.
func (f FTSIntegrity) Consistent() bool {
	return f.Orphaned == 0 && f.Missing() == 0
}

// CheckFTSIntegrity reports whether the search index still matches the turns
// table.
//
// It reads the %_docsize shadow table rather than turns_fts itself, because
// turns_fts is an external-content table: `SELECT COUNT(*) FROM turns_fts`
// resolves through the content table and returns the turns count, so it agrees
// with itself no matter how far the index has drifted. The documents the index
// actually holds are enumerable only through the shadow table, which FTS5
// maintains for every table that doesn't set columnsize=0.
//
// FTS5's own 'integrity-check' command with rank 1 detects the same condition
// and is the sanctioned API, but it tokenizes every indexed document and every
// row of the content table. `ccvault stats` runs this on an archive with tens
// of thousands of sessions, so this trades the stronger check for one that
// scans a two-column shadow table and does a rowid lookup per row.
//
// The counts it returns are a lower bound, not a verdict. A stranded document
// is only visible here while its rowid is still unused, and a re-parse hands
// every turn a fresh rowid from the end of the table — so the rowids a
// stranded document sits on get reused, its %_docsize row is overwritten, and
// these counts agree again while the index still holds the stale terms and
// still answers queries with them. Measured: 2,000 stranded documents injected
// into a 235,101-turn archive, then one `sync --full`, left Orphaned at 0 with
// all 2,000 still matchable and the strict check still reporting "database
// disk image is malformed". Consistent() reporting true therefore means
// "nothing cheap to see here", and only the strict check or a rebuild settles
// it. That is why `sync --full` rebuilds unconditionally rather than gating on
// this; see Syncer.rebuildSearchIndex.
func (db *DB) CheckFTSIntegrity() (FTSIntegrity, error) {
	return db.CheckFTSIntegrityContext(context.Background())
}

// CheckFTSIntegrityContext is CheckFTSIntegrity bound to ctx.
func (db *DB) CheckFTSIntegrityContext(ctx context.Context) (FTSIntegrity, error) {
	const query = `
		SELECT
			(SELECT COUNT(*) FROM turns),
			(SELECT COUNT(*) FROM turns_fts_docsize),
			(SELECT COUNT(*) FROM turns_fts_docsize d
			 WHERE NOT EXISTS (SELECT 1 FROM turns t WHERE t.rowid = d.id))`

	var f FTSIntegrity
	if err := db.QueryRowContext(ctx, query).Scan(&f.Turns, &f.Indexed, &f.Orphaned); err != nil {
		return FTSIntegrity{}, fmt.Errorf("check turns_fts integrity: %w", err)
	}
	return f, nil
}

// RebuildTurnsFTS discards the turns_fts index and derives it again from the
// turns table.
//
// This is the only repair for an index that has already drifted. The triggers
// keep the index in step with changes to turns, so they cannot remove a
// document whose turns row is gone — there is no row left to delete, and
// nothing to fire the trigger. FTS5's 'rebuild' command drops the lot and
// re-tokenizes the content table, which is also why it is the repair rather
// than a per-row fix: the cost is one pass over turns.
//
// Measured on the author's 1,014,942-turn archive: 18 seconds.
func (db *DB) RebuildTurnsFTS() error {
	return db.RebuildTurnsFTSContext(context.Background())
}

// RebuildTurnsFTSContext is RebuildTurnsFTS bound to ctx. Re-tokenizing a
// million turns is one long write, so a caller that can be cancelled needs to
// be able to interrupt it; cancelling rolls the rebuild back and leaves the
// index as it was.
func (db *DB) RebuildTurnsFTSContext(ctx context.Context) error {
	if _, err := db.ExecContext(ctx, "INSERT INTO turns_fts(turns_fts) VALUES('rebuild')"); err != nil {
		return fmt.Errorf("rebuild turns_fts: %w", err)
	}
	return nil
}
