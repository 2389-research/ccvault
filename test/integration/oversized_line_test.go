// ABOUTME: End-to-end sync test for issue #11 (oversized JSONL line handling).
// ABOUTME: Verifies sessions with a huge line still index and mtime persists.

package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	// Register adapters so their init() functions run
	_ "github.com/2389-research/ccvault/pkg/adapter/claudecode"

	"github.com/2389-research/ccvault/internal/config"
	"github.com/2389-research/ccvault/internal/db"
	"github.com/2389-research/ccvault/internal/sync"
)

// TestSync_OversizedJSONLLine_IndexesAndRecordsMtime is the end-to-end
// integration counterpart to the parser-level repro in issue #11.
//
// Before the fix, a JSONL line larger than bufio.Scanner's 10 MB cap aborted
// parsing and left the file un-indexed. Sync never recorded the mtime for such
// a file, so subsequent syncs retried it endlessly. After the fix, the file
// syncs cleanly and the mtime is recorded — the second sync is a no-op.
func TestSync_OversizedJSONLLine_IndexesAndRecordsMtime(t *testing.T) {
	tmpDir := t.TempDir()

	claudeDir := filepath.Join(tmpDir, "claude")
	projectDir := filepath.Join(claudeDir, "projects", "-Users-test-oversized")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatalf("create project dir: %v", err)
	}

	sessionFile := filepath.Join(projectDir, "22222222-3333-4444-5555-666666666666.jsonl")

	// ~12 MB of ASCII inside a valid JSON string field — mirrors a base64
	// PDF page Claude Code writes as a single line. Also throw in one truly
	// malformed line to confirm the skipped-line counter kicks in.
	oversized := strings.Repeat("B", 12*1024*1024)
	jsonl := fmt.Sprintf(
		`{"uuid":"turn-1","parentUuid":null,"type":"user","sessionId":"sess-e2e-oversized","timestamp":"2026-08-04T10:00:00Z","cwd":"/Users/test/oversized","message":{"role":"user","content":"hello"}}
{"uuid":"turn-2","parentUuid":"turn-1","type":"user","sessionId":"sess-e2e-oversized","timestamp":"2026-08-04T10:00:01Z","message":{"role":"user","content":"%s"}}
this line is not valid json and should be counted as skipped
{"uuid":"turn-3","parentUuid":"turn-2","type":"assistant","sessionId":"sess-e2e-oversized","timestamp":"2026-08-04T10:00:02Z","message":{"id":"msg-1","model":"claude-sonnet-4-20250514","role":"assistant","content":[{"type":"text","text":"world"}],"usage":{"input_tokens":10,"output_tokens":5}}}
`,
		oversized,
	)
	if err := os.WriteFile(sessionFile, []byte(jsonl), 0644); err != nil {
		t.Fatalf("write session: %v", err)
	}

	sources := []config.SourceConfig{
		{Name: "claude-code", Type: "claude-code", Path: claudeDir},
	}

	database, err := db.Open(filepath.Join(tmpDir, "data"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = database.Close() }()

	// First sync: file is new, should be parsed and indexed.
	stats, err := sync.New(database, sources, sync.WithFullSync(true)).Run()
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if stats.SessionsIndexed != 1 {
		t.Errorf("SessionsIndexed = %d, want 1", stats.SessionsIndexed)
	}
	if stats.TotalSkippedLines != 1 {
		t.Errorf("TotalSkippedLines = %d, want 1 (the malformed line; oversized ≠ malformed)",
			stats.TotalSkippedLines)
	}
	if stats.SessionsWithSkippedLines != 1 {
		t.Errorf("SessionsWithSkippedLines = %d, want 1", stats.SessionsWithSkippedLines)
	}
	if stats.TurnsWithTruncatedRawJSON != 1 {
		t.Errorf("TurnsWithTruncatedRawJSON = %d, want 1 (the 12MB middle turn)",
			stats.TurnsWithTruncatedRawJSON)
	}
	if stats.SessionsWithTruncatedTurns != 1 {
		t.Errorf("SessionsWithTruncatedTurns = %d, want 1", stats.SessionsWithTruncatedTurns)
	}

	// Verify the session actually landed with the expected turn count.
	session, err := database.GetSession("sess-e2e-oversized")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if session == nil {
		t.Fatal("session not persisted")
	}
	if session.TurnCount != 3 {
		t.Errorf("session.TurnCount = %d, want 3", session.TurnCount)
	}

	// Load the persisted turns and verify the middle turn's raw_json is the
	// small placeholder shape, not the original 12 MB blob.
	turns, err := database.GetTurns("sess-e2e-oversized")
	if err != nil {
		t.Fatalf("get turns: %v", err)
	}
	if len(turns) != 3 {
		t.Fatalf("expected 3 persisted turns, got %d", len(turns))
	}
	middleRaw := turns[1].RawJSON
	if len(middleRaw) > 500 {
		t.Errorf("middle turn raw_json not truncated in DB: len=%d bytes", len(middleRaw))
	}
	var placeholder map[string]any
	if err := json.Unmarshal(middleRaw, &placeholder); err != nil {
		t.Fatalf("persisted raw_json is not valid JSON: %v", err)
	}
	if placeholder["_ccvault_stripped"] != true {
		t.Errorf("persisted raw_json missing _ccvault_stripped marker: %v", placeholder)
	}
	if origSize, ok := placeholder["_ccvault_original_size"].(float64); !ok || origSize < 12*1024*1024 {
		t.Errorf("_ccvault_original_size = %v, want ≥ 12MB", placeholder["_ccvault_original_size"])
	}

	// Verify mtime was recorded — this is Bug B in the issue: previously,
	// parse errors bypassed the mtime upsert and every subsequent sync
	// retried the same file endlessly.
	mtimes, err := database.GetAllSourceMtimes("claude-code")
	if err != nil {
		t.Fatalf("get mtimes: %v", err)
	}
	if _, ok := mtimes[sessionFile]; !ok {
		t.Fatalf("mtime not recorded for %s (would cause infinite retry)", sessionFile)
	}

	// Second sync (incremental, not --full): the file's mtime hasn't moved,
	// so the sync should skip it entirely, not re-parse.
	stats2, err := sync.New(database, sources).Run()
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if stats2.SessionsIndexed != 0 {
		t.Errorf("second sync SessionsIndexed = %d, want 0 (should be no-op)",
			stats2.SessionsIndexed)
	}
	if stats2.SessionsSkipped == 0 {
		t.Errorf("second sync SessionsSkipped = 0, want ≥1")
	}
}

