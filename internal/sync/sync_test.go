// ABOUTME: Tests for the sync package's adapter-based session syncing
// ABOUTME: Validates multi-source discovery, parsing, incremental sync, and error handling

package sync

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/2389-research/ccvault/internal/config"
	"github.com/2389-research/ccvault/internal/db"
	"github.com/2389-research/ccvault/pkg/adapter"

	// Register the claude-code adapter
	_ "github.com/2389-research/ccvault/pkg/adapter/claudecode"
)

// setupTestDB creates a temporary database for testing
func setupTestDB(t *testing.T) (*db.DB, func()) {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "sync-test-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	database, err := db.Open(tmpDir)
	if err != nil {
		_ = os.RemoveAll(tmpDir)
		t.Fatalf("open db: %v", err)
	}
	return database, func() {
		_ = database.Close()
		_ = os.RemoveAll(tmpDir)
	}
}

// writeTestSession writes a minimal Claude Code JSONL session file
func writeTestSession(t *testing.T, dir, sessionID, projectDir string) string {
	t.Helper()
	// Create projects/<encoded-path>/ directory
	projDir := filepath.Join(dir, "projects", projectDir)
	if err := os.MkdirAll(projDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(projDir, sessionID+".jsonl")

	now := time.Now().UTC().Truncate(time.Millisecond)

	// Write a minimal JSONL file with a user message and an assistant response
	userMsg := map[string]any{
		"uuid":      "turn-1",
		"sessionId": sessionID,
		"type":      "human",
		"timestamp": now.Format(time.RFC3339Nano),
		"message": map[string]any{
			"role":    "user",
			"content": "Hello world",
		},
	}
	assistantMsg := map[string]any{
		"uuid":       "turn-2",
		"parentUuid": "turn-1",
		"sessionId":  sessionID,
		"type":       "assistant",
		"timestamp":  now.Add(time.Second).Format(time.RFC3339Nano),
		"message": map[string]any{
			"id":    "msg-1",
			"model": "claude-sonnet-4-20250514",
			"role":  "assistant",
			"content": []map[string]any{
				{"type": "text", "text": "Hi there!"},
			},
			"usage": map[string]any{
				"input_tokens":  10,
				"output_tokens": 5,
			},
		},
	}

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create file: %v", err)
	}
	defer func() { _ = f.Close() }()

	enc := json.NewEncoder(f)
	if err := enc.Encode(userMsg); err != nil {
		t.Fatalf("encode user: %v", err)
	}
	if err := enc.Encode(assistantMsg); err != nil {
		t.Fatalf("encode assistant: %v", err)
	}

	return path
}

func TestNewSyncerAcceptsSources(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	sources := []config.SourceConfig{
		{Name: "test-source", Type: "claude-code", Path: "/tmp/fake"},
	}
	syncer := New(database, sources)
	if syncer == nil {
		t.Fatal("expected non-nil syncer")
	}
	if len(syncer.sources) != 1 {
		t.Fatalf("expected 1 source, got %d", len(syncer.sources))
	}
	if syncer.sources[0].Name != "test-source" {
		t.Errorf("expected source name 'test-source', got %q", syncer.sources[0].Name)
	}
}

