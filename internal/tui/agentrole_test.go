// ABOUTME: Tests the Analytics tab that splits top-level sessions from subagent ones (#150).
// ABOUTME: Asserts it leads with per-session shape, which is where the signal is.

package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/2389-research/ccvault/pkg/models"
	tea "github.com/charmbracelet/bubbletea"
)

// agentRoleModel seeds the two populations with the shapes the real archive
// shows in miniature — a small minority of subagent sessions that are much
// longer and much more expensive each — then loads the Analytics view over a
// freshly built cache.
func agentRoleModel(t *testing.T) *AnalyticsModel {
	t.Helper()

	database := openTUITestDB(t)
	seed := func(id, parentID string, turns int, inTok, outTok int64, offset time.Duration) {
		s := &models.Session{
			ID:              id,
			StartedAt:       time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC).Add(offset),
			EndedAt:         time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC).Add(offset),
			Model:           "claude-opus-4",
			SourceFile:      "/tmp/" + id + ".jsonl",
			Source:          "claude-code",
			ParentSessionID: parentID,
			TurnCount:       turns,
			InputTokens:     inTok,
			OutputTokens:    outTok,
		}
		if err := database.UpsertSession(s); err != nil {
			t.Fatalf("upsert %s: %v", id, err)
		}
	}
	for i, id := range []string{"parent-a", "parent-b", "parent-c", "parent-d"} {
		seed(id, "", 10, 600, 400, time.Duration(i)*time.Minute)
	}
	seed("claude-code:parent-a:agent-a1", "parent-a", 60, 4000, 2000, 5*time.Minute)

	m := NewAnalyticsModel(database, t.TempDir())
	loaded, ok := m.loadAnalytics().(analyticsLoadedMsg)
	if !ok {
		t.Fatal("loadAnalytics did not return analyticsLoadedMsg")
	}
	if loaded.err != nil {
		t.Fatalf("loadAnalytics err: %v", loaded.err)
	}
	m.SetSize(100, 40)
	m.Update(loaded)
	return m
}

// TestAnalyticsModel_SubagentTabLeadsWithPerSessionShape is #150's red test.
// The aggregate share of subagent spend is small — 0.8% of sessions and 7.9%
// of tokens on the author's archive — so a view built around totals would be
// built around nothing. What is large is the per-session difference, and that
// is what the tab has to say first.
func TestAnalyticsModel_SubagentTabLeadsWithPerSessionShape(t *testing.T) {
	m := agentRoleModel(t)

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'5'}})
	view := stripANSI(m.View())

	for _, want := range []string{"Subagents", "top-level", "subagent"} {
		if !strings.Contains(view, want) {
			t.Errorf("subagent tab is missing %q:\n%s", want, view)
		}
	}

	// Per-session averages: 10 turns / 1,000 tokens against 60 / 6,000.
	for _, want := range []string{"10.0", "60.0", "1.0K", "6.0K"} {
		if !strings.Contains(view, want) {
			t.Errorf("subagent tab does not report per-session figure %q:\n%s", want, view)
		}
	}

	// And the multiplier, which is the sentence a reader takes away.
	if !strings.Contains(view, "6.0x") {
		t.Errorf("subagent tab does not state how much larger a subagent session is:\n%s", view)
	}
}

// TestAnalyticsModel_SubagentTabStatesItsAggregateShare: the share belongs on
// the tab — just not at the top. Omitting it would let a reader infer from the
// averages that subagents dominate the archive, which they do not.
func TestAnalyticsModel_SubagentTabStatesItsAggregateShare(t *testing.T) {
	m := agentRoleModel(t)

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'5'}})
	view := stripANSI(m.View())

	// 1 of 5 sessions, 6,000 of 10,000 tokens.
	for _, want := range []string{"20.0%", "60.0%"} {
		if !strings.Contains(view, want) {
			t.Errorf("subagent tab does not report share %q:\n%s", want, view)
		}
	}
}

// TestAnalyticsModel_SubagentTabSaysWhatItCounts: token figures in this
// archive are only as complete as per-session usage accounting, so the tab
// states what is in the number rather than implying it is everything.
func TestAnalyticsModel_SubagentTabSaysWhatItCounts(t *testing.T) {
	m := agentRoleModel(t)

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'5'}})
	view := stripANSI(m.View())

	if !strings.Contains(view, "recorded") {
		t.Errorf("subagent tab does not say what its token figures count:\n%s", view)
	}
}

// TestAnalyticsModel_SubagentTabIsReachableByArrows: 5 is the shortcut, but
// the arrow keys walk the tab bar and must reach the new tab too.
func TestAnalyticsModel_SubagentTabIsReachableByArrows(t *testing.T) {
	m := agentRoleModel(t)

	for i := 0; i < len(tabNames); i++ {
		m.Update(tea.KeyMsg{Type: tea.KeyRight})
	}
	if m.selectedTab != tabSubagents {
		t.Fatalf("walking right to the end landed on tab %d, want %d", m.selectedTab, tabSubagents)
	}
	if view := stripANSI(m.View()); !strings.Contains(view, "top-level") {
		t.Errorf("rightmost tab does not render the split:\n%s", view)
	}
}

// TestFormatRatio_NoDenominator: Go divides by a float zero happily, so
// without the guard an archive whose top-level sessions record no turns would
// render "+Infx" at a reader.
func TestFormatRatio_NoDenominator(t *testing.T) {
	if got := formatRatio(60, 0); got != "—" {
		t.Errorf("formatRatio(60, 0) = %q, want an em dash", got)
	}
	if got := formatRatio(60, 10); got != "6.0x" {
		t.Errorf("formatRatio(60, 10) = %q, want 6.0x", got)
	}
}

// TestAnalyticsTabBarFitsEightyColumns is the cost of a fifth tab. The bar
// was 74 columns with four, and the unselected-tab style's PaddingLeft(4) was
// spending 16 of them, so the fifth had to come out of the padding rather
// than out of an 80-column terminal. Whoever adds a sixth tab finds out here.
func TestAnalyticsTabBarFitsEightyColumns(t *testing.T) {
	m := &AnalyticsModel{}
	for tab := range tabNames {
		m.selectedTab = tab
		if got := widestRowVisible(stripANSI(m.renderTabs())); got > 80 {
			t.Errorf("analytics tab bar with tab %d selected is %d cols, want <= 80",
				tab, got)
		}
	}
}

// TestAnalyticsModel_SubagentContentFitsEightyColumns: the tab's tables are
// fixed-width, so they either fit the narrowest terminal we support or they
// wrap into illegibility.
func TestAnalyticsModel_SubagentContentFitsEightyColumns(t *testing.T) {
	m := agentRoleModel(t)

	if got := widestRowVisible(stripANSI(m.renderSubagents())); got > 80 {
		t.Errorf("subagent tab content is %d cols, want <= 80:\n%s",
			got, stripANSI(m.renderSubagents()))
	}
}

// TestAnalyticsModel_FooterAdvertisesEveryTab — a tab nothing mentions is a
// tab nobody finds (#60's lesson about undocumented bindings).
func TestAnalyticsModel_FooterAdvertisesEveryTab(t *testing.T) {
	m := agentRoleModel(t)

	view := stripANSI(m.View())
	if !strings.Contains(view, "1-5") {
		t.Errorf("analytics footer does not advertise the fifth tab:\n%s", view)
	}

	overlay := stripANSI(helpView(AnalyticsView, 80))
	if !strings.Contains(overlay, "1-5") {
		t.Errorf("analytics help overlay does not advertise the fifth tab:\n%s", overlay)
	}
}
