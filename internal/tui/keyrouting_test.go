// ABOUTME: Tests for TUI key routing — printable runes, editing keys, and the help overlay.
// ABOUTME: Also pins every view's footer help text to the bindings that view actually honours.

package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/2389-research/ccvault/internal/search"
	"github.com/2389-research/ccvault/pkg/models"
)

// typeText feeds a string to the model one key event at a time, the way a
// terminal delivers typing. Space arrives as tea.KeySpace, not KeyRunes, so
// the helper mirrors that rather than inventing a shape Bubble Tea never sends.
func typeText(m *Model, text string) {
	for _, r := range text {
		msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
		if r == ' ' {
			msg.Type = tea.KeySpace
		}
		m.Update(msg)
	}
}

// TestSearchInputReceivesPrintableRunes covers the first half of issue #50:
// the global key handler claimed q (quit/back) and / (search), so no query
// containing either character could be typed.
func TestSearchInputReceivesPrintableRunes(t *testing.T) {
	database := openTUITestDB(t)
	m := New(database, t.TempDir(), nil)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})

	m.pushView(SearchView, nil)
	if !m.search.InputFocused() {
		t.Fatal("setup: search input should be focused on entry")
	}

	const query = "sqlite query /tmp"
	typeText(m, query)

	if m.view != SearchView {
		t.Fatalf("typing popped the view: now %v, want SearchView", m.view)
	}
	if got := m.search.input.Value(); got != query {
		t.Fatalf("search input value = %q, want %q", got, query)
	}
}

// TestQuitKeyPopsSearchWhenInputUnfocused is the other side of the same fix:
// once focus moves to the results list, q is navigation again.
func TestQuitKeyPopsSearchWhenInputUnfocused(t *testing.T) {
	database := openTUITestDB(t)
	m := New(database, t.TempDir(), nil)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.pushView(SearchView, nil)

	// A completed search auto-focuses the results list.
	m.search.Update(searchResultsMsg{results: []search.Result{
		{Turn: models.Turn{ID: "t1"}},
	}})
	if m.search.InputFocused() {
		t.Fatal("setup: results should hold focus after a search returns rows")
	}

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if m.view != DashboardView {
		t.Fatalf("q with results focused should pop to DashboardView, got %v", m.view)
	}
}

// TestSearchViewEscStillPops guards the navigation that must survive the rune
// fix: esc is not text, so it still leaves the view even while typing.
func TestSearchViewEscStillPops(t *testing.T) {
	database := openTUITestDB(t)
	m := New(database, t.TempDir(), nil)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.pushView(SearchView, nil)
	typeText(m, "sq")

	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.view != DashboardView {
		t.Fatalf("esc in the search view should pop to DashboardView, got %v", m.view)
	}
}

// TestSearchInputOwnsCursorAndEditingKeys covers the second half of issue #59:
// home, end, ctrl+u and ctrl+d were claimed by the results-list switch and
// never forwarded to the query box, so editing a long query was impossible.
func TestSearchInputOwnsCursorAndEditingKeys(t *testing.T) {
	database := openTUITestDB(t)
	m := New(database, t.TempDir(), nil)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.pushView(SearchView, nil)
	typeText(m, "sqlite")

	if got := m.search.input.Position(); got != 6 {
		t.Fatalf("setup: cursor at %d after typing 6 runes, want 6", got)
	}

	m.Update(tea.KeyMsg{Type: tea.KeyHome})
	if got := m.search.input.Position(); got != 0 {
		t.Errorf("home left the cursor at %d, want 0", got)
	}

	m.Update(tea.KeyMsg{Type: tea.KeyEnd})
	if got := m.search.input.Position(); got != 6 {
		t.Errorf("end left the cursor at %d, want 6", got)
	}

	// ctrl+d deletes forward, so park the cursor where there is something
	// ahead of it.
	m.Update(tea.KeyMsg{Type: tea.KeyHome})
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	if got := m.search.input.Value(); got != "qlite" {
		t.Errorf("ctrl+d gave %q, want %q", got, "qlite")
	}

	m.Update(tea.KeyMsg{Type: tea.KeyCtrlE})
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlU})
	if got := m.search.input.Value(); got != "" {
		t.Errorf("ctrl+u gave %q, want the line cleared", got)
	}

	if m.view != SearchView {
		t.Errorf("editing keys navigated away: view is %v, want SearchView", m.view)
	}
}

