// ABOUTME: Compiles a search's date filters into FTS5 terms that prune inside the index
// ABOUTME: The terms name calendar periods; migration 010 stores the matching tokens per row

package search

import (
	"strings"
	"time"
)

// The period tokens, and the column they live in.
//
// Two granularities per row rather than one. A month term keeps a wide range
// short — a year is twelve terms instead of 365 — but it is five times coarser
// than a one-week window: measured on the author's archive, `after:2026-09-28`
// admits 16,687 turns while September and October together hold 89,071. A day
// term makes the pruned set the window itself. Carrying both lets a range use
// days at its partial edges and months through its whole interior, so the
// number of terms stays small and the set stays close to the window at every
// width.
//
// The prefixes exist so the tokens read as tokens in an EXPLAIN or a log, not
// so they cannot collide: every term is reachable by a user who types it,
// because unicode61 tokenizes a query the same way it tokenizes content. What
// makes collision impossible is that every MATCH this package builds is
// column-scoped — period terms are matched only against periodColumn, and the
// caller's own text only against the content columns. A message that happens to
// contain "ccvym202610" is findable by searching for it and contributes nothing
// to any date filter.
const (
	periodColumn      = "search_period"
	periodDayPrefix   = "ccvd"
	periodMonthPrefix = "ccvym"

	// periodUnknownTerm is what migration 010 stores for a row whose timestamp
	// is not a date it can read. Every compiled term set includes it, so such a
	// row stays a candidate and the outer timestamp predicate decides it, the
	// same as before this index existed. Without it, a row with an unreadable
	// timestamp would vanish from date-filtered searches — a wrong answer
	// rather than a slow one.
	periodUnknownTerm = "ccvymx"

	// periodDateLayout is the date half of both the text SQLite compares and
	// the token the index holds.
	periodDateLayout = "2006-01-02"

	// maxPeriodTerms caps the compiled expression. Beyond it the OR branch is
	// long enough to be its own cost and the range is wide enough that pruning
	// would save little — so periodTerms declines, the MATCH goes out without a
	// period clause, and the query costs what it costs today. 200 terms covers
	// any range up to about eleven years.
	maxPeriodTerms = 200
)

// periodFilterDate renders a date filter's bound as the calendar date SQLite
// will compare it against, or "" for an unset filter.
//
// In the bound's own location, deliberately. The driver binds a time.Time as
// its Go String() rendering and turns.timestamp holds the same rendering, so
// the filter is a lexicographic comparison of that text; `date(bound.UTC())`
// would name a different day than the one the comparison uses whenever the
// bound carries an offset, and a date-filtered search would lose the rows in
// between.
func periodFilterDate(bound time.Time) string {
	if bound.IsZero() {
		return ""
	}
	return bound.Format(periodDateLayout)
}

// periodTerms compiles an inclusive range of calendar dates into the period
// terms that cover it, or nil to decline pruning.
//
// The terms cover the range exactly: whole months collapse to a month term and
// the partial month at either end is spelled out a day at a time. Nothing
// outside [from, to] is admitted and nothing inside it is excluded, so the
// MATCH prunes to the window's own size while the caller's timestamp predicate
// still trims the two partial days at the edges to the hour.
//
// nil is the safe answer and is returned for anything this cannot compile: an
// unreadable bound, an inverted range, or a range so wide the expression would
// cost more than it saves. A nil term set means the search runs exactly as it
// did before this index existed.
func periodTerms(from, to string) []string {
	start, err := time.ParseInLocation(periodDateLayout, from, time.UTC)
	if err != nil {
		return nil
	}
	end, err := time.ParseInLocation(periodDateLayout, to, time.UTC)
	if err != nil {
		return nil
	}
	if start.After(end) {
		return nil
	}

	var terms []string
	for d := start; !d.After(end); {
		// A month collapses only when the cursor sits on its first day and its
		// last day is still inside the range. Checking the month's own end
		// rather than counting days is what makes February and leap years fall
		// out rather than needing a case.
		if d.Day() == 1 {
			if monthEnd := d.AddDate(0, 1, -1); !monthEnd.After(end) {
				terms = append(terms, periodMonthPrefix+d.Format("200601"))
				d = d.AddDate(0, 1, 0)
				continue
			}
		}
		terms = append(terms, periodDayPrefix+d.Format("20060102"))
		d = d.AddDate(0, 0, 1)

		if len(terms) > maxPeriodTerms {
			return nil
		}
	}

	return append(terms, periodUnknownTerm)
}

// ftsMatchExpr builds a MATCH expression for one index: the caller's text
// scoped to that index's content columns, and the period terms scoped to the
// period column.
//
// Both halves are scoped. Scoping the period terms is what keeps a row whose
// content happens to hold a period token out of a date filter; scoping the
// caller's text is what keeps a caller who searches for a period token from
// matching every row of that month through the index's own bookkeeping.
//
// The caller's text comes first so that it drives the intersection. FTS5 walks
// an AND by advancing its first child and seeking the rest to that rowid, and
// the period branch is the wide one — it names every document in the window.
// With the text first the period branch is only ever seeked, never scanned.
func ftsMatchExpr(contentColumns []string, text string, periods []string) string {
	expr := "{" + strings.Join(contentColumns, " ") + "} : (" + text + ")"
	if len(periods) == 0 {
		return expr
	}
	return expr + " AND {" + periodColumn + "} : (" + strings.Join(periods, " OR ") + ")"
}
