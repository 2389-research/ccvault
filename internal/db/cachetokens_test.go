// ABOUTME: Tests the archive's side of the cache-token backfill — coverage counting, candidate selection, guarded writes.
// ABOUTME: The guard on the write is what makes two runs over one archive safe, so it is tested directly.

package db

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/2389-research/ccvault/pkg/models"
)

// seedTokenSession inserts one session with the given counters.
func seedTokenSession(t *testing.T, database *DB, id string, input, output, cacheRead, cacheWrite int64) {
	t.Helper()

	project := &models.Project{
		Path:           "/Users/test/cachetokens",
		DisplayName:    "cachetokens",
		FirstSeenAt:    time.Now(),
		LastActivityAt: time.Now(),
	}
	if err := database.UpsertProject(project); err != nil {
		t.Fatalf("upsert project: %v", err)
	}
	session := &models.Session{
		ID:               id,
		ProjectID:        project.ID,
		StartedAt:        time.Now(),
		EndedAt:          time.Now(),
		InputTokens:      input,
		OutputTokens:     output,
		CacheReadTokens:  cacheRead,
		CacheWriteTokens: cacheWrite,
		SourceFile:       "/gone/" + id + ".jsonl",
	}
	if err := database.UpsertSession(session); err != nil {
		t.Fatalf("upsert session %s: %v", id, err)
	}
}

// TestCacheTokenCoverage separates the two readings of a zero cache counter.
//
// A session that cost input or output tokens and reports no cache tokens is
// the understatement #155 is about. A session with no tokens of any kind is
// either empty or from a source that records none — counting those would turn
// the figure `stats` reports into noise nobody acts on.
func TestCacheTokenCoverage(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	seedTokenSession(t, database, "has-cache", 100, 20, 4096, 512)
	seedTokenSession(t, database, "missing-cache", 100, 20, 0, 0)
	seedTokenSession(t, database, "no-tokens-at-all", 0, 0, 0, 0)

	coverage, err := database.CacheTokenCoverage()
	if err != nil {
		t.Fatalf("CacheTokenCoverage: %v", err)
	}

	if coverage.Sessions != 3 {
		t.Errorf("Sessions = %d, want 3", coverage.Sessions)
	}
	if coverage.MissingCache != 2 {
		t.Errorf("MissingCache = %d, want 2", coverage.MissingCache)
	}
	if coverage.MissingWithCost != 1 {
		t.Errorf("MissingWithCost = %d, want 1; the tokenless session is not an understatement",
			coverage.MissingWithCost)
	}
	if coverage.CacheTokens != 4096+512 {
		t.Errorf("CacheTokens = %d, want %d", coverage.CacheTokens, 4096+512)
	}
	if coverage.Complete() {
		t.Error("Complete() is true while a session that cost tokens reports no cache tokens")
	}
}

// TestCacheBackfillCandidates pins the idempotence key.
//
// Both cache columns being zero is the whole condition: it needs no schema and
// no bookkeeping, so a session repaired by an interrupted run is simply not a
// candidate for the next one.
func TestCacheBackfillCandidates(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	seedTokenSession(t, database, "repaired-already", 100, 20, 4096, 512)
	seedTokenSession(t, database, "read-only-set", 100, 20, 4096, 0)
	seedTokenSession(t, database, "still-zero", 100, 20, 0, 0)
	seedTokenSession(t, database, "zero-everything", 0, 0, 0, 0)

	candidates, err := database.CacheBackfillCandidates()
	if err != nil {
		t.Fatalf("CacheBackfillCandidates: %v", err)
	}

	got := make(map[string]CacheBackfillCandidate, len(candidates))
	for _, c := range candidates {
		got[c.ID] = c
	}
	if len(got) != 2 {
		t.Fatalf("got %d candidates (%v), want 2", len(got), got)
	}
	// A session with no tokens at all is still a candidate: its zero might be
	// a transcript that records nothing, and it might be one whose counters
	// were never read, and only the stored lines can say which.
	if _, ok := got["zero-everything"]; !ok {
		t.Error("a session with no tokens at all was not offered as a candidate")
	}
	candidate, ok := got["still-zero"]
	if !ok {
		t.Fatal("the understated session was not offered as a candidate")
	}
	if candidate.StoredInput != 100 || candidate.StoredOutput != 20 {
		t.Errorf("stored counters = %d/%d, want 100/20",
			candidate.StoredInput, candidate.StoredOutput)
	}
	if candidate.SourceFile != "/gone/still-zero.jsonl" {
		t.Errorf("SourceFile = %q; the adapter needs the original path", candidate.SourceFile)
	}
	// A session with one of the two columns already set has been through a
	// sync that assigned them, so it is not understated.
	if _, ok := got["read-only-set"]; ok {
		t.Error("a session carrying a cache read count was offered as a candidate")
	}
}

