// ABOUTME: Parametrized tests for TUI layout tier arithmetic.
// ABOUTME: Guards against tier overflows the earlier 80-col-only tests missed.

package tui

import (
	"testing"
)

// TestProjectsLayoutFitsWithinBudget verifies that at every plausible
// terminal width, the chosen projectsLayout's totalWidth fits within
// the terminal — catches the class of bug where a tier's column-sum
// arithmetic overflows its target width. The earlier
// TestProjectsModel_ViewFitsAt80Cols only tested one width.
func TestProjectsLayoutFitsWithinBudget(t *testing.T) {
	widths := []int{40, 55, 60, 70, 79, 80, 89, 90, 100, 104, 105, 110, 120, 150, 200}
	for _, w := range widths {
		layout := pickProjectsLayout(w)
		got := layout.totalWidth()
		if got > w && w >= 55 {
			// At <55 we don't guarantee fit — no reasonable terminal is
			// that narrow, and the default tier is a floor, not a ladder.
			t.Errorf("pickProjectsLayout(%d).totalWidth() = %d, exceeds terminal width", w, got)
		}
	}
}

// TestSessionsLayoutFitsWithinBudget mirrors the projects check for the
// sessions table, at both showProject=true (drilled-out view) and false
// (single-project view). The earlier layout had tier ≥100 showProject
// overflowing by 7 cols and tier ≥90 by 3 — this test would have caught
// both.
func TestSessionsLayoutFitsWithinBudget(t *testing.T) {
	widths := []int{55, 60, 70, 79, 80, 89, 90, 100, 109, 110, 120, 150, 200}
	for _, w := range widths {
		for _, showProject := range []bool{true, false} {
			layout := pickSessionsLayout(w, showProject)
			got := layout.totalWidth()
			if got > w && w >= 55 {
				t.Errorf("pickSessionsLayout(%d, showProject=%v).totalWidth() = %d, exceeds terminal width",
					w, showProject, got)
			}
		}
	}
}

// TestProjectsLayoutHeaderLabelsFit guards that every tier's column
// budgets accommodate the header labels rendered into them — otherwise
// padVisual returns the un-truncated label and the row misaligns.
func TestProjectsLayoutHeaderLabelsFit(t *testing.T) {
	widths := []int{40, 55, 60, 70, 80, 90, 100, 110, 150}
	// Minimum column widths to render header labels intact:
	// PROJECT=7, PATH=4, SOURCE=6, SESSIONS=8, TOKENS=6, ACTIVE=6.
	for _, w := range widths {
		l := pickProjectsLayout(w)
		if l.Project < 7 {
			t.Errorf("width %d: Project=%d < 7 (min for header 'PROJECT')", w, l.Project)
		}
		if l.Path < 4 {
			t.Errorf("width %d: Path=%d < 4 (min for header 'PATH')", w, l.Path)
		}
		if l.Source > 0 && l.Source < 6 {
			t.Errorf("width %d: Source=%d < 6 (min for header 'SOURCE')", w, l.Source)
		}
		if l.Sessions < 8 {
			t.Errorf("width %d: Sessions=%d < 8 (min for header 'SESSIONS')", w, l.Sessions)
		}
		if l.Tokens < 6 {
			t.Errorf("width %d: Tokens=%d < 6 (min for header 'TOKENS')", w, l.Tokens)
		}
		if l.LastActive < 6 {
			t.Errorf("width %d: LastActive=%d < 6 (min for header 'ACTIVE')", w, l.LastActive)
		}
	}
}
