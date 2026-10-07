// ABOUTME: Tests that TUI startup never constructs a glamour markdown renderer.
// ABOUTME: Guards issue #49 — glamour's auto style probes the tty and can block for a minute.

package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour/styles"
)

// TestStartupNeverBuildsMarkdownRenderer covers issue #49. glamour's "auto"
// style asks termenv for the terminal's background colour, which writes an
// OSC 11 query to the tty and then waits up to termenv.OSCTimeout (5s) for
// *each byte* of the reply. Over mosh+tmux no reply need ever arrive, so
// building a renderer is a potential multi-second stall — and the old New()
// built one before the dashboard drew its first frame, then built another on
// the first WindowSizeMsg.
//
// The stall is only reproducible on that terminal. What this test pins is the
// structural property that removes it: nothing on the path from New() to the
// first dashboard frame constructs a renderer.
func TestStartupNeverBuildsMarkdownRenderer(t *testing.T) {
	database := openTUITestDB(t)
	_, _ = seedProjectAndSessions(t, database, []string{"claude-code"})

	m := New(database, t.TempDir(), nil)
	if m.conversation.mdRenderer != nil {
		t.Fatal("New() built a markdown renderer; it must wait until the conversation view opens")
	}

	// The first WindowSizeMsg arrives before the first frame, and SetSize used
	// to rebuild the renderer — a second probe, this one racing Bubble Tea's
	// own input reader for the same descriptor.
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if m.conversation.mdRenderer != nil {
		t.Fatal("the startup WindowSizeMsg built a markdown renderer")
	}

	m.dashboard.Update(m.dashboard.loadStats())
	if view := m.View(); strings.TrimSpace(view) == "" {
		t.Fatal("dashboard rendered nothing")
	}
	if m.conversation.mdRenderer != nil {
		t.Fatal("drawing the dashboard built a markdown renderer")
	}
}

// TestOpeningConversationBuildsRendererAndRendersTurns is the other half of
// the #49 fix: the renderer has to appear once it is genuinely needed, and the
// conversation must still render its turns through it.
func TestOpeningConversationBuildsRendererAndRendersTurns(t *testing.T) {
	database := openTUITestDB(t)
	_, sessionIDs := seedProjectAndSessions(t, database, []string{"claude-code"})

	m := New(database, t.TempDir(), nil)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.pushView(ConversationView, sessionIDs[0])
	m.conversation.Update(m.conversation.loadConversation())

	if m.conversation.mdRenderer == nil {
		t.Fatal("opening the conversation view did not build a markdown renderer")
	}
	view := stripANSI(m.View())
	for _, want := range []string{"[USER]", "hello from claude-code"} {
		if !strings.Contains(view, want) {
			t.Errorf("conversation view is missing %q:\n%s", want, view)
		}
	}
}

// TestMarkdownRendererReusedUntilWidthChanges guards the repeat-probe half of
// #49: SetSize built a fresh renderer on every call, so every resize paid the
// terminal query again.
func TestMarkdownRendererReusedUntilWidthChanges(t *testing.T) {
	database := openTUITestDB(t)
	m := NewConversationModel(database)
	m.SetSize(120, 40)

	if m.mdRenderer != nil {
		t.Fatal("SetSize built a renderer eagerly")
	}

	first := m.markdownRenderer()
	if first == nil {
		t.Fatal("markdownRenderer() returned nil")
	}
	if second := m.markdownRenderer(); second != first {
		t.Error("markdownRenderer() rebuilt the renderer for an unchanged width")
	}

	m.SetSize(100, 40)
	if m.mdRenderer != first {
		t.Error("SetSize replaced the renderer eagerly instead of leaving it to the next render")
	}
	if third := m.markdownRenderer(); third == first {
		t.Error("a width change should produce a renderer at the new wrap width")
	}
}

// TestMarkdownStyleNameNeverAsksTheTerminal pins the second half of the fix:
// the style is chosen from the environment, never by querying the tty. "auto"
// is the one value that makes termenv probe, so it must never be returned.
func TestMarkdownStyleNameNeverAsksTheTerminal(t *testing.T) {
	t.Setenv("GLAMOUR_STYLE", "")
	got := markdownStyleName()
	if got == styles.AutoStyle {
		t.Error(`markdownStyleName() returned "auto", which makes termenv probe the terminal`)
	}
	if got != styles.DarkStyle {
		t.Errorf("markdownStyleName() = %q with no GLAMOUR_STYLE, want %q", got, styles.DarkStyle)
	}

	t.Setenv("GLAMOUR_STYLE", styles.LightStyle)
	if got := markdownStyleName(); got != styles.LightStyle {
		t.Errorf("markdownStyleName() = %q, want %q from GLAMOUR_STYLE", got, styles.LightStyle)
	}
}
