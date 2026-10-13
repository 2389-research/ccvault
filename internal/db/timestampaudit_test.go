// ABOUTME: Tests the report that counts DATETIME values outside the canonical layout, across every such column
// ABOUTME: The archive is better served by seeing a non-canonical row than by a constraint that refuses a legitimate import

package db

import (
	"strings"
	"testing"
	"time"
)

// TestCheckTimestampLayout_CleanArchiveIsClean. A report that said "found
// nothing" on an archive it had not actually looked at would be worse than no
// report, so it has to name what it covered as well as what it found.
func TestCheckTimestampLayout_CleanArchiveIsClean(t *testing.T) {
	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = database.Close() }()

	seedEveryDatetimeColumn(t, database, time.Date(2026, 8, 14, 20, 15, 0, 134000000, time.UTC))

	report, err := database.CheckTimestampLayout()
	if err != nil {
		t.Fatalf("check timestamp layout: %v", err)
	}
	if !report.OK() {
		t.Errorf("a freshly written archive is not canonical: %v", report.Findings())
	}
	if report.OffLayout() != 0 {
		t.Errorf("OffLayout() = %d on a clean archive, want 0", report.OffLayout())
	}

	// Every DATETIME column in the schema, covered, with rows in it. The
	// column set is discovered from the schema, so a column a later migration
	// adds is covered without anyone remembering to add it here.
	covered := map[string]bool{}
	for _, c := range report.Columns() {
		covered[c.Table+"."+c.Column] = true
		if c.Rows == 0 {
			t.Errorf("%s.%s was counted but holds no rows; the fixture does not exercise it", c.Table, c.Column)
		}
	}
	for _, want := range []string{
		"projects.first_seen_at", "projects.last_activity_at",
		"sessions.started_at", "sessions.ended_at", "sessions.source_mtime",
		"turns.timestamp", "tool_uses.timestamp",
		"source_files.mtime", "source_files.synced_at",
	} {
		if !covered[want] {
			t.Errorf("%s is a DATETIME column and the report does not cover it", want)
		}
	}
}

// TestCheckTimestampLayout_SeesAReadableButNonCanonicalRow is the whole point
// of #144.
//
// Migration 014 deliberately leaves alone any value it does not recognise —
// rewriting data of unknown provenance is how a cosmetic inconsistency becomes
// data loss. So a readable-but-non-canonical value survives the migration
// silently, and everything built on the canonical layout assumes it is not
// there: ORDER BY, MIN()/MAX() and the date predicate all compare this text,
// and search_period slices its calendar date out by character position.
//
// The seeded value is one SQLite's date() reads perfectly well. That is
// exactly what makes it dangerous: nothing else in the archive notices.
func TestCheckTimestampLayout_SeesAReadableButNonCanonicalRow(t *testing.T) {
	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = database.Close() }()

	seedEveryDatetimeColumn(t, database, time.Date(2026, 8, 14, 20, 15, 0, 134000000, time.UTC))

	const odd = "2026-08-14 20:15:00"
	if _, err := database.Exec("UPDATE turns SET timestamp = ? WHERE id = 'turn-1'", odd); err != nil {
		t.Fatalf("seed a non-canonical turn: %v", err)
	}

	// It is readable. The existing checks have nothing to say about it.
	var readable any
	if err := database.QueryRow("SELECT date(timestamp) FROM turns WHERE id = 'turn-1'").Scan(&readable); err != nil {
		t.Fatalf("date() on the seeded value: %v", err)
	}
	if readable == nil {
		t.Fatal("the seeded value is unreadable, so it proves nothing about readable-but-non-canonical rows")
	}

	report, err := database.CheckTimestampLayout()
	if err != nil {
		t.Fatalf("check timestamp layout: %v", err)
	}
	if report.OK() {
		t.Fatal("a non-canonical turns.timestamp was not reported")
	}
	if report.OffLayout() != 1 {
		t.Errorf("OffLayout() = %d, want 1", report.OffLayout())
	}

	findings := report.Findings()
	if len(findings) != 1 {
		t.Fatalf("Findings() = %v, want exactly turns.timestamp", findings)
	}
	got := findings[0]
	if got.Table != "turns" || got.Column != "timestamp" {
		t.Errorf("reported %s.%s, want turns.timestamp", got.Table, got.Column)
	}
	if got.Sample != odd {
		t.Errorf("Sample = %q, want %q — the report has to show the value, "+
			"or the user has no way to judge whether it is a problem", got.Sample, odd)
	}
	if got.Rows < 1 {
		t.Errorf("Rows = %d, want the column's non-NULL total", got.Rows)
	}
}

