// ABOUTME: End-to-end guard for #146 — sync counts and reports turns whose transcript timestamp could not be read.
// ABOUTME: Mirrors the skipped_lines diagnostic, so an unreadable clock becomes a number instead of a plausible-looking row.

package sync

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/2389-research/ccvault/internal/config"
)

// TestSync_ReportsUnparsableTimestamps is #146 at the layer a person reads.
//
// The three layout lists in pkg/parser and pkg/adapter/{codex,jeff} stay
// separate on purpose — each parses a different tool's transcript format,
// which we do not control. What was missing is any sign that one of them
// failed: the turn is kept with a fallback timestamp, which sorts somewhere
// arbitrary and escapes every date filter while looking like a real value.
//
// So sync counts them and names the first unreadable string, the way #144
// made non-canonical stored values visible rather than refusing them.
func TestSync_ReportsUnparsableTimestamps(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	claudeHome, err := os.MkdirTemp("", "claude-home-ts-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(claudeHome) }()

	const (
		sessionID = "7ba1e000-1111-2222-3333-444455556666"
		bad       = "2026/05/26 14:30:00"
	)
	projDir := filepath.Join(claudeHome, "projects", "-Users-test-badclock")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	lines := strings.Join([]string{
		`{"uuid":"ts-1","sessionId":"` + sessionID + `","type":"user","timestamp":"2026-05-26T14:29:00Z","cwd":"/Users/test/badclock","message":{"role":"user","content":"readable"}}`,
		`{"uuid":"ts-2","sessionId":"` + sessionID + `","type":"user","timestamp":"` + bad + `","cwd":"/Users/test/badclock","message":{"role":"user","content":"unreadable"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(projDir, sessionID+".jsonl"), []byte(lines), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	var progress []string
	syncer := New(database, []config.SourceConfig{
		{Name: "claude-code", Type: "claude-code", Path: claudeHome},
	}, WithFullSync(true), WithProgressCallback(func(msg string) {
		progress = append(progress, msg)
	}))

	stats, err := syncer.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	if stats.TurnsWithUnparsableTimestamp != 1 {
		t.Errorf("TurnsWithUnparsableTimestamp = %d, want 1", stats.TurnsWithUnparsableTimestamp)
	}
	if stats.SessionsWithUnparsableTimestamps != 1 {
		t.Errorf("SessionsWithUnparsableTimestamps = %d, want 1", stats.SessionsWithUnparsableTimestamps)
	}
	if stats.UnparsableTimestampSample != bad {
		t.Errorf("UnparsableTimestampSample = %q, want %q", stats.UnparsableTimestampSample, bad)
	}

	// Counting it without saying so would leave it just as invisible.
	joined := strings.Join(progress, "\n")
	if !strings.Contains(joined, bad) {
		t.Errorf("sync progress never names the unreadable value %q:\n%s", bad, joined)
	}

	// The turn is still indexed. Reporting the problem must not also lose the
	// conversation — the archive has repeatedly been better served by the user
	// seeing a problem than by a guard that drops data.
	if stats.TurnsIndexed != 2 {
		t.Errorf("TurnsIndexed = %d, want 2", stats.TurnsIndexed)
	}
}
