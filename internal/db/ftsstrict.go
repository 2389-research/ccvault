// ABOUTME: FTS5's own strict integrity-check over every full-text index in the archive
// ABOUTME: The only check that returns a verdict rather than a lower bound

package db

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// ftsRepairAdvice names the one repair for a malformed index, in the spelling
// the user has to type. `sync --full` re-parses every transcript and rebuilds
// the index on the way past; `sync --rebuild` would also fix it and wipes the
// archive first, which is a steep price for a search index.
const ftsRepairAdvice = "repair the index with 'ccvault sync --full'"

// FTSTableResult is one index's verdict.
type FTSTableResult struct {
	// Table is the fts5 virtual table that was checked.
	Table string
	// Err is what FTS5 said, or nil if the index is sound.
	Err error
}

// FTSStrictReport is the outcome of running FTS5's 'integrity-check' over
// every full-text index in the archive.
//
// A malformed index arrives in the report rather than as the error of the
// function that produced it, because "this index is broken" and "the check
// could not be run" are different facts and a caller has to be able to tell
// them apart. Err() exists for the callers that only need the verdict.
type FTSStrictReport struct {
	results []FTSTableResult
}

// Tables names every index the check actually inspected, in the order it
// inspected them.
//
// Not decoration. A strict check that silently enumerated zero tables would
// report clean on every archive ever handed to it, which is worse than having
// no check at all — it manufactures confidence. Callers that print a verdict
// should print what it covered.
func (r FTSStrictReport) Tables() []string {
	names := make([]string, 0, len(r.results))
	for _, res := range r.results {
		names = append(names, res.Table)
	}
	return names
}

// Malformed names the indexes that failed.
func (r FTSStrictReport) Malformed() []string {
	var names []string
	for _, res := range r.results {
		if res.Err != nil {
			names = append(names, res.Table)
		}
	}
	return names
}

// Results returns the per-index outcomes.
func (r FTSStrictReport) Results() []FTSTableResult {
	out := make([]FTSTableResult, len(r.results))
	copy(out, r.results)
	return out
}

// OK reports whether every index passed.
func (r FTSStrictReport) OK() bool {
	return len(r.Malformed()) == 0
}

// Err renders the failures as one error, naming the repair, or nil if there
// were none.
func (r FTSStrictReport) Err() error {
	var parts []string
	for _, res := range r.results {
		if res.Err != nil {
			parts = append(parts, fmt.Sprintf("%s failed FTS5's strict integrity-check: %v", res.Table, res.Err))
		}
	}
	if len(parts) == 0 {
		return nil
	}
	return fmt.Errorf("%s; %s", strings.Join(parts, "; "), ftsRepairAdvice)
}

// CheckFTSStrict runs FTS5's own 'integrity-check' over every full-text index
// in the archive.
//
// This is the authoritative check, and the only one. CheckFTSIntegrity's
// %_docsize comparison is a lower bound: a stranded document is visible there
// only while its rowid is still unused, and a re-parse hands every turn a fresh
// rowid, so the counts agree again while the index still answers queries with
// the stale terms. Measured in #93: 2,000 stranded documents in a 235,101-turn
// archive, one `sync --full`, and the cheap check read zero orphans while this
// one said "database disk image is malformed" and all 2,000 were still
// matchable. SQLite's own PRAGMA integrity_check says "ok" throughout, because
// nothing is structurally wrong with the pages — only with what they mean.
//
// It tokenizes every indexed document and every row of the content table, which
// is why it is not what `stats` runs by default: measured at ~60% of a full
// rebuild, 0.6s against 1.0s on a 235,101-turn index, so a couple of seconds on
// a million-turn archive.
func (db *DB) CheckFTSStrict() (FTSStrictReport, error) {
	return db.CheckFTSStrictContext(context.Background())
}

// CheckFTSStrictContext is CheckFTSStrict bound to ctx. Tokenizing a million
// turns takes seconds, so a caller that can be cancelled needs to be able to
// interrupt it.
func (db *DB) CheckFTSStrictContext(ctx context.Context) (FTSStrictReport, error) {
	return strictFTSCheck(ctx, db.DB)
}

