// ABOUTME: The one rendering every DATETIME column in the archive holds, and the parse that reads it back
// ABOUTME: RFC3339 in UTC with a fixed-width nanosecond field — see the comment on timestampLayout for why each part

package db

import (
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

// ParseTimestamp reads a stored timestamp back, in UTC.
//
// Tolerant on input and canonical on output. An archive holds whatever every
// ccvault that ever wrote to it put there, and migration 014 converts the rows
// it recognises rather than every string that has ever been in a DATETIME
// column — so a reader cannot assume the canonical layout even on a migrated
// archive.
//
// Most reads never come here: the driver parses any column declared DATETIME,
// DATE or TIMESTAMP on the way out. This is for the values that arrive as text
// anyway — SQLite aggregates like MIN()/MAX(), whose result column has no
// declared type.
func ParseTimestamp(s string) (time.Time, error) {
	// Go's time.Time.String() appends the monotonic clock reading when the
	// value came from time.Now(), e.g. " m=+0.003560667". No layout matches
	// it, and it carries no information a stored timestamp needs.
	if idx := strings.Index(s, " m="); idx > 0 {
		s = s[:idx]
	}

	for _, layout := range []string{
		timestampLayout,
		time.RFC3339Nano, // also covers "2006-01-02T15:04:05.999999999Z07:00"
		"2006-01-02 15:04:05.999999999 -0700 MST", // time.Time.String()
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999Z",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("parse timestamp %q: no known layout matches", s)
}
