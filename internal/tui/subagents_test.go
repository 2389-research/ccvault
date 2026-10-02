// ABOUTME: Tests the TUI's subagent defaults: parents only in the list, reachable from the detail view.
// ABOUTME: Same defaults as the CLI and MCP surfaces.

package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/2389-research/ccvault/internal/db"
	"github.com/2389-research/ccvault/pkg/models"
	tea "github.com/charmbracelet/bubbletea"
)

func subagentTestDB(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	seed := func(id, parentID string, offset time.Duration) {
		s := &models.Session{
			ID:              id,
			StartedAt:       time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC).Add(offset),
			SourceFile:      "/tmp/" + id + ".jsonl",
			Source:          "claude-code",
			ParentSessionID: parentID,
		}
		if err := database.UpsertSession(s); err != nil {
			t.Fatalf("upsert %s: %v", id, err)
		}
	}
	seed("parent-a", "", 0)
	seed("claude-code:parent-a:agent-a1", "parent-a", time.Minute)
	seed("claude-code:parent-a:agent-a2", "parent-a", 2*time.Minute)
	seed("parent-b", "", 3*time.Minute)

	return database
}

func TestSessionsModel_HidesSubagentsByDefault(t *testing.T) {
	database := subagentTestDB(t)

	m := NewSessionsModel(database)
	msg := m.loadSessions()
	loaded, ok := msg.(sessionsLoadedMsg)
	if !ok {
		t.Fatalf("loadSessions returned %T: %v", msg, msg)
	}
	if len(loaded.sessions) != 2 {
		t.Fatalf("loaded %d sessions, want 2 parents only", len(loaded.sessions))
	}
	for _, s := range loaded.sessions {
		if s.ParentSessionID != "" {
			t.Errorf("session %s is a subagent and should not be in the default list", s.ID)
		}
	}
}

// TestSessionsModel_ListShowsSubagentCount is the discoverability half of the
// default filtering. The list drops ~75% of rows on a real machine; a reader
// who cannot see that a session has 2 subagents has no way to know the `a`
// key would do anything. The count is what earns the filtering, so it is not
// optional — and it is spelled SUBS here to match `ccvault list-sessions`.
func TestSessionsModel_ListShowsSubagentCount(t *testing.T) {
	database := subagentTestDB(t)

	m := NewSessionsModel(database)
	m.SetSize(140, 40)
	loaded, ok := m.loadSessions().(sessionsLoadedMsg)
	if !ok {
		t.Fatal("loadSessions did not return sessionsLoadedMsg")
	}
	m.Update(loaded)

	view := m.View()
	if !strings.Contains(view, "SUBS") {
		t.Fatalf("sessions list has no SUBS header:\n%s", view)
	}

	// parent-a has two subagents, parent-b none. The rows are identified by
	// their distinct STARTED times.
	lines := strings.Split(view, "\n")
	var parentALine, parentBLine string
	for _, line := range lines {
		if strings.Contains(line, "12:00") {
			parentALine = line
		}
		if strings.Contains(line, "12:03") {
			parentBLine = line
		}
	}
	if parentALine == "" || parentBLine == "" {
		t.Fatalf("could not find the two parent rows:\n%s", view)
	}
	if !strings.Contains(parentALine, "2") {
		t.Errorf("parent-a's row does not report its 2 subagents:\n%s", parentALine)
	}
	if !strings.Contains(parentBLine, "-") {
		t.Errorf("parent-b's row does not render an empty subagent count:\n%s", parentBLine)
	}
}

// TestSessionsLayoutAlwaysBudgetsSubs: the count is non-negotiable, so no
// terminal width may drop the column the way SOURCE is dropped.
func TestSessionsLayoutAlwaysBudgetsSubs(t *testing.T) {
	for _, width := range []int{40, 55, 80, 90, 110, 200} {
		for _, showProject := range []bool{true, false} {
			layout := pickSessionsLayout(width, showProject)
			if layout.Subs <= 0 {
				t.Errorf("width %d showProject=%v dropped the SUBS column", width, showProject)
			}
		}
	}
}

func TestSessionsModel_ScopedToOneParent(t *testing.T) {
	database := subagentTestDB(t)

	m := NewSessionsModel(database)
	m.SetParentSession("parent-a")
	msg := m.loadSessions()
	loaded, ok := msg.(sessionsLoadedMsg)
	if !ok {
		t.Fatalf("loadSessions returned %T: %v", msg, msg)
	}
	if len(loaded.sessions) != 2 {
		t.Fatalf("loaded %d sessions, want parent-a's two subagents", len(loaded.sessions))
	}
	for _, s := range loaded.sessions {
		if s.ParentSessionID != "parent-a" {
			t.Errorf("session %s is not a child of parent-a", s.ID)
		}
	}

	m.sessions = loaded.sessions
	m.loading = false
	if view := m.View(); !strings.Contains(view, "Subagents") {
		t.Errorf("scoped sessions view does not say it is showing subagents:\n%s", view)
	}
}

// TestConversationModel_NavigatesToSubagents is the "expandable from the
// parent's detail view" half of the contract.
func TestConversationModel_NavigatesToSubagents(t *testing.T) {
	database := subagentTestDB(t)

	m := NewConversationModel(database)
	m.SetSession("parent-a")
	msg := m.loadConversation()
	loaded, ok := msg.(conversationLoadedMsg)
	if !ok {
		t.Fatalf("loadConversation returned %T: %v", msg, msg)
	}
	m.Update(loaded)
	m.SetSize(100, 40)

	if loaded.session.SubagentCount != 2 {
		t.Fatalf("session SubagentCount = %d, want 2", loaded.session.SubagentCount)
	}
	if view := m.View(); !strings.Contains(view, "2 subagents") {
		t.Errorf("detail view does not report the subagent count:\n%s", view)
	}

	cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	if cmd == nil {
		t.Fatal("pressing 'a' on a session with subagents produced no navigation")
	}
	nav, ok := cmd().(NavigateMsg)
	if !ok {
		t.Fatalf("expected NavigateMsg, got %T", cmd())
	}
	if nav.View != SessionsView {
		t.Errorf("navigated to view %v, want SessionsView", nav.View)
	}
	if nav.Data != SubagentsOf("parent-a") {
		t.Errorf("nav data = %#v, want SubagentsOf(parent-a)", nav.Data)
	}
}

// TestConversationModel_SubagentShowsItsParent is the way back up.
func TestConversationModel_SubagentShowsItsParent(t *testing.T) {
	database := subagentTestDB(t)

	m := NewConversationModel(database)
	m.SetSession("claude-code:parent-a:agent-a1")
	msg := m.loadConversation()
	loaded := msg.(conversationLoadedMsg)
	m.Update(loaded)
	m.SetSize(100, 40)

	if view := m.View(); !strings.Contains(view, "subagent of parent-a") {
		t.Errorf("a subagent's detail view does not name its parent:\n%s", view)
	}

	// Nothing to expand, so 'a' must not navigate anywhere.
	if cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}}); cmd != nil {
		t.Error("pressing 'a' on a session with no subagents navigated anyway")
	}
}
