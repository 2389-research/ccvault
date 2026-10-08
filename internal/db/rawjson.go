// ABOUTME: Integrity reporting for turns.raw_json, the stored copy of each JSONL line
// ABOUTME: Counts rows whose stored bytes are not valid JSON, so the condition isn't silent

package db

import (
	"context"
	"fmt"
	"strings"
)

// DefaultRawJSONWindow is how many of the newest turns `ccvault stats` checks
// when it is not asked for a whole-archive scan. See CheckRawJSONIntegrity for
// why the default is bounded at all.
const DefaultRawJSONWindow = 100000

// RawJSONIntegrity reports how many turns hold a raw_json that is not valid
// JSON.
//
// turns.raw_json is the stored copy of the source JSONL line, and it is the
// only input to anything re-derived from a turn after the fact — tool payloads,
// thinking blocks, the MCP server's tools list. GetTurns drops a raw_json that
// does not parse rather than handing downstream json.Marshal something that
// crashes it, so an affected turn arrives at its consumer with no payload at
// all and whatever was derived from it is simply absent. turns.content is
// extracted at parse time and unaffected, so search still reaches these turns;
// it is the structured re-derivation that is lost.
type RawJSONIntegrity struct {
	// Turns is the number of rows in the turns table, whether scanned or not.
	Turns int64
	// Scanned is how many of them this report actually examined — equal to
	// Turns for a whole-archive scan, and the window size otherwise.
	Scanned int64
	// Invalid is how many of the scanned rows hold a non-NULL raw_json that
	// is not valid JSON.
	Invalid int64
	// Sessions is how many distinct sessions those rows belong to.
	Sessions int64
	// SourceFiles are the distinct, non-empty source_file paths of those
	// sessions. A re-parse is the only repair, so whether these files still
	// exist is what decides whether the damage is repairable at all; the
	// caller stats them.
	SourceFiles []string
	// Complete reports whether the scan covered every turn. A windowed report
	// is a lower bound and nothing more.
	Complete bool
}

// Consistent reports whether every scanned row holds valid JSON.
func (r RawJSONIntegrity) Consistent() bool {
	return r.Invalid == 0
}

// CheckRawJSONIntegrity counts turns whose raw_json is not valid JSON, over the
// newest `window` turns by rowid. Pass 0 (or a window at or above the turn
// count) to scan the whole archive, which sets Complete.
//
// The window exists because this check cannot be made cheap. json_valid has to
// read every byte of the value, and raw_json is almost the entire size of the
// archive: measured on the author's 6 GB, 996,680-turn archive, a whole-archive
// scan takes ~35 seconds, against ~55 for everything else `ccvault stats` does
// put together. Two cheaper proxies were measured and rejected — a first-byte
// test is wrong (raw_json is stored as a BLOB, so substr returns a blob that
// never compares equal to '{'), and a first-and-last-byte test found 216,976 of
// the 216,978 but took just as long, because the cost is reading the column's
// overflow pages rather than parsing what they hold.
//
// Bounding it by rowid is what makes the default report worth its cost: rowids
// are handed out in insertion order, so the newest turns are the ones most
// recently written, and new damage is the only kind a report can still act on.
// The known backlog (issue #101: 216,978 turns, all written before 2026-08 by a
// parser that handed SQLite a slice of bufio.Scanner's reused buffer) is
// historical and, on the author's archive, unrepairable — every one of the 42
// affected sessions has had its source transcript pruned by Claude Code's own
// 30-day cleanup. A full scan reports that backlog; the windowed one answers
// the question that still has an answer.
func (db *DB) CheckRawJSONIntegrity(window int64) (RawJSONIntegrity, error) {
	return db.CheckRawJSONIntegrityContext(context.Background(), window)
}

