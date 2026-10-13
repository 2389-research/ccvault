// ABOUTME: End-to-end guard for #155 — a synced session row must carry the cache counters its transcript recorded.
// ABOUTME: The archive's cache_read/cache_write columns were 0 for every session because nothing assigned the parsed values.

package sync

import (
	"context"
	"os"
	"testing"

	"github.com/2389-research/ccvault/internal/config"
)

// TestSync_SessionCarriesCacheTokens is #155 at the layer that matters.
//
// Every layer was correct except one assignment: models.RawUsage parsed
// cache_read_input_tokens and cache_creation_input_tokens, the schema held the
// columns, internal/db wrote whatever it was handed and internal/analytics
// summed them — but nothing ever put the parsed numbers into the model, so
// zero was all that reached storage for all 47,409 sessions in the author's
// archive.
//
// Zero is a plausible value for a cache counter, which is why no output ever
// looked absurd. The assertion has to be on non-zero *and* on the exact
// transcript numbers, or it passes against the bug again.
func TestSync_SessionCarriesCacheTokens(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	claudeHome, err := os.MkdirTemp("", "claude-home-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(claudeHome) }()

	const sessionID = "cace0000-1111-2222-3333-444455556666"
	writeTestSession(t, claudeHome, sessionID, "-Users-test-cacheproject")

	syncer := New(database, []config.SourceConfig{
		{Name: "claude-code", Type: "claude-code", Path: claudeHome},
	}, WithFullSync(true))

	if _, err := syncer.Run(context.Background()); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	session, err := database.GetSession(sessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}

	// writeTestSession's assistant usage object. Kept as literals rather than
	// read back from the fixture so a fixture that stops carrying cache
	// counters fails here instead of silently agreeing with zero.
	const (
		wantInput      = int64(10)
		wantOutput     = int64(5)
		wantCacheRead  = int64(100)
		wantCacheWrite = int64(20)
	)

	if session.CacheReadTokens != wantCacheRead {
		t.Errorf("CacheReadTokens = %d, want %d", session.CacheReadTokens, wantCacheRead)
	}
	if session.CacheWriteTokens != wantCacheWrite {
		t.Errorf("CacheWriteTokens = %d, want %d", session.CacheWriteTokens, wantCacheWrite)
	}

	// The counted fields must not move: this is a fix for what was dropped,
	// not a re-interpretation of what was already stored.
	if session.InputTokens != wantInput {
		t.Errorf("InputTokens = %d, want %d", session.InputTokens, wantInput)
	}
	if session.OutputTokens != wantOutput {
		t.Errorf("OutputTokens = %d, want %d", session.OutputTokens, wantOutput)
	}

	if got, want := session.TotalTokens(), wantInput+wantOutput+wantCacheRead+wantCacheWrite; got != want {
		t.Errorf("TotalTokens() = %d, want %d", got, want)
	}
}