// TestSync_AllMalformedFile_StillCountsSkippedLines guards the regression
// CodeRabbit flagged on PR #12: when a session file contains no parseable
// turns, processSession's early return must not swallow the parser's
// skipped-line diagnostics — otherwise a fully corrupt file is silently
// invisible in the sync summary.
func TestSync_AllMalformedFile_StillCountsSkippedLines(t *testing.T) {
	tmpDir := t.TempDir()
	claudeDir := filepath.Join(tmpDir, "claude")
	projectDir := filepath.Join(claudeDir, "projects", "-Users-test-junk")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatalf("create project dir: %v", err)
	}

	// Three malformed lines, zero valid turns → parsed.ID == "".
	sessionFile := filepath.Join(projectDir, "77777777-8888-9999-aaaa-bbbbbbbbbbbb.jsonl")
	if err := os.WriteFile(sessionFile, []byte("not json\nalso not json\n{unclosed\n"), 0644); err != nil {
		t.Fatalf("write session: %v", err)
	}

	sources := []config.SourceConfig{
		{Name: "claude-code", Type: "claude-code", Path: claudeDir},
	}

	database, err := db.Open(filepath.Join(tmpDir, "data"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = database.Close() }()

	stats, err := sync.New(database, sources, sync.WithFullSync(true)).Run()
	if err != nil {
		t.Fatalf("sync: %v", err)
	}

	// The file has no valid session so it should not be "indexed"...
	if stats.SessionsIndexed != 0 {
		t.Errorf("SessionsIndexed = %d, want 0", stats.SessionsIndexed)
	}
	// ...but the malformed lines MUST still land in the totals.
	if stats.TotalSkippedLines != 3 {
		t.Errorf("TotalSkippedLines = %d, want 3", stats.TotalSkippedLines)
	}
	if stats.SessionsWithSkippedLines != 1 {
		t.Errorf("SessionsWithSkippedLines = %d, want 1", stats.SessionsWithSkippedLines)
	}
}
