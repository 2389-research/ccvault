// ABOUTME: Tests for TUI lifecycle — sync cancellation on navigate-away and force-quit.
// ABOUTME: Drives the real app Model against a real SQLite DB and real synthetic session files.

package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/2389-research/ccvault/internal/config"

	// Register the claude-code adapter so sync can discover synthetic sessions.
	_ "github.com/2389-research/ccvault/pkg/adapter/claudecode"
)

// syncGoroutineRunning reports whether any goroutine is currently inside
// sync.(*Syncer).Run. Scanning stacks rather than counting goroutines keeps
// the signal specific: SQLite and the test runner spawn their own.
func syncGoroutineRunning() bool {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	return strings.Contains(string(buf[:n]), "internal/sync.(*Syncer).Run")
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// writeSyntheticClaudeHome creates a Claude-Code-shaped source tree with
// `count` single-turn sessions. Enough of them to overflow the TUI's
// 100-slot progress channels is the point: that is when the sync goroutine
// starts blocking on send.
func writeSyntheticClaudeHome(t *testing.T, count int) string {
	t.Helper()
	home := t.TempDir()
	projDir := filepath.Join(home, "projects", "-Users-test-leaky")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)

	for i := 0; i < count; i++ {
		// The scanner only accepts UUID-shaped filenames, so the session IDs
		// have to look like real ones.
		sessionID := fmt.Sprintf("a1b2c3d4-e5f6-7890-abcd-%012d", i)
		turnID := sessionID + "-turn-1"
		msg := map[string]any{
			"uuid":      turnID,
			"sessionId": sessionID,
			"type":      "human",
			"timestamp": now.Format(time.RFC3339Nano),
			"cwd":       "/Users/test/leaky",
			"message": map[string]any{
				"role":    "user",
				"content": "hello",
			},
		}
		line, err := json.Marshal(msg)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		path := filepath.Join(projDir, sessionID+".jsonl")
		if err := os.WriteFile(path, append(line, '\n'), 0o600); err != nil {
			t.Fatalf("write session: %v", err)
		}
	}
	return home
}

// newLeakTestModel builds an app Model wired to a real DB and a synthetic
// source tree big enough that sync outlives a few hundred milliseconds.
func newLeakTestModel(t *testing.T) *Model {
	t.Helper()
	database := openTUITestDB(t)
	home := writeSyntheticClaudeHome(t, 400)
	sources := []config.SourceConfig{
		{Name: "claude-code", Type: "claude-code", Path: home},
	}
	return New(database, filepath.Join(t.TempDir(), "analytics"), sources)
}

// startSyncAndStall navigates dashboard -> syncing and waits until the sync
// goroutine has filled the count channel, i.e. its next send will block.
// Nothing ever drains the channels here, which is exactly the state the TUI
// is in once the user has navigated away from the syncing view.
func startSyncAndStall(t *testing.T, m *Model) {
	t.Helper()
	if _, cmd := m.Update(NavigateMsg{View: SyncingView}); cmd == nil {
		t.Fatal("expected a command from navigating to the syncing view")
	}
	if m.view != SyncingView {
		t.Fatalf("expected SyncingView, got %v", m.view)
	}
	if len(m.viewStack) != 2 {
		t.Fatalf("expected view stack depth 2, got %d", len(m.viewStack))
	}
	if !waitFor(20*time.Second, func() bool { return len(m.syncing.countCh) == cap(m.syncing.countCh) }) {
		t.Fatalf("sync goroutine never filled the count channel (len %d)", len(m.syncing.countCh))
	}

	// Keep the test honest: there must be a live sync goroutine with work left
	// to do, otherwise "no goroutine afterwards" proves nothing.
	if len(m.syncing.doneCh) != 0 {
		t.Fatal("sync finished before it could be interrupted; the fixture is too small")
	}
	if !syncGoroutineRunning() {
		t.Fatal("no sync goroutine running at the point of interruption")
	}
}

// TestPopViewCancelsSyncGoroutine covers issue #24: pressing q in the syncing
// view pops back to the dashboard, after which SyncingModel.Update is never
// called again and the sync goroutine blocks forever on a full channel.
func TestPopViewCancelsSyncGoroutine(t *testing.T) {
	m := newLeakTestModel(t)
	startSyncAndStall(t, m)

	// User presses q: context-sensitive back, not quit.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if m.view != DashboardView {
		t.Fatalf("expected q to pop back to DashboardView, got %v", m.view)
	}

	if !waitFor(10*time.Second, func() bool { return !syncGoroutineRunning() }) {
		t.Fatal("sync goroutine still running after navigating away from the syncing view")
	}
}

// TestForceQuitCancelsSyncGoroutine covers #32 meeting #24: ctrl+c during a
// sync must quit AND unwind the sync goroutine before the caller's
// database.Close() runs.
func TestForceQuitCancelsSyncGoroutine(t *testing.T) {
	m := newLeakTestModel(t)
	startSyncAndStall(t, m)

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("ctrl+c returned no command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("ctrl+c during sync should quit, got %T", cmd())
	}

	// shutdownSync is what tui.Run calls after the program loop ends, before
	// the caller closes the database. It must not return early.
	m.shutdownSync()
	if syncGoroutineRunning() {
		t.Fatal("shutdownSync returned while the sync goroutine was still running")
	}
}

// TestForceQuitFromNestedView covers issue #32: ctrl+c quits from every view,
// not just the dashboard.
func TestForceQuitFromNestedView(t *testing.T) {
	database := openTUITestDB(t)
	projectID, sessionIDs := seedProjectAndSessions(t, database, []string{"claude-code"})
	_ = projectID
	m := New(database, t.TempDir(), nil)

	views := []struct {
		name string
		view View
		data interface{}
	}{
		{"projects", ProjectsView, nil},
		{"sessions", SessionsView, projectID},
		{"conversation", ConversationView, sessionIDs[0]},
		{"search", SearchView, nil},
		{"analytics", AnalyticsView, nil},
	}

	for _, tc := range views {
		t.Run(tc.name, func(t *testing.T) {
			m.view = DashboardView
			m.viewStack = []View{DashboardView}
			m.pushView(tc.view, tc.data)
			if m.view != tc.view {
				t.Fatalf("setup: expected view %v, got %v", tc.view, m.view)
			}

			_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
			if cmd == nil {
				t.Fatal("ctrl+c returned no command")
			}
			if _, ok := cmd().(tea.QuitMsg); !ok {
				t.Fatalf("expected tea.QuitMsg from ctrl+c, got %T", cmd())
			}
			if m.view != tc.view {
				t.Fatalf("ctrl+c popped the view (now %v) instead of quitting", m.view)
			}
		})
	}
}

// TestQuitKeyKeepsContextSensitiveBehaviour pins the half of #32 that must not
// change: q still pops in a nested view and quits on the dashboard.
func TestQuitKeyKeepsContextSensitiveBehaviour(t *testing.T) {
	database := openTUITestDB(t)
	m := New(database, t.TempDir(), nil)

	m.pushView(ProjectsView, nil)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if m.view != DashboardView {
		t.Fatalf("q in a nested view should pop, view is %v", m.view)
	}
	if cmd != nil {
		if _, ok := cmd().(tea.QuitMsg); ok {
			t.Fatal("q in a nested view should not quit")
		}
	}

	_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if cmd == nil {
		t.Fatal("q on the dashboard returned no command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("q on the dashboard should quit, got %T", cmd())
	}
}