// CheckRawJSONIntegrityContext is CheckRawJSONIntegrity bound to ctx. A
// whole-archive scan reads the largest column in the database, so a caller that
// can be cancelled needs to be able to interrupt it.
func (db *DB) CheckRawJSONIntegrityContext(ctx context.Context, window int64) (RawJSONIntegrity, error) {
	var r RawJSONIntegrity

	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM turns").Scan(&r.Turns); err != nil {
		return RawJSONIntegrity{}, fmt.Errorf("count turns: %w", err)
	}

	// A window of 0, a negative one, or one that reaches past the end of the
	// table all mean the same thing: scan everything, and say so.
	limit := window
	if limit <= 0 || limit >= r.Turns {
		limit = r.Turns
		r.Complete = true
	}
	r.Scanned = limit
	if limit == 0 {
		return r, nil
	}

	// One pass over the column, grouped, so the per-session counts and the
	// totals come out of the same read: at ~35 seconds per 6 GB this is the
	// one query worth not running twice. The subquery is ordered by rowid DESC
	// and limited, so the scan stops at the window rather than reading the
	// column for every row and filtering afterwards.
	const perSession = `
		SELECT session_id, COUNT(*)
		FROM (
			SELECT session_id, raw_json FROM turns ORDER BY rowid DESC LIMIT ?
		)
		WHERE raw_json IS NOT NULL AND json_valid(raw_json) = 0
		GROUP BY session_id`

	rows, err := db.QueryContext(ctx, perSession, limit)
	if err != nil {
		return RawJSONIntegrity{}, fmt.Errorf("count invalid raw_json: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var sessionIDs []string
	for rows.Next() {
		var sessionID string
		var invalid int64
		if err := rows.Scan(&sessionID, &invalid); err != nil {
			return RawJSONIntegrity{}, fmt.Errorf("scan invalid raw_json count: %w", err)
		}
		r.Invalid += invalid
		r.Sessions++
		sessionIDs = append(sessionIDs, sessionID)
	}
	if err := rows.Err(); err != nil {
		return RawJSONIntegrity{}, fmt.Errorf("count invalid raw_json: %w", err)
	}
	if err := rows.Close(); err != nil {
		return RawJSONIntegrity{}, fmt.Errorf("count invalid raw_json: %w", err)
	}

	files, err := db.sessionSourceFiles(ctx, sessionIDs)
	if err != nil {
		return RawJSONIntegrity{}, err
	}
	r.SourceFiles = files

	return r, nil
}

// sessionSourceFiles returns the distinct, non-empty source_file paths of the
// given sessions. Lookups are by primary key, so this costs nothing next to the
// scan that produced the ids — which is the point of passing them in rather
// than letting the source_file query repeat the scan as a subquery.
func (db *DB) sessionSourceFiles(ctx context.Context, sessionIDs []string) ([]string, error) {
	// Chunked so a pathological archive — every session affected — can't
	// exceed SQLite's bound-parameter ceiling, the same way
	// ReconcileProjectAggregates chunks its paths.
	const chunk = 500

	seen := make(map[string]bool, len(sessionIDs))
	var paths []string

	for start := 0; start < len(sessionIDs); start += chunk {
		end := min(start+chunk, len(sessionIDs))
		batch, err := db.sessionSourceFileChunk(ctx, sessionIDs[start:end])
		if err != nil {
			return nil, err
		}
		for _, path := range batch {
			if !seen[path] {
				seen[path] = true
				paths = append(paths, path)
			}
		}
	}

	return paths, nil
}

// sessionSourceFileChunk reads one bound-parameter-sized batch of source files.
// Its own function so the rows handle is closed by defer, on every path.
func (db *DB) sessionSourceFileChunk(ctx context.Context, sessionIDs []string) ([]string, error) {
	placeholders := make([]string, len(sessionIDs))
	args := make([]interface{}, len(sessionIDs))
	for i, id := range sessionIDs {
		placeholders[i] = "?"
		args[i] = id
	}

	query := `SELECT source_file FROM sessions
		WHERE source_file IS NOT NULL AND source_file != ''
		  AND id IN (` + strings.Join(placeholders, ", ") + `)
		ORDER BY source_file`

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list source files of damaged sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var paths []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, fmt.Errorf("scan session source file: %w", err)
		}
		paths = append(paths, path)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list source files of damaged sessions: %w", err)
	}
	return paths, nil
}
