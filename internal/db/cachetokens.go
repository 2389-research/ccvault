// ABOUTME: The archive's side of #155's backfill — which sessions still read zero, and the stored lines to re-derive them from.
// ABOUTME: Holds no per-source token rule: the rule lives in the adapters, and this only supplies and stores bytes.

package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
)

// ErrNoStoredLine is returned by a StoredTranscript whose session holds a turn
// with no raw_json at all.
//
// 216,978 turns across 42 sessions of the author's archive are in that state
// deliberately: the bytes were provably unrelated input and the transcripts
// are gone, so they were nulled rather than left to be read as the session's
// own content (#101). Those sessions can never be re-derived, which is a skip
// and not a failure — the sentinel is here so the caller can tell the two
// apart rather than treating "unrepairable" as "the run went wrong".
var ErrNoStoredLine = errors.New("turn has no stored transcript line")

// CacheTokenCoverage is how much of the archive carries the prompt-cache
// counters, and how much still reads zero.
type CacheTokenCoverage struct {
	// Sessions is every session row.
	Sessions int
	// MissingCache is sessions whose cache_read and cache_write are both zero.
	MissingCache int
	// MissingWithCost is the subset of those that counted input or output
	// tokens. That is the figure worth acting on: a session with no tokens of
	// any kind is either empty or came from a source that records none (jeff
	// and hex record none at all), whereas a session that cost input and
	// output tokens and claims zero cache tokens is the understatement #155
	// is about.
	MissingWithCost int
	// CacheTokens is the cache total the archive currently reports.
	CacheTokens int64
}

// Complete reports whether every session that cost anything carries the cache
// pair.
func (c CacheTokenCoverage) Complete() bool {
	return c.MissingWithCost == 0
}

// CacheTokenCoverage counts the sessions still missing their cache counters.
//
// Cheap on purpose — sessions only, no raw_json — because this is reported by
// `stats` on every run, and a check the default report cannot afford is a
// check nobody sees. The expensive half is the repair itself, which has to
// read every stored line.
func (db *DB) CacheTokenCoverage() (CacheTokenCoverage, error) {
	var c CacheTokenCoverage
	err := db.QueryRow(`
		SELECT
			COUNT(*),
			COALESCE(SUM(COALESCE(cache_read_tokens, 0) = 0
				AND COALESCE(cache_write_tokens, 0) = 0), 0),
			COALESCE(SUM(COALESCE(cache_read_tokens, 0) = 0
				AND COALESCE(cache_write_tokens, 0) = 0
				AND COALESCE(input_tokens, 0) + COALESCE(output_tokens, 0) > 0), 0),
			COALESCE(SUM(COALESCE(cache_read_tokens, 0) + COALESCE(cache_write_tokens, 0)), 0)
		FROM sessions`).Scan(&c.Sessions, &c.MissingCache, &c.MissingWithCost, &c.CacheTokens)
	if err != nil {
		return CacheTokenCoverage{}, fmt.Errorf("measure cache token coverage: %w", err)
	}
	return c, nil
}

// CacheBackfillCandidate is one session the cache-token backfill should look
// at, with the counters it currently claims.
type CacheBackfillCandidate struct {
	ID string
	// Source names the adapter that first read this session, and so the one
	// whose rules apply to re-reading it.
	Source string
	// SourceFile is the path the transcript came from. The file is usually
	// gone — that is why the backfill exists — but adapters answer some
	// questions from the shape of a path rather than from the bytes, so it is
	// passed back to them unopened.
	SourceFile string
	// StoredInput and StoredOutput are what the session row counts today.
	// They were counted correctly from the start, which is what makes them
	// usable as a checksum: a re-derivation that does not reproduce them has
	// not read the whole session.
	StoredInput  int64
	StoredOutput int64
}

