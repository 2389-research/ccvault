// ABOUTME: Tests for TUI key routing — printable runes belong to a focused text input.
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
		// The dashboard is the one view where q really does quit.
		{"dashboard", renderDashboard, []string{"q: quit"}, []string{"esc/q: back"}},
		// While the query input holds focus, q is a character — so the footer
		// must not offer it as back.
		{"search typing", renderSearchTyping, []string{"esc: back", "ctrl+c: quit"}, []string{"esc/q: back"}},
		{"search results", renderSearchResults, []string{"esc/q: back", "ctrl+c: quit"}, nil},
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
