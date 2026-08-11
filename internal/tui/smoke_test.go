// ABOUTME: Smoke test that dumps rendered TUI views at 80/100/140 cols for eyeball inspection.
// ABOUTME: Kept in-tree so future manual smoke checks can just run `go test -v -run TestSmoke`.

package tui

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TestSmokeManualEyeball renders each TUI model at three terminal
// widths and prints the output. Purely for human eyeball inspection —
// no assertions. Run with:
//
//	go test ./internal/tui -run TestSmokeManualEyeball -v
//
// Use to verify that the Faint compactStyle fix, tier layout arithmetic,
// and Unicode handling all render as expected. Skipped under -short.
func TestSmokeManualEyeball(t *testing.T) {
	if os.Getenv("CCVAULT_SMOKE") == "" {
		t.Skip("set CCVAULT_SMOKE=1 to run this render-dump smoke test")
	}

	database := openTUITestDB(t)
	now := time.Now().UTC()
	t.Setenv("HOME", "/Users/testuser")

	// Mixed fixtures: home-prefix path, non-home path, deep-nested path,
	// multibyte path, short path — covers every branch of compact.Path
	// + projectref.Label under realistic-looking sources.
	fixtures := []struct {
		path, name, source string
		sessionCount       int
		tokens             int64
	}{
		{"/Users/testuser/work/2389/ccvault", "ccvault", "claude-code", 42, 1_234_567},
		{"/Users/other/personal/ccvault", "ccvault", "claude-code", 8, 45_678},
		{"/opt/team/very/deep/nested/experiment", "experiment", "nanoclaw", 15, 891_234},
		{"/Users/testuser/α-project", "α-project", "codex", 3, 12_000},
		{"/short", "short", "hex", 1, 500},
	}
	for _, f := range fixtures {
		_, err := database.Exec(`INSERT INTO projects
			(path, display_name, first_seen_at, last_activity_at, session_count, total_tokens, source)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			f.path, f.name, now, now, f.sessionCount, f.tokens, f.source)
		if err != nil {
			t.Fatalf("insert %s: %v", f.path, err)
		}
	}
	// Add sessions under one project so Sessions view has content.
	_, _ = seedProjectAndSessions(t, database, []string{"claude-code", "nanoclaw"})

	widths := []int{80, 100, 140}
	for _, w := range widths {
		// "═" is a 3-byte rune; use Repeat + rune count for the sub-banner
		// so we don't slice mid-codepoint (byte slicing here reproduced
		// the exact class of bug the compact package guards against).
		banner := strings.Repeat("═", w)
		subBanner := strings.Repeat("═", 20)
		t.Logf("\n\n%s\n%s ─ terminal width = %d ─\n%s", banner, subBanner, w, banner)

		// Projects
		pm := NewProjectsModel(database)
		if msg := pm.loadProjects(); msg != nil {
			pm.Update(msg)
		}
		pm.SetSize(w, 30)
		t.Logf("\n── Projects @ %d cols ──\n%s", w, pm.View())

		// Sessions (unfiltered)
		sm := NewSessionsModel(database)
		if msg := sm.loadSessions(); msg != nil {
			sm.Update(msg)
		}
		sm.SetSize(w, 30)
		t.Logf("\n── Sessions (unfiltered) @ %d cols ──\n%s", w, sm.View())
	}
}