// CacheBackfillCandidates lists every session whose cache counters are both
// zero, newest first.
//
// Zero is the key, for two reasons. It needs no schema and no bookkeeping
// table, so the run is resumable by construction — a session repaired by an
// interrupted pass is not a candidate for the next one. And it is the same
// condition `stats` reports, so the number a user is told and the number the
// repair acts on cannot drift.
//
// What zero cannot distinguish is "never recorded" from "genuinely spent no
// cache tokens", which is a real state for a short session. Those sessions are
// therefore re-examined on every run and reported separately from the ones
// repaired — the alternative, a marker that says "looked at this one", would
// also freeze the answer an adapter fix might later change.
//
// Newest first because a run interrupted halfway should have repaired the
// sessions anyone is most likely to be looking at.
func (db *DB) CacheBackfillCandidates() ([]CacheBackfillCandidate, error) {
	rows, err := db.Query(`
		SELECT id, source, source_file,
		       COALESCE(input_tokens, 0), COALESCE(output_tokens, 0)
		FROM sessions
		WHERE COALESCE(cache_read_tokens, 0) = 0
		  AND COALESCE(cache_write_tokens, 0) = 0
		ORDER BY started_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list cache backfill candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var candidates []CacheBackfillCandidate
	for rows.Next() {
		var c CacheBackfillCandidate
		if err := rows.Scan(&c.ID, &c.Source, &c.SourceFile, &c.StoredInput, &c.StoredOutput); err != nil {
			return nil, fmt.Errorf("scan cache backfill candidate: %w", err)
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list cache backfill candidates: %w", err)
	}
	return candidates, nil
}

// StoredTranscript streams a session's stored transcript lines back as the
// JSONL the source wrote, one line per turn in ordinal order.
//
// turns.raw_json holds the complete line, not a fragment of it, which is what
// makes this possible: the archive is its own transcript store once the files
// under ~/.claude are gone. Ordinal order is file order — the parser numbers
// the turns it keeps, so a line it could not use leaves no hole — so the
// reassembled stream is the original file minus any line that was never a
// turn.
//
// Streamed rather than assembled in memory because a single session in the
// author's archive holds 56,000 turns, and because the whole point of the
// bounded reads in pkg/parser is that a transcript's size is not something
// ccvault gets to assume.
func (db *DB) StoredTranscript(sessionID string) (*StoredTranscript, error) {
	rows, err := db.Query(`
		SELECT raw_json FROM turns
		WHERE session_id = ?
		ORDER BY ordinal`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("read stored transcript for session %s: %w", sessionID, err)
	}
	return &StoredTranscript{sessionID: sessionID, rows: rows}, nil
}

// StoredTranscript is an io.Reader over the lines a session's turns hold. Close
// it when done, as you would the file it stands in for.
//
// Close it *before* writing anything, too. The pool is one connection wide on
// purpose (see SetMaxOpenConns in db.go), so an open cursor holds the only
// handle there is and a write issued while it is open waits for a connection
// that cannot be freed until the write returns.
type StoredTranscript struct {
	sessionID string
	rows      *sql.Rows

	// pending is the unread tail of the line currently being handed out.
	pending []byte
	// done ends the stream: io.EOF once the rows run out, or the first error
	// that stopped it. Sticky, so a caller that keeps reading keeps being told
	// the same thing.
	done error
}

// Read implements io.Reader.
//
// A turn with no stored line ends the stream with ErrNoStoredLine rather than
// being skipped. Skipping it would hand the parser a transcript missing a turn
// and no way to know, which is exactly the shape of silent undercount #155
// already is.
func (t *StoredTranscript) Read(p []byte) (int, error) {
	for len(t.pending) == 0 {
		if t.done != nil {
			return 0, t.done
		}
		if !t.rows.Next() {
			if err := t.rows.Err(); err != nil {
				t.done = fmt.Errorf("read stored transcript for session %s: %w", t.sessionID, err)
			} else {
				t.done = io.EOF
			}
			return 0, t.done
		}
		var raw sql.NullString
		if err := t.rows.Scan(&raw); err != nil {
			t.done = fmt.Errorf("scan stored line of session %s: %w", t.sessionID, err)
			return 0, t.done
		}
		if !raw.Valid {
			t.done = fmt.Errorf("session %s: %w", t.sessionID, ErrNoStoredLine)
			return 0, t.done
		}
		t.pending = append([]byte(raw.String), '\n')
	}

	n := copy(p, t.pending)
	t.pending = t.pending[n:]
	return n, nil
}

// Close releases the cursor.
func (t *StoredTranscript) Close() error {
	return t.rows.Close()
}

// SessionTokenCounters is a repaired set of the four token columns for one
// session.
type SessionTokenCounters struct {
	SessionID  string
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
}

// WriteSessionTokenCounters stores repaired token counters and returns how many
// rows it changed.
//
// The UPDATE repeats the candidate condition — both cache columns still zero —
// so it can only ever apply to a row nobody has repaired in the meantime.
// Without that, two runs over the same archive, or a run racing a sync, could
// each write a total the other had already accounted for.
//
// All four columns, not just the cache pair, because for a source whose cached
// count is a subset of its input count the repair is a split: the cached share
// moves out of input and the session's total is unchanged. Writing only the
// cache pair there would add the cached tokens a second time.
func (db *DB) WriteSessionTokenCounters(ctx context.Context, counters []SessionTokenCounters) (int, error) {
	if len(counters) == 0 {
		return 0, nil
	}

	written := 0
	err := db.WithTxContext(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `
			UPDATE sessions
			SET input_tokens = ?, output_tokens = ?,
			    cache_read_tokens = ?, cache_write_tokens = ?
			WHERE id = ?
			  AND COALESCE(cache_read_tokens, 0) = 0
			  AND COALESCE(cache_write_tokens, 0) = 0`)
		if err != nil {
			return fmt.Errorf("prepare token counter update: %w", err)
		}
		defer func() { _ = stmt.Close() }()

		for _, c := range counters {
			res, err := stmt.ExecContext(ctx, c.Input, c.Output, c.CacheRead, c.CacheWrite, c.SessionID)
			if err != nil {
				return fmt.Errorf("update token counters of session %s: %w", c.SessionID, err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("count the token counter update of session %s: %w", c.SessionID, err)
			}
			written += int(n)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return written, nil
}