// seedTurn inserts one turn of a session with the given stored line.
func seedTurn(t *testing.T, database *DB, sessionID string, ordinal int, rawJSON any) {
	t.Helper()

	_, err := database.Exec(`
		INSERT INTO turns (id, session_id, type, timestamp, content, raw_json, ordinal)
		VALUES (?, ?, 'assistant', '2026-01-01T00:00:00.000000000Z', '', ?, ?)`,
		fmt.Sprintf("%s-t%d", sessionID, ordinal), sessionID, rawJSON, ordinal)
	if err != nil {
		t.Fatalf("insert turn %d of %s: %v", ordinal, sessionID, err)
	}
}

// TestStoredTranscript_ReassemblesTheLines is the premise the whole backfill
// rests on: turns.raw_json holds the complete JSONL line, so the lines put
// back in ordinal order are the transcript the source wrote.
//
// The lines here are long enough to span bufio's 4096-byte buffer several
// times over, because that is the shape real transcripts have and it is the
// only way the partial-copy path in Read runs at all.
func TestStoredTranscript_ReassemblesTheLines(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	const sessionID = "transcript-session"
	seedTokenSession(t, database, sessionID, 0, 0, 0, 0)

	want := make([]string, 3)
	for i := range want {
		// ~6 KB per line, so no line fits in one Read and none aligns with the
		// buffer boundary.
		want[i] = fmt.Sprintf(`{"uuid":"t%d","text":%q}`, i, strings.Repeat("x", 6000+i))
		seedTurn(t, database, sessionID, i, want[i])
	}

	transcript, err := database.StoredTranscript(sessionID)
	if err != nil {
		t.Fatalf("StoredTranscript: %v", err)
	}
	defer func() { _ = transcript.Close() }()

	read, err := io.ReadAll(transcript)
	if err != nil {
		t.Fatalf("read the transcript: %v", err)
	}
	if got := string(read); got != strings.Join(want, "\n")+"\n" {
		t.Errorf("reassembled transcript is not the stored lines in order (%d bytes, want %d)",
			len(got), len(strings.Join(want, "\n"))+1)
	}
	if n := transcript.StrippedLines(); n != 0 {
		t.Errorf("StrippedLines = %d, want 0; none of these is a placeholder", n)
	}
	// Closing twice is what a defer plus an explicit close does, and it must
	// not be an error.
	if err := transcript.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// TestStoredTranscript_StopsAtALineItCannotRead pins both unreadable shapes.
//
// A NULL is the state 216,978 turns across 42 sessions were deliberately left
// in (#101). An empty string is the one that would otherwise pass: it becomes
// a bare newline, which pkg/parser drops without even counting it as a skipped
// line, so the turn would leave the transcript with nothing to say so.
func TestStoredTranscript_StopsAtALineItCannotRead(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
	}{
		{name: "null", value: nil},
		{name: "empty string", value: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, cleanup := setupTestDB(t)
			defer cleanup()

			const sessionID = "unreadable-session"
			seedTokenSession(t, database, sessionID, 0, 0, 0, 0)
			seedTurn(t, database, sessionID, 0, `{"uuid":"t0"}`)
			seedTurn(t, database, sessionID, 1, tc.value)

			transcript, err := database.StoredTranscript(sessionID)
			if err != nil {
				t.Fatalf("StoredTranscript: %v", err)
			}
			defer func() { _ = transcript.Close() }()

			_, err = io.ReadAll(transcript)
			if !errors.Is(err, ErrNoStoredLine) {
				t.Fatalf("read error = %v, want ErrNoStoredLine", err)
			}
			if !strings.Contains(err.Error(), sessionID) {
				t.Errorf("error %q does not name the session", err)
			}
			// Sticky: a caller that keeps reading keeps being told the same
			// thing rather than being handed an EOF that looks like a clean end.
			if _, again := transcript.Read(make([]byte, 16)); !errors.Is(again, ErrNoStoredLine) {
				t.Errorf("second read error = %v, want ErrNoStoredLine", again)
			}
		})
	}
}

