// ABOUTME: Decomposes turn-count drift into named causes (#121) — transcript gone, cannot be stored, not re-parsed yet.
// ABOUTME: One undifferentiated number made a warning that could never be cleared, so it stopped being read at all.

package db

import (
	"database/sql"
	"fmt"
	"os"
	"time"
)

// DriftCause names why one session's turn counter disagrees with its rows.
//
// The distinction is the whole of #121. Before it, `stats` reported a single
// figure with a single instruction — "recover the missing turns with
// `ccvault sync --full`" — and on the author's archive that instruction could
// not be satisfied: 8,694 of the 8,716 drifted turns belong to transcripts
// that are no longer on disk, and the remaining 22 are lines no re-parse can
// store. A warning that cannot be cleared is printed, ignored, and then
// ignored again on the day it reports something real.
type DriftCause int

const (
	// DriftTranscriptGone is a session whose source file is not on disk. Its
	// turns were deleted from the archive and the only thing that could bring
	// them back is gone, so nothing — not `sync --full`, not a migration —
	// will ever recover them. A session recording no source file at all lands
	// here too: an empty path is not a transcript anybody can re-read.
	DriftTranscriptGone DriftCause = iota

	// DriftUnstorable is a session whose transcript is on disk and has
	// already been parsed at the revision now on disk. Re-reading it produces
	// the rows it produced last time, so the shortfall is not missing data
	// waiting to be recovered — it is lines that cannot be stored.
	//
	// #121's own case lands here: a subagent transcript of 125 lines carrying
	// 103 distinct uuids stores 103 turns, because within one session two
	// lines sharing a uuid are the same (session_id, id) key. Lines a parse
	// skipped as malformed land here too, and the report says the same true
	// thing about both — the transcript as it stands yields these rows.
	DriftUnstorable

	// DriftUnparsed is a session whose transcript is on disk and has not been
	// parsed at its current revision — either it changed since the last sync
	// or its source_files row is absent, which is the state migration 012
	// deliberately left the damaged sessions in so an ordinary sync would
	// re-read exactly them.
	//
	// This is the only cause with an instruction attached, and the only one
	// that should ever be loud.
	DriftUnparsed

	// DriftUnclassified is a drifted session past driftClassifyLimit. It is
	// reported rather than dropped, and counts as actionable, because nothing
	// has established that it is not.
	DriftUnclassified
)

// String names a cause for a %v in a test failure. The report in
// cmd/ccvault writes its own sentences — a cause needs an explanation, not a
// label.
func (c DriftCause) String() string {
	switch c {
	case DriftTranscriptGone:
		return "transcript-gone"
	case DriftUnstorable:
		return "unstorable"
	case DriftUnparsed:
		return "unparsed"
	case DriftUnclassified:
		return "unclassified"
	}
	return fmt.Sprintf("DriftCause(%d)", int(c))
}

// driftClassifyLimit is how many drifted sessions get a cause.
//
// Classifying one costs a stat of its transcript, so the bound is what keeps a
// `stats` run from turning into one syscall per session on an archive where
// everything drifted. It is free on a healthy archive — there is nothing to
// classify — and generous against every drift this project has measured: 201
// sessions at its worst (#92, before migration 012), 4 today. An archive past
// the bound has a problem a finer breakdown would not help with.
const driftClassifyLimit = 1000

// DriftByCause is one cause and the sessions it accounts for.
//
// Claimed and Rows rather than a single difference, because that is the shape
// the top-level figures already use and because the direction matters: a
// counter *below* its rows is just as much a sign the two came apart, and a
// subtraction would hide it inside a smaller number.
type DriftByCause struct {
	Cause    DriftCause
	Sessions int
	// Claimed is SUM(turn_count) over this cause's sessions.
	Claimed int
	// Rows is how many turns those sessions actually hold.
	Rows int
}

// TurnDrift is what sessions.turn_count claims against what turns holds.
type TurnDrift struct {
	// Counted is SUM(sessions.turn_count) — what the sessions say they hold.
	Counted int
	// Rows is COUNT(*) FROM turns — what the archive actually holds.
	Rows int
	// Sessions is how many session rows disagree with their own turns, in
	// either direction.
	Sessions int
	// Causes accounts for those sessions, most-unrecoverable first, holding
	// only the causes with sessions in them. Empty on a healthy archive.
	Causes []DriftByCause
}

