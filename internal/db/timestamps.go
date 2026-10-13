// ABOUTME: The one rendering every DATETIME column in the archive holds, and the parse that reads it back
// ABOUTME: RFC3339 in UTC with a fixed-width nanosecond field — see the comment on timestampLayout for why each part

package db

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// timestampLayout is the rendering every DATETIME column holds: RFC3339, in
// UTC, with the nanosecond field padded rather than trimmed.
//
// Three decisions, each load-bearing.
//
// **SQLite has to be able to read it.** That is the whole of #33. The columns
// are declared DATETIME and held Go's time.Time.String() —
// "2026-01-14 20:15:00.134 +0000 UTC" — whose trailing zone name SQLite's
// parser cannot get past, so date(), datetime(), strftime() and julianday()
// returned NULL for all 1,049,544 turns and all 47,409 sessions of the
// author's archive. Nothing in ccvault noticed, because nothing in ccvault
// called a date function; everything *else* that touches the archive did,
// silently, by getting a single NULL bucket back from a GROUP BY.
//
// **UTC, always.** That is the other half of #90. The date-filter predicate is
// `t.timestamp > $1`, a comparison SQLite resolves on the text; a bound and a
// stored value carrying different offsets compare on their wall-clock digits
// and stop long before the offset is reached. Rendering both sides in UTC is
// what makes that comparison a comparison of instants. It also makes
// lexicographic order chronological order, which ORDER BY t.timestamp,
// MIN()/MAX() over the column, and merge.go's ended_at comparison all depend
// on.
//
// **A fixed-width fraction.** #33 proposed time.RFC3339Nano, and RFC3339Nano
// would be wrong here: it trims trailing zeros, so a whole second renders
// "…:05Z" and a tenth past it "…:05.1Z" — and '.' (0x2E) sorts before 'Z'
// (0x5A), putting the later instant first. Nine digits, always, costs six
// bytes a row and makes "sorts as text" true unconditionally rather than for
// the values that happen to have the same precision.
//
// What the layout deliberately does *not* change is where the calendar date
// sits. turns.search_period and tool_uses.search_period are generated columns
// that slice the year, month and day out of this text with
// substr(ts,1,4)/(6,2)/(9,2), guarded by a GLOB on the same positions, and
// both FTS indexes hold the result. Characters 1-10 are "YYYY-MM-DD" in this
// layout exactly as they were in time.Time.String(), so migration 014 is a
// storage change that both million-document indexes do not notice.
const timestampLayout = "2006-01-02T15:04:05.000000000Z"

// FormatTimestamp renders an instant the way every DATETIME column stores it.
//
// .UTC() both normalises the zone and drops the monotonic clock reading that
// time.Now() carries, which is what put " m=+4.493169084" in
// source_files.synced_at and sessions.source_mtime (#33's third bullet).
func FormatTimestamp(t time.Time) string {
	return t.UTC().Format(timestampLayout)
}

// acceptedLayouts is every rendering a DATETIME column in some ccvault
// archive, somewhere, might hold — the single list, for every package.
//
// Leniency here is deliberate and is not the same thing as having two lists.
// What ccvault *writes* is timestampLayout and nothing else; what it must
// *read* is whatever every ccvault that ever wrote to this file put there,
// plus whatever an import carried in. Narrowing this list does not make the
// archive more canonical, it makes rows unreadable. The bug #143 names is two
// packages maintaining their own copies of it, so a value parsed in one and
// not the other and a search silently missed rows.
//
// Order is a fast path, not semantics: the canonical layout is tried first
// because, after migration 014, it is what essentially every row holds.
var acceptedLayouts = []string{
	// What ccvault writes today, and the only thing it writes. Subsumed by
	// RFC3339Nano below, but first because it is the overwhelming majority.
	timestampLayout,

	// Any ISO-8601/RFC3339 spelling with a T separator: trimmed fractions
	// ("…:05Z"), full ones, and any numeric offset. Covers time.RFC3339,
	// which is this layout with the fraction absent, so it needs no entry of
	// its own. This is what a foreign archive or another tool most plausibly
	// wrote.
	time.RFC3339Nano,

	// Go's time.Time.String(), with the zone abbreviation. Every DATETIME
	// column in the author's archive held this before migration 014, and any
	// archive not yet migrated still does.
	"2006-01-02 15:04:05.999999999 -0700 MST",

	// time.Time.String() again, for an instant whose location is an unnamed
	// fixed zone: Go prints the numeric offset and then has no abbreviation
	// to append. Inherited from internal/search's list, which had it and
	// db's did not — exactly the divergence #143 is about.
	"2006-01-02 15:04:05.999999999 -0700",

	// The SQLite driver's own "sqlite" time rendering (see the note in
	// db.go), in its three zone spellings: numeric offset, Z, and absent.
	// The last is what a hand-written INSERT or a sqlite3 shell session
	// leaves behind, and SQLite reads it as UTC — so Parse does too, which
	// is what omitting a zone from a layout means in Go.
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02 15:04:05.999999999Z",
	"2006-01-02 15:04:05.999999999",

	// Date only, midnight UTC. A column written by a date() expression, or a
	// row repaired by hand.
	"2006-01-02",
}

// ParseTimestamp reads a stored timestamp back, in UTC.
//
// Tolerant on input and canonical on output. An archive holds whatever every
// ccvault that ever wrote to it put there, and migration 014 converts the rows
// it recognises rather than every string that has ever been in a DATETIME
// column — so a reader cannot assume the canonical layout even on a migrated
// archive.
//
// This is the only timestamp parse in the archive's read path, and
// TestTimestampLayoutEnforcement keeps it that way: a package that grows its
// own layout list accepts a different set of values than this one, and the
// symptom is not an error but a row that one query finds and another does not.
//
// Most reads never come here: the driver parses any column declared DATETIME,
// DATE or TIMESTAMP on the way out. This is for the values that arrive as text
// anyway — SQLite aggregates like MIN()/MAX(), whose result column has no
// declared type.
func ParseTimestamp(s string) (time.Time, error) {
	// Surrounding whitespace is not part of any layout and is not information.
	// internal/search trimmed before parsing and db did not, which is one more
	// way the two lists disagreed about what the archive could read.
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, errors.New("parse timestamp: empty")
	}

	// Go's time.Time.String() appends the monotonic clock reading when the
	// value came from time.Now(), e.g. " m=+0.003560667". No layout matches
	// it, and it carries no information a stored timestamp needs.
	if idx := strings.Index(s, " m="); idx > 0 {
		s = s[:idx]
	}

	for _, layout := range acceptedLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("parse timestamp %q: no known layout matches", s)
}

// ParseStoredTimestampOrZero is ParseTimestamp for the callers that must not
// fail a whole operation over one bad row.
//
// Search is the case this exists for (#85): a million-row archive with one
// unparseable timestamp in it should return the other results, with that row's
// time zero, rather than returning an error for the query. Separated from
// ParseTimestamp rather than folded into it so that the callers who do want to
// know — migrations, integrity checks — still get told.
func ParseStoredTimestampOrZero(s string) time.Time {
	t, err := ParseTimestamp(s)
	if err != nil {
		return time.Time{}
	}
	return t
}