// TestStoredTranscript_CountsThePlaceholderLines covers the signal the backfill
// refuses on.
//
// A placeholder parses cleanly and carries no usage, so nothing downstream can
// tell that the turn's cost was dropped — recognising the marker is the only
// way to know. The size bound is deliberate: a real line mentioning the marker
// inside a megabyte of content is not a placeholder, and scanning every line in
// full would put a 4 GB substring search in front of every repair.
func TestStoredTranscript_CountsThePlaceholderLines(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	const sessionID = "stripped-session"
	seedTokenSession(t, database, sessionID, 0, 0, 0, 0)
	seedTurn(t, database, sessionID, 0, `{"uuid":"t0","message":{"usage":{"input_tokens":1}}}`)
	seedTurn(t, database, sessionID, 1,
		`{"_ccvault_stripped":true,"_ccvault_original_size":9000000,"uuid":"t1","sessionId":"s","type":"assistant","timestamp":"2026-01-01T00:00:00Z"}`)
	// A real line that merely quotes the marker, past the size bound. Counting
	// this would refuse a session that is perfectly repairable.
	seedTurn(t, database, sessionID, 2,
		fmt.Sprintf(`{"uuid":"t2","text":"_ccvault_stripped %s"}`, strings.Repeat("y", maxStrippedLineBytes)))

	transcript, err := database.StoredTranscript(sessionID)
	if err != nil {
		t.Fatalf("StoredTranscript: %v", err)
	}
	defer func() { _ = transcript.Close() }()

	if _, err := io.ReadAll(transcript); err != nil {
		t.Fatalf("read the transcript: %v", err)
	}
	if n := transcript.StrippedLines(); n != 1 {
		t.Errorf("StrippedLines = %d, want 1", n)
	}
}

// TestWriteSessionTokenCounters_RefusesARowThatIsNoLongerZero is what makes two
// runs over one archive safe.
//
// The candidate list is read before the writes begin, so a row can be repaired
// by another run — or by a sync that re-read its transcript — in between. The
// UPDATE repeats everything the caller read the row for, so the second writer
// changes nothing rather than adding a total the first already accounted for,
// and it names the rows it declined so the caller can correct its own counts.
//
// Both halves of the condition are covered: a row whose cache pair has been
// filled in, and a row whose input and output have moved since the repair was
// derived — which is the one a cache-only guard cannot see, and the one that
// would overwrite a sync's newer figures with this run's stale ones.
func TestWriteSessionTokenCounters_RefusesARowThatIsNoLongerZero(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	seedTokenSession(t, database, "zero-now", 100, 20, 0, 0)
	seedTokenSession(t, database, "already-repaired", 100, 20, 4096, 512)
	seedTokenSession(t, database, "counted-moved", 5000, 900, 0, 0)

	written, refused, err := database.WriteSessionTokenCounters(context.Background(), []SessionTokenCounters{
		{SessionID: "zero-now", Input: 100, Output: 20, CacheRead: 777, CacheWrite: 11},
		{SessionID: "already-repaired", Input: 100, Output: 20, CacheRead: 999, CacheWrite: 99},
		// Derived when the row read 100/20; a sync has since re-parsed it.
		{SessionID: "counted-moved", Input: 100, Output: 20, CacheRead: 555, CacheWrite: 55},
	})
	if err != nil {
		t.Fatalf("WriteSessionTokenCounters: %v", err)
	}
	if written != 1 {
		t.Errorf("written = %d, want 1; only the row the repair was derived from may be written", written)
	}
	if len(refused) != 2 {
		t.Errorf("refused = %v, want the two rows that had moved", refused)
	}

	moved, err := database.GetSession("counted-moved")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if moved.InputTokens != 5000 || moved.OutputTokens != 900 {
		t.Errorf("input/output = %d/%d, want 5000/900 — a stale repair must not overwrite them",
			moved.InputTokens, moved.OutputTokens)
	}

	repaired, err := database.GetSession("zero-now")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if repaired.CacheReadTokens != 777 || repaired.CacheWriteTokens != 11 {
		t.Errorf("cache pair = %d/%d, want 777/11",
			repaired.CacheReadTokens, repaired.CacheWriteTokens)
	}

	untouched, err := database.GetSession("already-repaired")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if untouched.CacheReadTokens != 4096 || untouched.CacheWriteTokens != 512 {
		t.Errorf("cache pair = %d/%d, want 4096/512 left alone",
			untouched.CacheReadTokens, untouched.CacheWriteTokens)
	}
}