// TestSearchEscLeavesTheViewFromEitherFocus records the #59 decision on the
// unreachable esc-refocus branch: esc keeps the same meaning it has in every
// other view — back out — and the way back to the query box is "/", which the
// results footer advertises.
func TestSearchEscLeavesTheViewFromEitherFocus(t *testing.T) {
	database := openTUITestDB(t)

	for _, focusResults := range []bool{false, true} {
		m := New(database, t.TempDir(), nil)
		m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
		m.pushView(SearchView, nil)

		if focusResults {
			m.search.Update(searchResultsMsg{results: []search.Result{
				{Turn: models.Turn{ID: "t1"}},
			}})
			if m.search.InputFocused() {
				t.Fatal("setup: results should hold focus after a search returns rows")
			}
		}

		m.Update(tea.KeyMsg{Type: tea.KeyEsc})
		if m.view != DashboardView {
			t.Errorf("esc with focusResults=%v should pop to DashboardView, got %v",
				focusResults, m.view)
		}
	}
}

// TestSearchSlashReturnsFocusToQueryBox pins the documented way back from the
// results list to the query box (#59).
func TestSearchSlashReturnsFocusToQueryBox(t *testing.T) {
	database := openTUITestDB(t)
	m := New(database, t.TempDir(), nil)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.pushView(SearchView, nil)
	m.search.Update(searchResultsMsg{results: []search.Result{
		{Turn: models.Turn{ID: "t1"}},
	}})
	if m.search.InputFocused() {
		t.Fatal("setup: results should hold focus after a search returns rows")
	}

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	if m.view != SearchView {
		t.Fatalf("/ in the search view navigated away: now %v", m.view)
	}
	if !m.search.InputFocused() {
		t.Error("/ with results focused should return focus to the query box")
	}
	if got := m.search.input.Value(); got != "" {
		t.Errorf("/ was inserted as text: input value %q", got)
	}
}

// TestHelpOverlayOpensClosesAndIsModal covers issue #60: "?" was declared in
// the key map and handled by no Update at all.
func TestHelpOverlayOpensClosesAndIsModal(t *testing.T) {
	database := openTUITestDB(t)
	m := New(database, t.TempDir(), nil)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 40})
	m.dashboard.Update(m.dashboard.loadStats())

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	if !m.showHelp {
		t.Fatal("? did not open the help overlay")
	}
	overlay := stripANSI(m.View())
	for _, want := range []string{"Keyboard help", "ctrl+c", "?"} {
		if !strings.Contains(overlay, want) {
			t.Errorf("help overlay is missing %q:\n%s", want, overlay)
		}
	}

	// The overlay is modal: the key that dismisses it must not also act on the
	// view underneath. q on the dashboard would otherwise quit.
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if m.showHelp {
		t.Error("q did not close the help overlay")
	}
	if cmd != nil {
		if _, ok := cmd().(tea.QuitMsg); ok {
			t.Error("the key that closed the help overlay also quit")
		}
	}

	// ctrl+c is the one exception: it quits even with help open.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("ctrl+c with help open returned no command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Errorf("ctrl+c with help open should quit, got %T", cmd())
	}
}

// TestHelpOverlayIsPerViewAndFitsEightyColumns checks that every view has an
// overlay naming its own bindings, and that the overlay respects the same
// 80-column budget the footers do — the reason #60 wanted an overlay at all.
func TestHelpOverlayIsPerViewAndFitsEightyColumns(t *testing.T) {
	cases := []struct {
		view View
		want []string
	}{
		{DashboardView, []string{"Dashboard", "r"}},
		{ProjectsView, []string{"Projects", "r"}},
		{SessionsView, []string{"Sessions", "r"}},
		// x as an alias for e, and r, are the bindings #60 found undocumented.
		{ConversationView, []string{"Conversation", "e or x", "a", "r"}},
		{SearchView, []string{"Search", "shift+tab"}},
		{AnalyticsView, []string{"Analytics", "1-4"}},
		{SyncingView, []string{"Sync", "esc"}},
	}

	for _, tc := range cases {
		overlay := stripANSI(helpView(tc.view))
		for _, want := range tc.want {
			if !strings.Contains(overlay, want) {
				t.Errorf("help overlay for view %v is missing %q:\n%s", tc.view, want, overlay)
			}
		}
		if got := widestRowVisible(overlay); got > 80 {
			t.Errorf("help overlay for view %v is %d cols, want <= 80:\n%s", tc.view, got, overlay)
		}
	}
}

// TestHelpKeyIsTextInTheSearchInput keeps the #60 binding from re-breaking the
// #50 fix: "?" is a character while the query box has focus.
func TestHelpKeyIsTextInTheSearchInput(t *testing.T) {
	database := openTUITestDB(t)
	m := New(database, t.TempDir(), nil)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.pushView(SearchView, nil)

	typeText(m, "what?")
	if m.showHelp {
		t.Error("? opened the help overlay while the query box had focus")
	}
	if got := m.search.input.Value(); got != "what?" {
		t.Errorf("search input value = %q, want %q", got, "what?")
	}
}