// strictFTSCheck is CheckFTSStrictContext against any connection, so
// compaction can run the same verdict on a copy it has not swapped in yet.
//
// The connection must be writable. FTS5's integrity-check is spelled as an
// INSERT into the virtual table, and a read-only handle refuses it with
// "attempt to write a readonly database" before fts5 ever sees it — a failure
// that reads exactly like a passing check to anything that only looks at
// whether an error came back, and exactly like corruption to anything that
// does not look at which error. The check itself writes nothing: a read-write
// handle running it over a copy leaves the file byte-for-byte identical.
func strictFTSCheck(ctx context.Context, q sqlRunner) (FTSStrictReport, error) {
	tables, err := listFTSTables(ctx, q)
	if err != nil {
		return FTSStrictReport{}, err
	}
	if len(tables) == 0 {
		return FTSStrictReport{}, errors.New(
			"no fts5 indexes found, so there was nothing to check: this is not a ccvault archive, " +
				"or its search indexes have been dropped")
	}

	report := FTSStrictReport{results: make([]FTSTableResult, 0, len(tables))}
	for _, table := range tables {
		quoted := quoteSQLiteIdent(table)
		stmt := fmt.Sprintf("INSERT INTO %s(%s, rank) VALUES('integrity-check', 1)", quoted, quoted)

		_, err := q.ExecContext(ctx, stmt)
		switch {
		case err == nil:
			report.results = append(report.results, FTSTableResult{Table: table})
		case isCorruptionError(err):
			report.results = append(report.results, FTSTableResult{Table: table, Err: err})
		default:
			// Not a verdict. A read-only handle, a cancelled context or an I/O
			// error must not be reported as a malformed index: the user would
			// go and rebuild a sound one, and — worse — the next clean run
			// would look like the rebuild had fixed something.
			return FTSStrictReport{}, fmt.Errorf("run FTS5 integrity-check on %s: %w", table, err)
		}
	}
	return report, nil
}

// listFTSTables names every fts5 virtual table in the schema. Discovered
// rather than hardcoded, so an index added later is checked by everything that
// checks indexes, instead of by whichever call sites someone remembered.
//
// Discovered structurally rather than by reading the CREATE statement, which
// is #137. The text match it replaces was `lower(sql) LIKE '%using fts5%'`, so
// an index declared `USING  fts5` — two spaces, or a newline — dropped out of
// --strict's coverage with no error and no visible change beyond one fewer
// table in a list nobody reads closely. The failure mode is the one the strict
// check exists to eliminate: a check that silently covers less than you think
// it does. Formatting cannot defeat a structural test.
//
// The structure, in two parts. A virtual table carries rootpage 0 in
// sqlite_master, which is what separates the index from the ordinary tables
// around it — including its own shadow tables, which have real roots. And
// every fts5 table, whatever its options, owns shadow tables named
// <name>_data, <name>_idx and <name>_config; fts3 and fts4 own a different set
// (_content, _segdir, _segments) and so are not mistaken for fts5 here.
// %_docsize is deliberately not required: columnsize=0 omits it.
func listFTSTables(ctx context.Context, q sqlRunner) ([]string, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT m.name FROM sqlite_master m
		WHERE m.type = 'table'
		  AND COALESCE(m.rootpage, 0) = 0
		  AND EXISTS (SELECT 1 FROM sqlite_master s
		              WHERE s.type = 'table' AND s.name = m.name || '_data')
		  AND EXISTS (SELECT 1 FROM sqlite_master s
		              WHERE s.type = 'table' AND s.name = m.name || '_idx')
		  AND EXISTS (SELECT 1 FROM sqlite_master s
		              WHERE s.type = 'table' AND s.name = m.name || '_config')
		ORDER BY m.name`)
	if err != nil {
		return nil, fmt.Errorf("list fts5 indexes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan fts5 index name: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list fts5 indexes: %w", err)
	}
	return names, nil
}

// isCorruptionError reports whether err is SQLite calling the data malformed.
// FTS5's integrity-check answers a damaged index with SQLITE_CORRUPT_VTAB,
// which shares the SQLITE_CORRUPT primary code.
func isCorruptionError(err error) bool {
	var serr *sqlite.Error
	if errors.As(err, &serr) {
		return serr.Code()&0xff == sqlite3.SQLITE_CORRUPT
	}
	return strings.Contains(strings.ToLower(err.Error()), "database disk image is malformed")
}

// quoteSQLiteIdent renders a name as a double-quoted SQLite identifier. The
// names come from sqlite_master rather than from a user, but a table name
// reaching SQL unquoted is a habit worth not having.
func quoteSQLiteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
