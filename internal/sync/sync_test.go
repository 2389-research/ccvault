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
