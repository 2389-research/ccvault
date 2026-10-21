// ABOUTME: Tests the archive's side of the cache-token backfill — coverage counting, candidate selection, guarded writes.
// ABOUTME: The guard on the write is what makes two runs over one archive safe, so it is tested directly.

package db

import (
	"context"
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

// TestWriteSessionTokenCounters_RefusesARowThatIsNoLongerZero is what makes two
// runs over one archive safe.
//
// The candidate list is read before the writes begin, so a row can be repaired
// by another run — or by a sync that re-read its transcript — in between. The
// UPDATE repeats the candidate condition, so the second writer changes nothing
// rather than adding a total the first already accounted for.
func TestWriteSessionTokenCounters_RefusesARowThatIsNoLongerZero(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	seedTokenSession(t, database, "zero-now", 100, 20, 0, 0)
	seedTokenSession(t, database, "already-repaired", 100, 20, 4096, 512)

	written, err := database.WriteSessionTokenCounters(context.Background(), []SessionTokenCounters{
		{SessionID: "zero-now", Input: 100, Output: 20, CacheRead: 777, CacheWrite: 11},
		{SessionID: "already-repaired", Input: 100, Output: 20, CacheRead: 999, CacheWrite: 99},
	})
	if err != nil {
		t.Fatalf("WriteSessionTokenCounters: %v", err)
	}
	if written != 1 {
		t.Errorf("written = %d, want 1; only the row still reading zero may be written", written)
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
