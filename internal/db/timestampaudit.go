// ABOUTME: Counts the DATETIME values that are not in the canonical layout, across every such column in the schema
// ABOUTME: Reports rather than enforces — a non-canonical value is readable, and refusing it would refuse a legitimate import

package db

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// DatetimeColumn names one column the schema declares DATETIME.
type DatetimeColumn struct {
	Table  string
	Column string
}

// TimestampLayoutColumn is one column's count.
type TimestampLayoutColumn struct {
	DatetimeColumn

	// Rows is how many non-NULL values the column holds. NULLs are not a
	// layout problem and are not counted either way.
	Rows int64

	// OffLayout is how many of those are not canonicalTimestampGlob.
	OffLayout int64

	// Sample is one of them, exactly as it sits on disk, or "" when there are
	// none. A count with no example gives the reader nothing to act on: the
	// whole question is whether these values came from an import they trust
	// or from something that went wrong, and the only way to tell is to look.
	Sample string
}

// TimestampLayoutReport is the outcome of counting non-canonical values in
// every DATETIME column in the archive.
//
// Reported, not enforced. The alternative considered in #144 was a CHECK
// constraint, which would make any future layout change a table rebuild and
// would refuse rows a legitimate import might carry. A non-canonical value is
// readable — SQLite's date functions parse it and db.ParseTimestamp parses it
// — it is only the things built on top of the layout that quietly assume it
// away: ORDER BY, MIN()/MAX() and the date predicate compare this text, and
// search_period slices the calendar date out of it by character position. So
// the archive is better served by the user seeing such a row than by a guard
// that blocks the operation that carried it in. #105 made the same trade,
// allowing reads of a newer archive while refusing writes.
type TimestampLayoutReport struct {
	columns []TimestampLayoutColumn
}

// Columns returns every DATETIME column that was counted, in schema order.
//
// Not decoration, for the same reason FTSStrictReport.Tables is not: a check
// that silently counted zero columns would report clean on every archive, and
// the only way a reader can tell that apart from a genuinely clean archive is
// if the output says what it looked at.
func (r TimestampLayoutReport) Columns() []TimestampLayoutColumn {
	out := make([]TimestampLayoutColumn, len(r.columns))
	copy(out, r.columns)
	return out
}

// Findings returns only the columns that hold a non-canonical value.
func (r TimestampLayoutReport) Findings() []TimestampLayoutColumn {
	var out []TimestampLayoutColumn
	for _, c := range r.columns {
		if c.OffLayout > 0 {
			out = append(out, c)
		}
	}
	return out
}

// OffLayout is the total across every column.
func (r TimestampLayoutReport) OffLayout() int64 {
	var total int64
	for _, c := range r.columns {
		total += c.OffLayout
	}
	return total
}

// Rows is the total number of non-NULL DATETIME values inspected.
func (r TimestampLayoutReport) Rows() int64 {
	var total int64
	for _, c := range r.columns {
		total += c.Rows
	}
	return total
}

// OK reports whether every value in every DATETIME column is canonical.
func (r TimestampLayoutReport) OK() bool { return r.OffLayout() == 0 }

// CheckTimestampLayout counts the values outside the canonical layout in every
// DATETIME column in the archive.
//
// It reads every row of every such column — on a million-turn archive that is
// a full scan of turns and tool_uses — which is why `stats` runs it under
// --strict rather than in the report people run to see their token counts.
func (db *DB) CheckTimestampLayout() (TimestampLayoutReport, error) {
	return db.CheckTimestampLayoutContext(context.Background())
}

// CheckTimestampLayoutContext is CheckTimestampLayout bound to ctx.
func (db *DB) CheckTimestampLayoutContext(ctx context.Context) (TimestampLayoutReport, error) {
	return timestampLayoutCheck(ctx, db.DB)
}

