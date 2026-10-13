// ABOUTME: Rebuilds every FTS5 index from the content tables it is derived from
// ABOUTME: The cheap repair that --strict now advises, reachable with migrations pending

package db

import (
	"context"
	"fmt"
	"time"
)

// FTSRebuildResult is one index's rebuild and how long it took. The timing is
// reported because it is the argument: 22 seconds against a full re-parse of
// 47,000 transcripts is why this command exists.
type FTSRebuildResult struct {
	// Table is the fts5 virtual table that was rebuilt.
	Table string
	// Took is how long FTS5 spent re-deriving it.
	Took time.Duration
}

// RebuildFTSIndexes discards every full-text index in the archive and derives
// each one again from the table it indexes.
//
// This is the whole repair for a malformed index, and it reads nothing outside
// the database. turns_fts and tool_uses_fts are external-content tables, so
// each index is a pure function of its content table — FTS5's 'rebuild'
// command drops the index and re-tokenizes that table, and the content tables
// are never touched.
//
// Measured on the author's 1,049,544-turn archive on 2026-10-13, with
// turns_fts holding 136,279 stranded documents: 22 seconds, orphans to zero,
// and the strict check clean afterwards. The advice the strict check used to
// give — `ccvault sync --full` — re-reads roughly 47,000 transcript files to
// repair a structure derived entirely from rows already in the archive.
//
// Every index, discovered rather than named, so the repair and the check agree
// about what "every index" means; see listFTSTables.
func (db *DB) RebuildFTSIndexes() ([]FTSRebuildResult, error) {
	return db.RebuildFTSIndexesContext(context.Background())
}

// RebuildFTSIndexesContext is RebuildFTSIndexes bound to ctx. Re-tokenizing a
// million turns is one long write, so a caller that can be cancelled needs to
// be able to interrupt it; cancelling rolls that index's rebuild back and
// leaves it as it was.
func (db *DB) RebuildFTSIndexesContext(ctx context.Context) ([]FTSRebuildResult, error) {
	return rebuildFTSIndexes(ctx, db.DB)
}

// rebuildFTSIndexes is RebuildFTSIndexesContext against any writable
// connection.
func rebuildFTSIndexes(ctx context.Context, q sqlRunner) ([]FTSRebuildResult, error) {
	tables, err := listFTSTables(ctx, q)
	if err != nil {
		return nil, err
	}
	if len(tables) == 0 {
		// Same refusal the strict check makes, and for the same reason: a
		// repair that rebuilt nothing and reported success would leave the
		// user believing a broken index had been fixed.
		return nil, errNoFTSIndexes
	}

	results := make([]FTSRebuildResult, 0, len(tables))
	for _, table := range tables {
		quoted := quoteSQLiteIdent(table)
		stmt := fmt.Sprintf("INSERT INTO %s(%s) VALUES('rebuild')", quoted, quoted)

		started := time.Now()
		if _, err := q.ExecContext(ctx, stmt); err != nil {
			return nil, fmt.Errorf("rebuild %s: %w", table, err)
		}
		results = append(results, FTSRebuildResult{Table: table, Took: time.Since(started)})
	}
	return results, nil
}
