// ABOUTME: End-to-end integration test for multi-source sync and search
// ABOUTME: Verifies that Claude Code and Codex sessions are synced and searchable with source filtering

package integration

import (
	"os"
	"path/filepath"
	"testing"

	// Register adapters so their init() functions run
	_ "github.com/2389-research/ccvault/pkg/adapter/claudecode"
	_ "github.com/2389-research/ccvault/pkg/adapter/codex"
	_ "github.com/2389-research/ccvault/pkg/adapter/hex"
	_ "github.com/2389-research/ccvault/pkg/adapter/jeff"

	"github.com/2389-research/ccvault/internal/config"
	"github.com/2389-research/ccvault/internal/db"
	"github.com/2389-research/ccvault/internal/search"
	"github.com/2389-research/ccvault/internal/sync"
)

func TestMultiSourceSyncAndSearch(t *testing.T) {
	// Set up temp directories
	tmpDir := t.TempDir()

	// --- Claude Code session data ---
	claudeDir := filepath.Join(tmpDir, "claude")
	claudeProjectDir := filepath.Join(claudeDir, "projects", "-Users-test-myproject")
	if err := os.MkdirAll(claudeProjectDir, 0755); err != nil {
		t.Fatalf("create claude project dir: %v", err)
	}

	claudeSessionFile := filepath.Join(claudeProjectDir, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee.jsonl")
	claudeJSONL := `{"uuid":"turn-1","parentUuid":null,"type":"user","message":{"role":"user","content":"hello from claude"},"timestamp":"2026-01-01T00:00:01Z","sessionId":"session-cc-1","cwd":"/Users/test/myproject","version":"1.0"}
{"uuid":"turn-2","parentUuid":"turn-1","type":"assistant","message":{"id":"msg-1","model":"claude-sonnet-4-20250514","role":"assistant","content":[{"type":"text","text":"Hello! How can I help?"}],"usage":{"input_tokens":10,"output_tokens":5}},"timestamp":"2026-01-01T00:00:02Z","sessionId":"session-cc-1"}
`
	if err := os.WriteFile(claudeSessionFile, []byte(claudeJSONL), 0644); err != nil {
		t.Fatalf("write claude session: %v", err)
	}

	// --- Codex session data ---
	codexDir := filepath.Join(tmpDir, "codex")
	codexSessionDir := filepath.Join(codexDir, "sessions", "2026", "03", "11")
	if err := os.MkdirAll(codexSessionDir, 0755); err != nil {
		t.Fatalf("create codex session dir: %v", err)
	}

	codexSessionFile := filepath.Join(codexSessionDir, "rollout-2026-03-11T11-00-00-11111111-2222-3333-4444-555555555555.jsonl")
	codexJSONL := `{"timestamp":"2026-03-11T16:00:00.000Z","type":"session_meta","payload":{"id":"session-codex-1","timestamp":"2026-03-11T16:00:00.000Z","cwd":"/Users/test/codexproject","cli_version":"0.114.0","model_provider":"openai","git":{"branch":"main"}}}
{"timestamp":"2026-03-11T16:00:01.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hello from codex"}]}}
{"timestamp":"2026-03-11T16:00:02.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hi there!"}]}}
{"timestamp":"2026-03-11T16:00:02.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":15,"output_tokens":8,"total_tokens":23}}}}
`
	if err := os.WriteFile(codexSessionFile, []byte(codexJSONL), 0644); err != nil {
		t.Fatalf("write codex session: %v", err)
	}

	// --- Jeff session data ---
	jeffDir := filepath.Join(tmpDir, "jeff")
	jeffSessionsDir := filepath.Join(jeffDir, "sessions")
	if err := os.MkdirAll(jeffSessionsDir, 0755); err != nil {
		t.Fatalf("create jeff sessions dir: %v", err)
	}
	jeffSessionFile := filepath.Join(jeffSessionsDir, "20260224_195605.jsonl")
	jeffJSONL := `{"timestamp":"2026-02-24T19:56:05.125559Z","entry_type":"session_start","conversation_id":"jeff-conv-1","data":{"model":"claude-sonnet-4-5","session_id":"20260224_195605"}}
{"timestamp":"2026-02-24T19:56:10.000000Z","entry_type":"user_message","conversation_id":"jeff-conv-1","data":{"content":"hello from jeff"}}
{"timestamp":"2026-02-24T19:56:15.000000Z","entry_type":"assistant_message","conversation_id":"jeff-conv-1","data":{"content":"Greetings from Jeff!"}}
`
	if err := os.WriteFile(jeffSessionFile, []byte(jeffJSONL), 0644); err != nil {
		t.Fatalf("write jeff session: %v", err)
	}

	// --- Hex session data ---
	hexDir := filepath.Join(tmpDir, "hex")
	hexSessionsDir := filepath.Join(hexDir, "sessions")
	if err := os.MkdirAll(hexSessionsDir, 0755); err != nil {
		t.Fatalf("create hex sessions dir: %v", err)
	}
	hexSessionFile := filepath.Join(hexSessionsDir, "hex-1.json")
	hexJSON := `{"id":"hex-1","title":"test","created_at":"2026-01-12T11:48:34Z","updated_at":"2026-01-12T11:48:50Z","messages":[{"role":"user","content":"hello from hex","timestamp":"2026-01-12T11:48:34Z"},{"role":"assistant","content":"Hex says hi","timestamp":"2026-01-12T11:48:50Z"}],"favorite":false}`
	if err := os.WriteFile(hexSessionFile, []byte(hexJSON), 0644); err != nil {
		t.Fatalf("write hex session: %v", err)
	}

	// --- Multi-source config ---
	sources := []config.SourceConfig{
		{Name: "claude-code", Type: "claude-code", Path: claudeDir},
		{Name: "codex", Type: "codex", Path: codexDir},
		{Name: "jeff", Type: "jeff", Path: jeffDir},
		{Name: "hex", Type: "hex", Path: hexDir},
	}

	// --- Open temporary SQLite database ---
	dbDir := filepath.Join(tmpDir, "data")
	database, err := db.Open(dbDir)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer func() { _ = database.Close() }()

	// --- Run sync ---
	syncer := sync.New(database, sources, sync.WithFullSync(true))
	stats, err := syncer.Run()
	if err != nil {
		t.Fatalf("sync run: %v", err)
	}

	// Verify sync indexed sessions from all four sources
	if stats.SessionsIndexed < 4 {
		t.Errorf("expected at least 4 sessions indexed, got %d", stats.SessionsIndexed)
	}

	// --- Verify source column values via search ---
	searcher := search.New(database.DB)

	// Search with source:codex should return only Codex sessions
	codexQuery := search.Parse("source:codex")
	codexResults, err := searcher.Search(codexQuery, 100)
	if err != nil {
		t.Fatalf("search source:codex: %v", err)
	}
	if len(codexResults) == 0 {
		t.Error("expected results for source:codex, got none")
	}
	for _, r := range codexResults {
		// Verify these are from codex sessions by checking content
		if r.Turn.Content != "hello from codex" && r.Turn.Content != "Hi there!" {
			t.Errorf("unexpected content in codex result: %q", r.Turn.Content)
		}
	}

	// Search with source:claude-code should return only Claude Code sessions
	ccQuery := search.Parse("source:claude-code")
	ccResults, err := searcher.Search(ccQuery, 100)
	if err != nil {
		t.Fatalf("search source:claude-code: %v", err)
	}
	if len(ccResults) == 0 {
		t.Error("expected results for source:claude-code, got none")
	}
	for _, r := range ccResults {
		if r.Turn.Content != "hello from claude" && r.Turn.Content != "Hello! How can I help?" {
			t.Errorf("unexpected content in claude-code result: %q", r.Turn.Content)
		}
	}

	// Search without source filter should return results from both
	allQuery := search.Parse("hello")
	allResults, err := searcher.Search(allQuery, 100)
	if err != nil {
		t.Fatalf("search hello: %v", err)
	}

	// We expect at least one result from each source
	var foundClaude, foundCodex bool
	for _, r := range allResults {
		if r.Turn.Content == "hello from claude" {
			foundClaude = true
		}
		if r.Turn.Content == "hello from codex" {
			foundCodex = true
		}
	}
	if !foundClaude {
		t.Error("search for 'hello' did not return Claude Code session")
	}
	if !foundCodex {
		t.Error("search for 'hello' did not return Codex session")
	}

	// Verify no cross-contamination: codex search should not return claude results
	for _, r := range codexResults {
		if r.Turn.Content == "hello from claude" || r.Turn.Content == "Hello! How can I help?" {
			t.Error("source:codex search returned a Claude Code result")
		}
	}
	for _, r := range ccResults {
		if r.Turn.Content == "hello from codex" || r.Turn.Content == "Hi there!" {
			t.Error("source:claude-code search returned a Codex result")
		}
	}

	// --- source:jeff returns only Jeff sessions ---
	jeffQuery := search.Parse("source:jeff")
	jeffResults, err := searcher.Search(jeffQuery, 100)
	if err != nil {
		t.Fatalf("search source:jeff: %v", err)
	}
	if len(jeffResults) == 0 {
		t.Error("expected results for source:jeff, got none")
	}
	for _, r := range jeffResults {
		if r.Turn.Content != "hello from jeff" && r.Turn.Content != "Greetings from Jeff!" {
			t.Errorf("unexpected content in jeff result: %q", r.Turn.Content)
		}
	}

	// --- source:hex returns only Hex sessions ---
	hexQuery := search.Parse("source:hex")
	hexResults, err := searcher.Search(hexQuery, 100)
	if err != nil {
		t.Fatalf("search source:hex: %v", err)
	}
	if len(hexResults) == 0 {
		t.Error("expected results for source:hex, got none")
	}
	for _, r := range hexResults {
		if r.Turn.Content != "hello from hex" && r.Turn.Content != "Hex says hi" {
			t.Errorf("unexpected content in hex result: %q", r.Turn.Content)
		}
	}

	// --- Unfiltered search reaches all four sources ---
	helloAll := search.Parse("hello")
	helloAllResults, err := searcher.Search(helloAll, 100)
	if err != nil {
		t.Fatalf("search hello (all sources): %v", err)
	}
	var foundJeff, foundHex bool
	for _, r := range helloAllResults {
		switch r.Turn.Content {
		case "hello from jeff":
			foundJeff = true
		case "hello from hex":
			foundHex = true
		}
	}
	if !foundJeff {
		t.Error("search for 'hello' did not return Jeff session")
	}
	if !foundHex {
		t.Error("search for 'hello' did not return Hex session")
	}
}
