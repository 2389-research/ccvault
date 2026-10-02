// ABOUTME: Tests that Syncer.Run honours context cancellation mid-run.
// ABOUTME: Uses real session files and a real SQLite database in temp dirs.

package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/2389-research/ccvault/internal/config"
)

// writeManyTestSessions creates `count` single-turn Claude Code session files
// so a sync run lasts long enough to be cancelled part-way through.
func writeManyTestSessions(t *testing.T, dir string, count int) {
	t.Helper()
	projDir := filepath.Join(dir, "projects", "-Users-test-cancel")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)

	for i := 0; i < count; i++ {
		// The scanner only accepts UUID-shaped filenames.
		sessionID := fmt.Sprintf("c0ffee00-dead-beef-cafe-%012d", i)
		msg := map[string]any{
			"uuid":      sessionID + "-turn-1",
			"sessionId": sessionID,
			"type":      "human",
			"timestamp": now.Format(time.RFC3339Nano),
			"cwd":       "/Users/test/cancel",
			"message": map[string]any{
				"role":    "user",
				"content": "hello",
			},
		}
		line, err := json.Marshal(msg)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := os.WriteFile(filepath.Join(projDir, sessionID+".jsonl"), append(line, '\n'), 0o600); err != nil {
			t.Fatalf("write session: %v", err)
		}
	}
}

// TestRunStopsOnContextCancel: cancelling mid-run must abandon the remaining
// sessions and report context.Canceled, with the partial stats intact.
func TestRunStopsOnContextCancel(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	claudeHome := t.TempDir()
	const total = 300
	writeManyTestSessions(t, claudeHome, total)

	sources := []config.SourceConfig{
		{Name: "claude-code", Type: "claude-code", Path: claudeHome},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	syncer := New(database, sources,
		WithCountProgressCallback(func(current, total int) {
			if current == 5 {
				cancel()
			}
		}),
	)

	stats, err := syncer.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if stats == nil {
		t.Fatal("expected partial stats on cancellation, got nil")
	}
	if stats.SessionsIndexed == 0 {
		t.Error("expected some sessions indexed before cancellation")
	}
	if stats.SessionsIndexed >= total {
		t.Errorf("sync kept going after cancellation: %d of %d sessions indexed", stats.SessionsIndexed, total)
	}
	if stats.Duration == 0 {
		t.Error("expected Duration to be recorded on the partial stats")
	}
}

// TestCancelledRunLeavesProjectCountersConsistent: UpsertProject accumulates
// session_count / total_tokens, so an abandoned run has to reconcile them
// against the rows that exist. Otherwise every cancelled sync inflates the
// numbers the dashboard, projects view, and MCP all read.
func TestCancelledRunLeavesProjectCountersConsistent(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	claudeHome := t.TempDir()
	writeManyTestSessions(t, claudeHome, 300)

	sources := []config.SourceConfig{
		{Name: "claude-code", Type: "claude-code", Path: claudeHome},
	}

	// A completed sync first, so the counters start out correct and a
	// re-parse has something to double up on.
	if _, err := New(database, sources).Run(context.Background()); err != nil {
		t.Fatalf("initial sync: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// --full forces every session to be re-parsed and re-upserted.
	syncer := New(database, sources,
		WithFullSync(true),
		WithCountProgressCallback(func(current, total int) {
			if current == 50 {
				cancel()
			}
		}),
	)
	if _, err := syncer.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}

	var storedCount int
	if err := database.QueryRow("SELECT session_count FROM projects WHERE path = ?", "/Users/test/cancel").Scan(&storedCount); err != nil {
		t.Fatalf("read project counter: %v", err)
	}
	var actual int
	if err := database.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&actual); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if storedCount != actual {
		t.Errorf("project session_count = %d but %d sessions exist; cancelled sync left the counters inflated",
			storedCount, actual)
	}
}

// TestRunWithCancelledContextIndexesNothing: a context cancelled before Run
// starts must stop the sync before it writes anything.
func TestRunWithCancelledContextIndexesNothing(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	claudeHome := t.TempDir()
	writeManyTestSessions(t, claudeHome, 10)

	sources := []config.SourceConfig{
		{Name: "claude-code", Type: "claude-code", Path: claudeHome},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	stats, err := New(database, sources).Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if stats != nil && stats.SessionsIndexed != 0 {
		t.Errorf("expected no sessions indexed, got %d", stats.SessionsIndexed)
	}

	var count int
	if err := database.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&count); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 rows in sessions, got %d", count)
	}
}