func TestRunWithClaudeCodeAdapter(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	// Create a fake Claude home with a session
	claudeHome, err := os.MkdirTemp("", "claude-home-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(claudeHome) }()

	writeTestSession(t, claudeHome, "a1b2c3d4-e5f6-7890-abcd-ef1234567890", "-Users-test-myproject")

	sources := []config.SourceConfig{
		{Name: "claude-code", Type: "claude-code", Path: claudeHome},
	}

	var progressMsgs []string
	syncer := New(database, sources,
		WithFullSync(true),
		WithProgressCallback(func(msg string) {
			progressMsgs = append(progressMsgs, msg)
		}),
	)

	stats, err := syncer.Run()
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	if stats.SessionsScanned != 1 {
		t.Errorf("expected 1 session scanned, got %d", stats.SessionsScanned)
	}
	if stats.SessionsIndexed != 1 {
		t.Errorf("expected 1 session indexed, got %d", stats.SessionsIndexed)
	}
	if stats.TurnsIndexed < 1 {
		t.Errorf("expected at least 1 turn indexed, got %d", stats.TurnsIndexed)
	}
	if stats.ProjectsFound < 1 {
		t.Errorf("expected at least 1 project, got %d", stats.ProjectsFound)
	}
	if len(progressMsgs) == 0 {
		t.Error("expected progress messages, got none")
	}
}

func TestIncrementalSyncSkipsUnchanged(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	claudeHome, err := os.MkdirTemp("", "claude-home-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(claudeHome) }()

	writeTestSession(t, claudeHome, "b2c3d4e5-f6a7-8901-bcde-f12345678901", "-Users-test-project2")

	sources := []config.SourceConfig{
		{Name: "claude-code", Type: "claude-code", Path: claudeHome},
	}

	// First sync: full
	syncer := New(database, sources, WithFullSync(true))
	stats1, err := syncer.Run()
	if err != nil {
		t.Fatalf("first Run() error: %v", err)
	}
	if stats1.SessionsIndexed != 1 {
		t.Fatalf("expected 1 indexed on first sync, got %d", stats1.SessionsIndexed)
	}

	// Second sync: incremental (should skip the file since mtime hasn't changed)
	syncer2 := New(database, sources)
	stats2, err := syncer2.Run()
	if err != nil {
		t.Fatalf("second Run() error: %v", err)
	}
	if stats2.SessionsSkipped != 1 {
		t.Errorf("expected 1 skipped on incremental sync, got %d", stats2.SessionsSkipped)
	}
	if stats2.SessionsIndexed != 0 {
		t.Errorf("expected 0 indexed on incremental sync, got %d", stats2.SessionsIndexed)
	}
}

func TestRunWithUnknownAdapterType(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	sources := []config.SourceConfig{
		{Name: "unknown", Type: "nonexistent-adapter", Path: "/tmp/fake"},
	}

	syncer := New(database, sources)
	stats, err := syncer.Run()
	if err != nil {
		t.Fatalf("Run() should not return top-level error for adapter failures, got: %v", err)
	}
	if len(stats.Errors) == 0 {
		t.Error("expected errors for unknown adapter type, got none")
	}
}

func TestNeedsSyncWithMtimeMap(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	syncer := New(database, nil)
	now := time.Now()

	tests := []struct {
		name     string
		sf       adapter.SessionFile
		mtimes   map[string]time.Time
		expected bool
	}{
		{
			name:     "no stored mtime",
			sf:       adapter.SessionFile{Path: "/a.jsonl", ModTime: now},
			mtimes:   map[string]time.Time{},
			expected: true,
		},
		{
			name:     "file newer than stored",
			sf:       adapter.SessionFile{Path: "/b.jsonl", ModTime: now},
			mtimes:   map[string]time.Time{"/b.jsonl": now.Add(-time.Hour)},
			expected: true,
		},
		{
			name:     "file same as stored",
			sf:       adapter.SessionFile{Path: "/c.jsonl", ModTime: now},
			mtimes:   map[string]time.Time{"/c.jsonl": now},
			expected: false,
		},
		{
			name:     "file older than stored",
			sf:       adapter.SessionFile{Path: "/d.jsonl", ModTime: now.Add(-time.Hour)},
			mtimes:   map[string]time.Time{"/d.jsonl": now},
			expected: false,
		},
		{
			name:     "zero modtime always syncs",
			sf:       adapter.SessionFile{Path: "/e.jsonl", ModTime: time.Time{}},
			mtimes:   map[string]time.Time{"/e.jsonl": now},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := syncer.needsSync(tt.sf, tt.mtimes)
			if result != tt.expected {
				t.Errorf("needsSync() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestMultipleSourcesSync(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	// Create two separate claude homes
	home1, err := os.MkdirTemp("", "home1-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(home1) }()

	home2, err := os.MkdirTemp("", "home2-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(home2) }()

	writeTestSession(t, home1, "c3d4e5f6-a7b8-9012-cdef-123456789012", "-Users-test-proj1")
	writeTestSession(t, home2, "d4e5f6a7-b8c9-0123-defa-234567890123", "-Users-test-proj2")

	sources := []config.SourceConfig{
		{Name: "source-a", Type: "claude-code", Path: home1},
		{Name: "source-b", Type: "claude-code", Path: home2},
	}

	syncer := New(database, sources, WithFullSync(true))
	stats, err := syncer.Run()
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	if stats.SessionsScanned != 2 {
		t.Errorf("expected 2 sessions scanned, got %d", stats.SessionsScanned)
	}
	if stats.SessionsIndexed != 2 {
		t.Errorf("expected 2 sessions indexed, got %d", stats.SessionsIndexed)
	}
}

func TestOptionsApply(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	var gotProgress bool
	var gotCount bool

	syncer := New(database, nil,
		WithFullSync(true),
		WithVerbose(true),
		WithProgressCallback(func(msg string) { gotProgress = true }),
		WithCountProgressCallback(func(c, total int) { gotCount = true }),
	)

	if !syncer.full {
		t.Error("expected full=true")
	}
	if !syncer.verbose {
		t.Error("expected verbose=true")
	}

	syncer.onProgress("test")
	if !gotProgress {
		t.Error("progress callback not called")
	}

	syncer.onCountProgress(1, 10)
	if !gotCount {
		t.Error("count progress callback not called")
	}
}

// TestSyncer_FullFlagClearsStaleData is the integration test for the --full
// bug fix from PR #22. Without ResetAll(), running --full against a source
// where files have been renamed or removed leaves the old rows behind and
// the DB drifts from the source of truth. With ResetAll(), --full wipes
// the archive so re-scanning produces a clean state.
func TestSyncer_FullFlagClearsStaleData(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	claudeHome, err := os.MkdirTemp("", "claude-home-full-*")
	if err != nil {
		t.Fatalf("create claude home: %v", err)
	}
	defer func() { _ = os.RemoveAll(claudeHome) }()

	// First sync: one session that we'll later "delete" from disk to simulate
	// a rename/removal upstream.
	writeTestSession(t, claudeHome, "aaaaaaaa-1111-2222-3333-444444444444", "-Users-test-old-project")
	sources := []config.SourceConfig{
		{Name: "claude-code", Type: "claude-code", Path: claudeHome},
	}

	first := New(database, sources)
	stats1, err := first.Run()
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if stats1.SessionsIndexed != 1 {
		t.Fatalf("first sync: SessionsIndexed = %d, want 1", stats1.SessionsIndexed)
	}

	var beforeSessions, beforeProjects int
	if err := database.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&beforeSessions); err != nil {
		t.Fatalf("count sessions before: %v", err)
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM projects").Scan(&beforeProjects); err != nil {
		t.Fatalf("count projects before: %v", err)
	}
	if beforeSessions != 1 || beforeProjects != 1 {
		t.Fatalf("pre-condition: sessions=%d projects=%d, want 1/1", beforeSessions, beforeProjects)
	}

	// Simulate the upstream state changing: the old project directory is
	// removed and a new one takes its place.
	if err := os.RemoveAll(filepath.Join(claudeHome, "projects", "-Users-test-old-project")); err != nil {
		t.Fatalf("remove old project dir: %v", err)
	}
	writeTestSession(t, claudeHome, "bbbbbbbb-5555-6666-7777-888888888888", "-Users-test-new-project")

	// Full sync: should wipe the old session/project and index the new one.
	full := New(database, sources, WithFullSync(true))
	stats2, err := full.Run()
	if err != nil {
		t.Fatalf("full sync: %v", err)
	}
	if stats2.SessionsIndexed != 1 {
		t.Errorf("full sync SessionsIndexed = %d, want 1", stats2.SessionsIndexed)
	}

	var afterSessions, afterProjects int
	if err := database.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&afterSessions); err != nil {
		t.Fatalf("count sessions after: %v", err)
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM projects").Scan(&afterProjects); err != nil {
		t.Fatalf("count projects after: %v", err)
	}
	// Must be exactly 1 of each — the old rows are gone, only the new session survives.
	if afterSessions != 1 {
		t.Errorf("sessions after full sync = %d, want 1 (old row should be gone)", afterSessions)
	}
	if afterProjects != 1 {
		t.Errorf("projects after full sync = %d, want 1 (old row should be gone)", afterProjects)
	}

	// The one surviving session must be the new one, not the old one.
	var surviving string
	if err := database.QueryRow("SELECT id FROM sessions").Scan(&surviving); err != nil {
		t.Fatalf("read surviving session: %v", err)
	}
	if surviving != "bbbbbbbb-5555-6666-7777-888888888888" {
		t.Errorf("surviving session = %q, want bbbbbbbb-... (old session should have been wiped)", surviving)
	}
}

// TestSyncer_IncrementalDoesNotClear guards against the opposite regression:
// an ordinary (non-full) sync must NOT invoke ResetAll and must preserve
// rows that no longer have a corresponding source file (e.g. a user briefly
// moved their ~/.claude to another disk). Only --full is destructive.
func TestSyncer_IncrementalDoesNotClear(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	claudeHome, err := os.MkdirTemp("", "claude-home-incr-*")
	if err != nil {
		t.Fatalf("create claude home: %v", err)
	}
	defer func() { _ = os.RemoveAll(claudeHome) }()

	writeTestSession(t, claudeHome, "cccccccc-9999-0000-1111-222222222222", "-Users-test-persist")
	sources := []config.SourceConfig{
		{Name: "claude-code", Type: "claude-code", Path: claudeHome},
	}

	first := New(database, sources)
	if _, err := first.Run(); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	// Remove the source file; run another INCREMENTAL sync.
	if err := os.RemoveAll(filepath.Join(claudeHome, "projects", "-Users-test-persist")); err != nil {
		t.Fatalf("remove source: %v", err)
	}

	second := New(database, sources) // NOT WithFullSync(true)
	if _, err := second.Run(); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	// The old session should still be in the DB — incremental does NOT prune.
	var n int
	if err := database.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&n); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if n != 1 {
		t.Errorf("incremental sync sessions = %d, want 1 (row must survive an incremental resync)", n)
	}
}
