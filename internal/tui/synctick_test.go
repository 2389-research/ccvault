// ABOUTME: Tests for issue #99 — the sync's tick chain must survive a view change.
// ABOUTME: Drives the real app Model with a real sync goroutine over synthetic sessions.

package tui

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// pumpCmds drives the app's message loop the way Bubble Tea would: it runs each
// command it is handed, feeds the resulting message back into Model.Update, and
// follows the chain of commands that come back until the chain runs dry, stop
// reports done, or the deadline passes.
//
// Running a tea.Tick command blocks for the tick's delay, so this is real time,
// not simulated time — which is the point: it is the same chain the program
// loop runs, and a broken chain shows up as an empty queue.
func pumpCmds(t *testing.T, m *Model, cmd tea.Cmd, within time.Duration, stop func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	queue := []tea.Cmd{cmd}

	for len(queue) > 0 && time.Now().Before(deadline) {
		if stop != nil && stop() {
			return
		}
		next := queue[0]
		queue = queue[1:]
		if next == nil {
			continue
		}
		msg := next()
		if msg == nil {
			continue
		}
		if batch, ok := msg.(tea.BatchMsg); ok {
			queue = append(queue, batch...)
			continue
		}
		_, out := m.Update(msg)
		queue = append(queue, out)
	}
}

// TestSyncTickSurvivesPushingTheSearchView covers issue #99: "/" pushes the
// search view on top of a running sync, and syncTickMsg used to be delegated to
// whichever view was focused. SearchModel.Update has no case for it, so the
// chain — which nothing but its own handler schedules — died there, and the sync
// goroutine blocked forever on a full progress channel holding the SQLite
// writer.
func TestSyncTickSurvivesPushingTheSearchView(t *testing.T) {
	m := newLeakTestModel(t)
	defer m.shutdownSync()

	startSyncAndStall(t, m)

	// Baseline: on the syncing view, a tick schedules the next one.
	if _, cmd := m.Update(syncTickMsg{}); cmd == nil {
		t.Fatal("setup: the tick chain was already dead on the syncing view")
	}

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	if m.view != SearchView {
		t.Fatalf("expected / to push SearchView, got %v", m.view)
	}

	if _, cmd := m.Update(syncTickMsg{}); cmd == nil {
		t.Fatal("the search view swallowed syncTickMsg: the sync's tick chain is dead and its goroutine will block on a full channel forever (#99)")
	}

	// "/" is a push, not a pop: unlike q and esc it must leave the sync alone,
	// so esc comes back to a sync that is still running.
	if m.syncing.ctx.Err() != nil {
		t.Error("/ cancelled the sync; only leaving the view is supposed to do that")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.view != SyncingView {
		t.Fatalf("esc should come back to SyncingView, got %v", m.view)
	}
	if _, cmd := m.Update(syncTickMsg{}); cmd == nil {
		t.Error("the tick chain did not survive the round trip through the search view")
	}
}

// TestSyncCompletesWhileTheSearchViewIsFocused is the behavioural half of #99:
// a sync interrupted by "/" has to run to completion, not freeze.
func TestSyncCompletesWhileTheSearchViewIsFocused(t *testing.T) {
	m := newLeakTestModel(t)
	defer m.shutdownSync()

	startSyncAndStall(t, m)

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	if m.view != SearchView {
		t.Fatalf("expected / to push SearchView, got %v", m.view)
	}

	pumpCmds(t, m, m.syncing.tick(), 30*time.Second, func() bool {
		return !syncGoroutineRunning()
	})

	if syncGoroutineRunning() {
		t.Fatal("the sync goroutine is still running after / pushed the search view: the sync froze (#99)")
	}
	if !m.syncing.IsDone() {
		t.Error("the sync goroutine finished but the model never saw the completion")
	}
}

// TestSyncCompletionDoesNotYankTheUserOutOfASearch pins the consequence of the
// #99 fix: with the tick chain alive, a sync started before the user pressed
// "/" can now finish while they are typing a query. The transition to the
// dashboard must not throw their search away — but the syncing view must stop
// being the thing esc goes back to, since it is finished.
func TestSyncCompletionDoesNotYankTheUserOutOfASearch(t *testing.T) {
	database := openTUITestDB(t)
	m := New(database, t.TempDir(), nil)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 40})

	m.Update(NavigateMsg{View: SyncingView})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	if m.view != SearchView {
		t.Fatalf("expected / to push SearchView, got %v", m.view)
	}
	typeText(m, "sqlite")

	m.Update(syncCompleteMsg{stats: nil, err: nil})
	m.Update(syncDoneTransitionMsg{})

	if m.view != SearchView {
		t.Fatalf("sync completion moved the user off their search to %v", m.view)
	}
	if got := m.search.input.Value(); got != "sqlite" {
		t.Errorf("sync completion lost the query: input value %q", got)
	}

	if _, cmd := m.popView(); cmd != nil {
		_ = cmd()
	}
	if m.view != DashboardView {
		t.Errorf("esc from the search view went back to %v, want the dashboard: the finished syncing view should no longer be on the stack", m.view)
	}
}