// TestCheckTimestampLayout_CountsTheLegacyEra. An archive still on the pre-014
// rendering is entirely non-canonical, and the report should say so for every
// column rather than for the one that happens to be checked first.
func TestCheckTimestampLayout_CountsTheLegacyEra(t *testing.T) {
	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = database.Close() }()

	utc := time.Date(2026, 8, 14, 20, 15, 0, 134000000, time.UTC)
	seedEveryDatetimeColumn(t, database, utc)
	if _, err := database.Exec("UPDATE turns SET timestamp = ?", legacyRendering(utc)); err != nil {
		t.Fatalf("seed legacy turns: %v", err)
	}
	if _, err := database.Exec("UPDATE source_files SET synced_at = ?", legacyRendering(utc)); err != nil {
		t.Fatalf("seed legacy source_files: %v", err)
	}

	report, err := database.CheckTimestampLayout()
	if err != nil {
		t.Fatalf("check timestamp layout: %v", err)
	}
	if report.OffLayout() != 2 {
		t.Errorf("OffLayout() = %d, want 2", report.OffLayout())
	}

	var named []string
	for _, f := range report.Findings() {
		named = append(named, f.Table+"."+f.Column)
	}
	want := "source_files.synced_at turns.timestamp"
	if got := strings.Join(sortedCopy(named), " "); got != want {
		t.Errorf("Findings() named %q, want %q", got, want)
	}
}

// TestCanonicalTimestampGlob_AgreesWithTheLayout keeps the pattern and the
// rendering from drifting apart. The glob is a second description of
// timestampLayout, and a second description is a thing that can disagree.
func TestCanonicalTimestampGlob_AgreesWithTheLayout(t *testing.T) {
	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = database.Close() }()

	matches := func(s string) bool {
		var ok int
		if err := database.QueryRow("SELECT ? GLOB ?", s, canonicalTimestampGlob).Scan(&ok); err != nil {
			t.Fatalf("glob %q: %v", s, err)
		}
		return ok == 1
	}

	for _, instant := range []time.Time{
		time.Date(2026, 8, 14, 20, 15, 0, 134000000, time.UTC),
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),                       // a whole second, padded
		time.Date(2026, 12, 31, 23, 59, 59, 999999999, time.UTC),          // every digit used
		time.Date(2026, 8, 14, 15, 15, 0, 1, time.FixedZone("x", -18000)), // normalised out of an offset
	} {
		if rendered := FormatTimestamp(instant); !matches(rendered) {
			t.Errorf("FormatTimestamp(%v) = %q, which the canonical glob rejects", instant, rendered)
		}
	}

	// Everything ParseTimestamp will read but FormatTimestamp would never
	// write. These are the values the report exists to surface.
	for _, off := range []string{
		"2026-08-14 20:15:00.134 +0000 UTC",
		"2026-08-14T20:15:00.134Z", // RFC3339Nano, fraction trimmed
		"2026-08-14T20:15:00Z",     // the ordering trap from #142
		"2026-08-14 20:15:00",
		"2026-08-14",
		"2026-08-14T20:15:00.134000000+00:00", // canonical instant, wrong spelling
	} {
		if matches(off) {
			t.Errorf("the canonical glob accepts %q, which FormatTimestamp does not produce", off)
		}
	}
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}
