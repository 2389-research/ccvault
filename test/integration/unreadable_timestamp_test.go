// ABOUTME: #146 through the real binary — sync names the turns whose transcript timestamp it could not read.
// ABOUTME: Also pins #155 end-to-end: a synced session's cache columns carry what the transcript recorded.

package integration

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// TestCLI_SyncReportsUnreadableTimestamps is #146's visibility half at the
// layer a person actually reads.
//
// pkg/parser, pkg/adapter/codex and pkg/adapter/jeff each keep their own
// timestamp layout list, deliberately — they parse three different tools'
// transcript formats, which change independently of each other and of us, and
// a shared list would couple them (besides pointing pkg/ at internal/db).
// What the three lacked was a signal: a timestamp none of the layouts match is
// replaced by a fallback, which sorts somewhere arbitrary in the archive and
// answers date predicates exactly like a value someone recorded.
//
// Reporting, not refusal. The turn is still indexed.
func TestCLI_SyncReportsUnreadableTimestamps(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	f := newCLIFixture(t)

	// A clean archive must say nothing, or the line is noise nobody reads.
	if clean := f.Run(t, "sync"); strings.Contains(clean.Stdout, "Unreadable timestamps") {
		t.Errorf("a clean sync reports unreadable timestamps:\n%s", clean.Stdout)
	}

	const (
		sessionID = "dddddddd-eeee-ffff-0000-111111111111"
		bad       = "2026/01/01 00:00:03"
	)
	writeSessionWithTimestamp(t, f.ClaudeHome, "-Users-test-badclock", sessionID, bad)

	out := f.Run(t, "sync").Stdout
	if !strings.Contains(out, "Unreadable timestamps") {
		t.Errorf("sync does not report the unreadable timestamp:\n%s", out)
	}
	// The sample is the actionable half: it names the layout that is missing.
	if !strings.Contains(out, bad) {
		t.Errorf("sync does not name the unreadable value %q:\n%s", bad, out)
	}

	// The turn survives. An unreadable clock is not a reason to lose the
	// conversation — the same call #144 made for non-canonical stored values.
	turns := countTurns(t, f.DataDir, sessionID)
	if turns != 2 {
		t.Errorf("session %s holds %d turns, want 2", sessionID, turns)
	}
}

// TestCLI_SyncPersistsCacheTokens is #155 through the binary: the columns were
// 0 for all 47,409 sessions in the author's archive because nothing assigned
// the values the parser had already read.
func TestCLI_SyncPersistsCacheTokens(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	f := newCLIFixture(t)

	const sessionID = "cccccccc-dddd-eeee-ffff-000011112222"
	writeSessionWithUsage(t, f.ClaudeHome, "-Users-test-cachetok", sessionID,
		`{"input_tokens":11,"output_tokens":7,"cache_read_input_tokens":90000,"cache_creation_input_tokens":4000}`)
	f.Run(t, "sync")

	pool := openFixtureArchive(t, f.DataDir)
	var cacheRead, cacheWrite, input, output int64
	if err := pool.QueryRow(`
		SELECT input_tokens, output_tokens, cache_read_tokens, cache_write_tokens
		FROM sessions WHERE id = ?`, sessionID).
		Scan(&input, &output, &cacheRead, &cacheWrite); err != nil {
		t.Fatalf("read session tokens: %v", err)
	}

	if cacheRead != 90000 || cacheWrite != 4000 {
		t.Errorf("cache_read_tokens/cache_write_tokens = %d/%d, want 90000/4000", cacheRead, cacheWrite)
	}
	if input != 11 || output != 7 {
		t.Errorf("input_tokens/output_tokens = %d/%d, want 11/7 — the counted fields must not move",
			input, output)
	}

	// stats reports the sum of all four, so the archive's headline number is
	// the one that was off by ~256x.
	if out := f.Run(t, "stats").Stdout; !strings.Contains(out, "Total Tokens") {
		t.Fatalf("stats does not report Total Tokens:\n%s", out)
	}
}

// writeSessionWithTimestamp writes a two-turn session whose assistant turn
// carries the given timestamp string verbatim, readable or not.
func writeSessionWithTimestamp(t *testing.T, claudeHome, encodedProject, sessionID, assistantTimestamp string) {
	t.Helper()
	writeFixtureSession(t, claudeHome, encodedProject, sessionID,
		`{"input_tokens":11,"output_tokens":7}`, assistantTimestamp)
}

// writeSessionWithUsage writes a two-turn session whose assistant turn carries
// the given usage object verbatim.
func writeSessionWithUsage(t *testing.T, claudeHome, encodedProject, sessionID, usage string) {
	t.Helper()
	writeFixtureSession(t, claudeHome, encodedProject, sessionID, usage, "2026-01-01T00:00:02Z")
}

func writeFixtureSession(t *testing.T, claudeHome, encodedProject, sessionID, usage, assistantTimestamp string) {
	t.Helper()

	dir := filepath.Join(claudeHome, "projects", encodedProject)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	jsonl := fmt.Sprintf(`{"uuid":"%[1]s-t1","parentUuid":null,"type":"user","message":{"role":"user","content":"hello"},"timestamp":"2026-01-01T00:00:01Z","sessionId":%[1]q,"cwd":"/Users/test/fixture","version":"1.0"}
{"uuid":"%[1]s-t2","parentUuid":"%[1]s-t1","type":"assistant","message":{"id":"%[1]s-m1","model":"claude-sonnet-4-20250514","role":"assistant","content":[{"type":"text","text":"acknowledged"}],"usage":%[2]s},"timestamp":%[3]q,"sessionId":%[1]q}
`, sessionID, usage, assistantTimestamp)
	if err := os.WriteFile(filepath.Join(dir, sessionID+".jsonl"), []byte(jsonl), 0o644); err != nil {
		t.Fatalf("write session: %v", err)
	}
}

func openFixtureArchive(t *testing.T, dataDir string) *sql.DB {
	t.Helper()

	pool, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "ccvault.db"))
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	return pool
}

func countTurns(t *testing.T, dataDir, sessionID string) int {
	t.Helper()

	var n int
	if err := openFixtureArchive(t, dataDir).
		QueryRow("SELECT COUNT(*) FROM turns WHERE session_id = ?", sessionID).Scan(&n); err != nil {
		t.Fatalf("count turns: %v", err)
	}
	return n
}
