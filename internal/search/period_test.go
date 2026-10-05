// ABOUTME: Tests the date-range to FTS5-period-term compilation
// ABOUTME: Covers the day/month decomposition, its boundaries, and the cases that decline to prune

package search

import (
	"strings"
	"testing"
	"time"
)

func TestPeriodTerms(t *testing.T) {
	tests := []struct {
		name string
		from string
		to   string
		want []string
	}{
		{
			name: "a single day is one day term",
			from: "2026-10-05", to: "2026-10-05",
			want: []string{"ccvd20261005", "ccvymx"},
		},
		{
			name: "a few days inside one month stay days",
			from: "2026-09-28", to: "2026-09-30",
			want: []string{"ccvd20260928", "ccvd20260929", "ccvd20260930", "ccvymx"},
		},
		{
			// The issue's own example: last week, spanning a month boundary.
			// Neither month is whole, so neither collapses.
			name: "a range across a month boundary is days on both sides",
			from: "2026-09-28", to: "2026-10-02",
			want: []string{
				"ccvd20260928", "ccvd20260929", "ccvd20260930",
				"ccvd20261001", "ccvd20261002",
				"ccvymx",
			},
		},
		{
			name: "a whole month collapses to one month term",
			from: "2026-09-01", to: "2026-09-30",
			want: []string{"ccvym202609", "ccvymx"},
		},
		{
			name: "a whole month plus a tail is one month term and days",
			from: "2026-09-01", to: "2026-10-02",
			want: []string{"ccvym202609", "ccvd20261001", "ccvd20261002", "ccvymx"},
		},
		{
			name: "a leap February collapses on the 29th",
			from: "2024-02-01", to: "2024-02-29",
			want: []string{"ccvym202402", "ccvymx"},
		},
		{
			// 2025 is not a leap year, so the 28th is the whole month. Getting
			// this wrong in the other direction would emit 28 day terms, which
			// is slower but still correct — so the assertion is on the terms,
			// not on the rows a search returns.
			name: "a common February collapses on the 28th",
			from: "2025-02-01", to: "2025-02-28",
			want: []string{"ccvym202502", "ccvymx"},
		},
		{
			name: "a month one day short of whole stays days",
			from: "2026-09-01", to: "2026-09-29",
			want: append(dayTermRange(t, "2026-09-01", "2026-09-29"), "ccvymx"),
		},
		{
			name: "an archive-width range mixes days and whole months",
			from: "2026-01-14", to: "2026-10-05",
			want: append(append(
				dayTermRange(t, "2026-01-14", "2026-01-31"),
				"ccvym202602", "ccvym202603", "ccvym202604", "ccvym202605",
				"ccvym202606", "ccvym202607", "ccvym202608", "ccvym202609"),
				append(dayTermRange(t, "2026-10-01", "2026-10-05"), "ccvymx")...),
		},
		{
			name: "an inverted range prunes nothing",
			from: "2026-10-05", to: "2026-10-01",
			want: nil,
		},
		{
			name: "a malformed bound prunes nothing",
			from: "last tuesday", to: "2026-10-01",
			want: nil,
		},
		{
			name: "an empty bound prunes nothing",
			from: "", to: "2026-10-01",
			want: nil,
		},
		{
			// Wider than the term cap: pruning is declined rather than
			// compiled into an expression with hundreds of OR branches. The
			// outer timestamp predicate still filters, so the answer stays
			// right and only the cost reverts to today's.
			name: "a range too wide to compile prunes nothing",
			from: "1990-01-01", to: "2026-10-05",
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := periodTerms(tc.from, tc.to)
			if len(got) != len(tc.want) {
				t.Fatalf("periodTerms(%q, %q) returned %d terms, want %d\ngot:  %v\nwant: %v",
					tc.from, tc.to, len(got), len(tc.want), got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("term %d = %q, want %q (full: %v)", i, got[i], tc.want[i], got)
				}
			}
		})
	}
}

// dayTermRange builds the day terms for an inclusive date range, so a test
// expectation can name a long run of days without spelling each one.
func dayTermRange(t *testing.T, from, to string) []string {
	t.Helper()

	start, err := time.Parse(periodDateLayout, from)
	if err != nil {
		t.Fatalf("parse %q: %v", from, err)
	}
	end, err := time.Parse(periodDateLayout, to)
	if err != nil {
		t.Fatalf("parse %q: %v", to, err)
	}

	var terms []string
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		terms = append(terms, periodDayPrefix+d.Format("20060102"))
	}
	return terms
}

// TestPeriodTermsAreTheTokensTheIndexHolds is the tie between the Go side and
// migration 010's generated column. The two derive the same token from the same
// date by different routes — Go formatting here, SQL string slicing there — and
// a disagreement would not fail to compile or to run. It would just silently
// return no rows for a date-filtered search.
//
// This test states the shape both sides must produce; TestSearchPeriod_ColumnHoldsTheTokens
// in internal/db checks the SQL side against the same constants.
func TestPeriodTermsAreTheTokensTheIndexHolds(t *testing.T) {
	terms := periodTerms("2026-10-05", "2026-10-05")
	if len(terms) != 2 {
		t.Fatalf("got %v, want one day term plus the unknown-period sentinel", terms)
	}
	if terms[0] != "ccvd20261005" {
		t.Errorf("day term = %q, want ccvd20261005 — the shape migration 010 stores", terms[0])
	}
	if terms[1] != periodUnknownTerm {
		t.Errorf("last term = %q, want the sentinel %q", terms[1], periodUnknownTerm)
	}
	for _, term := range terms {
		if strings.ContainsAny(term, " \t\"'()*^-") {
			t.Errorf("term %q holds a character FTS5 would treat as syntax, so it cannot be used bare in a MATCH", term)
		}
	}
}

// TestPeriodFilterDate pins how a date filter's bound becomes a calendar date.
// It has to be the date SQLite will compare, which is the date in the bound's
// own location — the driver binds a time.Time as its Go String() rendering, and
// turns.timestamp holds the same rendering, so the comparison is on that text.
// Converting to UTC first would shift the bound by the offset and could drop a
// whole day of results at the edge of the window.
func TestPeriodFilterDate(t *testing.T) {
	plus14 := time.FixedZone("plus14", 14*60*60)
	// 2026-10-05 01:00 +14:00 is 2026-10-04 11:00 UTC: a different date.
	bound := time.Date(2026, 10, 5, 1, 0, 0, 0, plus14)

	if got := periodFilterDate(bound); got != "2026-10-05" {
		t.Errorf("periodFilterDate(%s) = %q, want 2026-10-05 — the date in the bound's own zone, which is what the driver binds and SQLite compares",
			bound, got)
	}
	if got := periodFilterDate(time.Time{}); got != "" {
		t.Errorf("periodFilterDate(zero) = %q, want empty — an unset filter names no date", got)
	}
}