// Drifted reports whether the counter and the rows disagree at all.
func (d TurnDrift) Drifted() bool {
	return d.Counted != d.Rows || d.Sessions > 0
}

// Actionable reports whether any of the drift is something a user can repair.
//
// This is what #121 asks for. `Drifted` stays as it was — the accounting is
// still worth printing, and `stats --json` still reports it — but the
// instruction to run a recovery is gated on this, so an archive whose
// remaining drift is all accounted for stops being told to run a command that
// cannot help it. On the author's archive that is the difference between a
// permanent warning and a closed book.
func (d TurnDrift) Actionable() bool {
	for _, c := range d.Causes {
		if c.Cause == DriftUnparsed || c.Cause == DriftUnclassified {
			return true
		}
	}
	return false
}

// driftedSession is one row of the drift query, before its cause is decided.
type driftedSession struct {
	id         string
	sourceFile string
	claimed    int
	rows       int
	// parsedMtime is the mtime sync recorded for sourceFile, zero when it
	// recorded none.
	parsedMtime time.Time
}

// TurnCountDrift measures sessions.turn_count against the turns table, and
// says why each disagreement exists.
//
// #91 asks for the discrepancy to be surfaced rather than quietly corrected,
// in the same shape `stats` already uses for reclaimable storage. The reason
// is that the drift is not a cosmetic accounting error: a session claiming
// more turns than it holds means its rows were deleted, which is what #92
// was, and a visible disagreement is strictly better than a confident wrong
// number.
//
// #121 adds the causes, because a figure with one instruction attached is only
// honest while the instruction can work. Each drifted session is classified
// from its transcript: absent from disk, present and already parsed at the
// revision on disk, or present and not yet parsed at it. The last of those is
// the only one a sync repairs, and the only one the report tells anybody to
// act on. Classification does not re-read a transcript — it stats it, and
// compares against what sync recorded, using the same comparison needsSync
// makes.
//
// No migration and no new column. The archive already holds everything the
// split needs; what was missing was the question.
//
// Both directions of drift count, in the totals and in the causes. A counter
// below the rows is just as much a sign the two have come apart, and
// reporting only the over-count would hide it.
func (db *DB) TurnCountDrift() (TurnDrift, error) {
	var d TurnDrift

	// The two archive-wide totals, which are cheap aggregate scans and the
	// figures #91 put on the report.
	err := db.QueryRow(`
		SELECT
			(SELECT COALESCE(SUM(COALESCE(turn_count, 0)), 0) FROM sessions),
			(SELECT COUNT(*) FROM turns)
	`).Scan(&d.Counted, &d.Rows)
	if err != nil {
		return TurnDrift{}, fmt.Errorf("measure turn count drift: %w", err)
	}

	// The drifted sessions themselves. One pass with the correlated count,
	// which is the same pass the pre-#121 version made to produce its single
	// Sessions figure — so naming the causes costs no extra scan of sessions,
	// only a stat per drifted session.
	//
	// One row past the bound is read so an archive that exceeds it can be
	// told apart from one that lands exactly on it.
	drifted, err := db.driftedSessions(driftClassifyLimit + 1)
	if err != nil {
		return TurnDrift{}, err
	}

	if len(drifted) <= driftClassifyLimit {
		d.Sessions = len(drifted)
	} else {
		// Past the bound the list is truncated, so the true count has to be
		// asked for. The rare path on an archive that is already in trouble.
		if err := db.QueryRow(`
			SELECT COUNT(*) FROM sessions s
			WHERE COALESCE(s.turn_count, 0) <>
			      (SELECT COUNT(*) FROM turns t WHERE t.session_id = s.id)
		`).Scan(&d.Sessions); err != nil {
			return TurnDrift{}, fmt.Errorf("count drifted sessions: %w", err)
		}
	}

	d.Causes = classifyDrift(drifted, d.Sessions)
	return d, nil
}