// footerOf returns the last non-empty line of a rendered view with ANSI
// styling stripped — the help line.
func footerOf(t *testing.T, view string) string {
	t.Helper()
	lines := strings.Split(stripANSI(view), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			return strings.TrimSpace(lines[i])
		}
	}
	t.Fatalf("view rendered no non-empty lines:\n%s", view)
	return ""
}

// TestFootersMatchActualBindings covers the second half of issue #50. Every
// view below honours q as back and ctrl+c as quit (PR #48), except the
// dashboard where q quits outright and the search input where q is text.
func TestFootersMatchActualBindings(t *testing.T) {
	database := openTUITestDB(t)
	projectID, sessionIDs := seedProjectAndSessions(t, database, []string{"claude-code"})

	renderProjects := func() string {
		m := NewProjectsModel(database)
		m.Update(m.loadProjects())
		m.SetSize(80, 30)
		return m.View()
	}
	renderSessions := func() string {
		m := NewSessionsModel(database)
		m.SetProject(projectID)
		m.Update(m.loadSessions())
		m.SetSize(80, 30)
		return m.View()
	}
	renderConversation := func() string {
		m := NewConversationModel(database)
		m.SetSession(sessionIDs[0])
		m.SetSize(80, 30)
		m.Update(m.loadConversation())
		return m.View()
	}
	renderAnalytics := func() string {
		m := NewAnalyticsModel(database, t.TempDir())
		m.loading = false
		m.SetSize(80, 30)
		return m.View()
	}
	renderAnalyticsError := func() string {
		m := NewAnalyticsModel(database, t.TempDir())
		m.loading = false
		m.err = errors.New("analytics cache unavailable")
		m.SetSize(80, 30)
		return m.View()
	}
	renderSyncing := func() string {
		m := NewSyncingModel(database, nil)
		return m.View()
	}
	renderDashboard := func() string {
		m := NewDashboardModel(database)
		m.Update(m.loadStats())
		m.SetSize(80, 30)
		return m.View()
	}
	renderSearchTyping := func() string {
		m := NewSearchModel(database)
		m.SetSize(80, 30)
		return m.View()
	}
	renderSearchResults := func() string {
		m := NewSearchModel(database)
		m.SetSize(80, 30)
		m.Update(searchResultsMsg{results: []search.Result{
			{Turn: models.Turn{ID: "t1", Timestamp: time.Now()}},
		}})
		return m.View()
	}

	cases := []struct {
		name   string
		render func() string
		want   []string
		reject []string
	}{
		{"projects", renderProjects, []string{"esc/q: back", "ctrl+c: quit"}, []string{"• q: quit"}},
		{"sessions", renderSessions, []string{"esc/q: back", "ctrl+c: quit"}, nil},
		{"conversation", renderConversation, []string{"esc/q: back", "ctrl+c: quit"}, nil},
		{"analytics", renderAnalytics, []string{"esc/q: back", "ctrl+c: quit"}, nil},
		{"analytics error", renderAnalyticsError, []string{"esc/q: back", "ctrl+c: quit"}, nil},
		{"syncing", renderSyncing, []string{"esc/q: back", "ctrl+c: quit"}, nil},
		// The dashboard is the one view where q really does quit. It is also
		// the one footer with room to advertise the help overlay (#60).
		{"dashboard", renderDashboard, []string{"q: quit", "?: help"}, []string{"esc/q: back"}},
		// While the query input holds focus, q is a character — so the footer
		// must not offer it as back.
		{"search typing", renderSearchTyping, []string{"esc: back", "ctrl+c: quit"}, []string{"esc/q: back"}},
		// With results focused, "/" is the advertised way back to the query
		// box — the undocumented gap issue #59 reported.
		{"search results", renderSearchResults, []string{"/: query", "esc/q: back", "ctrl+c: quit"}, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			view := tc.render()
			footer := footerOf(t, view)
			for _, want := range tc.want {
				if !strings.Contains(footer, want) {
					t.Errorf("footer %q is missing %q", footer, want)
				}
			}
			for _, reject := range tc.reject {
				if strings.Contains(footer, reject) {
					t.Errorf("footer %q still advertises %q", footer, reject)
				}
			}
			if got := widestRowVisible(footer); got > 80 {
				t.Errorf("footer is %d cols, want ≤ 80: %q", got, footer)
			}
		})
	}
}