func timestampLayoutCheck(ctx context.Context, q sqlRunner) (TimestampLayoutReport, error) {
	columns, err := listDatetimeColumns(ctx, q)
	if err != nil {
		return TimestampLayoutReport{}, err
	}
	if len(columns) == 0 {
		return TimestampLayoutReport{}, fmt.Errorf(
			"no DATETIME columns found: this is not a ccvault archive")
	}

	report := TimestampLayoutReport{columns: make([]TimestampLayoutColumn, 0, len(columns))}
	for _, col := range columns {
		// CAST to TEXT on both sides of the comparison. SQLite is dynamically
		// typed, so a DATETIME column can hold an integer or a real that some
		// other tool put there, and GLOB against a non-text value is not a
		// question with an obvious answer. Casting asks the question this
		// check means: does the text of this value read as the canonical
		// layout? It also keeps the driver out of it — modernc parses any
		// column declared DATETIME into a time.Time on the way out and
		// re-renders it, so a plain SELECT would report the driver's
		// rendering of the value rather than the archive's.
		stmt := fmt.Sprintf(`
			SELECT COUNT(*),
			       COALESCE(SUM(CASE WHEN CAST(%[1]s AS TEXT) NOT GLOB ? THEN 1 ELSE 0 END), 0)
			FROM %[2]s WHERE %[1]s IS NOT NULL`,
			quoteSQLiteIdent(col.Column), quoteSQLiteIdent(col.Table))

		var counted TimestampLayoutColumn
		counted.DatetimeColumn = col
		if err := q.QueryRowContext(ctx, stmt, canonicalTimestampGlob).
			Scan(&counted.Rows, &counted.OffLayout); err != nil {
			return TimestampLayoutReport{}, fmt.Errorf(
				"count non-canonical values in %s.%s: %w", col.Table, col.Column, err)
		}

		if counted.OffLayout > 0 {
			sample := fmt.Sprintf(
				"SELECT CAST(%s AS TEXT) FROM %s WHERE CAST(%s AS TEXT) NOT GLOB ? LIMIT 1",
				quoteSQLiteIdent(col.Column), quoteSQLiteIdent(col.Table), quoteSQLiteIdent(col.Column))
			if err := q.QueryRowContext(ctx, sample, canonicalTimestampGlob).
				Scan(&counted.Sample); err != nil {
				return TimestampLayoutReport{}, fmt.Errorf(
					"read a non-canonical sample from %s.%s: %w", col.Table, col.Column, err)
			}
		}

		report.columns = append(report.columns, counted)
	}
	return report, nil
}

// listDatetimeColumns names every column the schema declares DATETIME.
//
// Discovered rather than hardcoded, for the reason listFTSTables is: a
// DATETIME column added by a later migration is covered by everything that
// covers DATETIME columns, instead of by whichever call sites someone
// remembered. #144 is specifically about covering all of them — sessions,
// tool_uses, source_files and projects hold them too, not just turns.timestamp.
func listDatetimeColumns(ctx context.Context, q sqlRunner) ([]DatetimeColumn, error) {
	tables, err := listUserTables(ctx, q)
	if err != nil {
		return nil, err
	}

	var found []DatetimeColumn
	for _, table := range tables {
		cols, err := datetimeColumnsOf(ctx, q, table)
		if err != nil {
			return nil, err
		}
		found = append(found, cols...)
	}
	return found, nil
}

func listUserTables(ctx context.Context, q sqlRunner) ([]string, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT name FROM sqlite_master
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%'
		ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan table name: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	return names, nil
}

func datetimeColumnsOf(ctx context.Context, q sqlRunner, table string) ([]DatetimeColumn, error) {
	// PRAGMA table_info takes no bind parameters, so the name is quoted as an
	// identifier. It came from sqlite_master rather than from a user, but a
	// table name reaching SQL unquoted is a habit worth not having.
	rows, err := q.QueryContext(ctx, "PRAGMA table_info("+quoteSQLiteIdent(table)+")")
	if err != nil {
		return nil, fmt.Errorf("read columns of %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	var found []DatetimeColumn
	for rows.Next() {
		var cid, notNull, pk int
		var name, colType string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			return nil, fmt.Errorf("scan columns of %s: %w", table, err)
		}
		if strings.EqualFold(colType, "DATETIME") {
			found = append(found, DatetimeColumn{Table: table, Column: name})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read columns of %s: %w", table, err)
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Column < found[j].Column })
	return found, nil
}