// driftedSessions reads up to limit sessions whose counter disagrees with
// their rows, with what sync last recorded about their transcript.
func (db *DB) driftedSessions(limit int) ([]driftedSession, error) {
	// LEFT JOIN on both halves of source_files' key, (path, source): two
	// sources can hold the same path, and matching on path alone would pick
	// up the other source's watermark.
	rows, err := db.Query(`
		SELECT
			s.id,
			COALESCE(s.source_file, ''),
			COALESCE(s.turn_count, 0) AS claimed,
			(SELECT COUNT(*) FROM turns t WHERE t.session_id = s.id) AS held,
			sf.mtime
		FROM sessions s
		LEFT JOIN source_files sf
			ON sf.path = s.source_file AND sf.source = s.source
		WHERE COALESCE(s.turn_count, 0) <>
		      (SELECT COUNT(*) FROM turns t WHERE t.session_id = s.id)
		ORDER BY s.id
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("read drifted sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []driftedSession
	for rows.Next() {
		var s driftedSession
		var mtime sql.NullTime
		if err := rows.Scan(&s.id, &s.sourceFile, &s.claimed, &s.rows, &mtime); err != nil {
			return nil, fmt.Errorf("scan drifted session: %w", err)
		}
		if mtime.Valid {
			s.parsedMtime = mtime.Time
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read drifted sessions: %w", err)
	}
	return out, nil
}

// classifyDrift gives each drifted session a cause and sums them, in the fixed
// order the causes are declared in — unrecoverable first, then unstorable,
// then the one a sync repairs, then whatever the bound could not reach.
//
// A fixed order rather than map iteration: the section is printed on every
// `stats` run, and a breakdown that reshuffles itself between runs cannot be
// diffed.
//
// totalDrifted is the true number of drifted sessions, which exceeds len(d)
// when the classification bound was hit; the remainder becomes
// DriftUnclassified.
func classifyDrift(drifted []driftedSession, totalDrifted int) []DriftByCause {
	if totalDrifted == 0 {
		return nil
	}

	classified := drifted
	if len(classified) > driftClassifyLimit {
		classified = classified[:driftClassifyLimit]
	}

	byCause := map[DriftCause]*DriftByCause{}
	add := func(cause DriftCause, sessions, claimed, rows int) {
		c, ok := byCause[cause]
		if !ok {
			c = &DriftByCause{Cause: cause}
			byCause[cause] = c
		}
		c.Sessions += sessions
		c.Claimed += claimed
		c.Rows += rows
	}

	for _, s := range classified {
		add(driftCauseOf(s), 1, s.claimed, s.rows)
	}

	// The sessions past the bound. Their claimed and held figures were never
	// read, so the bucket reports only how many there are — a zero where a
	// number is unknown would be a claim, and the point of this report is to
	// stop making those.
	if unclassified := totalDrifted - len(classified); unclassified > 0 {
		add(DriftUnclassified, unclassified, 0, 0)
	}

	order := []DriftCause{DriftTranscriptGone, DriftUnstorable, DriftUnparsed, DriftUnclassified}
	out := make([]DriftByCause, 0, len(order))
	for _, cause := range order {
		if c, ok := byCause[cause]; ok {
			out = append(out, *c)
		}
	}
	return out
}

// driftCauseOf decides why one session drifted.
//
// The mtime comparison is needsSync's, inverted: sync re-reads a transcript
// when its mtime is *after* the one recorded, so a transcript whose mtime is
// not after it has already been parsed exactly as it now stands, and reading
// it again cannot produce different rows. Deliberately the same comparison and
// not a near-copy of it — a classification that disagreed with the sync about
// which files are stale would mislabel precisely the sessions it exists to
// explain.
func driftCauseOf(s driftedSession) DriftCause {
	if s.sourceFile == "" {
		return DriftTranscriptGone
	}

	info, err := os.Stat(s.sourceFile)
	if err != nil {
		// Any reason the transcript cannot be stat'd is "not there to
		// re-read": absent, on an unmounted volume, unreadable directory. The
		// report says what that means for recovery, which is the same thing
		// in every case.
		return DriftTranscriptGone
	}

	if s.parsedMtime.IsZero() || info.ModTime().After(s.parsedMtime) {
		return DriftUnparsed
	}
	return DriftUnstorable
}
