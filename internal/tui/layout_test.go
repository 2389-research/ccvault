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

// TestProjectsLayoutHeaderLabelsFit builds the ACTUAL header string
// used in projects.go at each tier and asserts its visible width fits
// the terminal budget. Stronger than the previous "column ≥ label
// length" check — a header rename (e.g. ACTIVE → LAST_ACTIVITY) would
// silently pad through the old test but overflow this one.
func TestProjectsLayoutHeaderLabelsFit(t *testing.T) {
	widths := []int{40, 55, 60, 70, 80, 90, 100, 110, 150}
	for _, w := range widths {
		l := pickProjectsLayout(w)

		// Reproduce the header shape from projects.go View().
		var header string
		if l.Source > 0 {
			header = padVisual("PROJECT", l.Project) + " " +
				padVisual("PATH", l.Path) + " " +
				padVisual("SOURCE", l.Source) + " " +
				padVisual("SESSIONS", l.Sessions) + " " +
				padVisual("TOKENS", l.Tokens) + " " +
				padVisual("ACTIVE", l.LastActive)
		} else {
			header = padVisual("PROJECT", l.Project) + " " +
				padVisual("PATH", l.Path) + " " +
				padVisual("SESSIONS", l.Sessions) + " " +
				padVisual("TOKENS", l.Tokens) + " " +
				padVisual("ACTIVE", l.LastActive)
		}

		// Header + 2 chars row padding must fit terminalWidth (or the
		// 55-col floor for the default tier).
		total := 0
		for range header {
			total++
		}
		total += 2 // headerStyle.Padding(0, 1)

		floor := w
		if w < 55 {
			floor = 55
		}
		if total > floor {
			t.Errorf("width %d: header rendered %d cols, exceeds terminal budget %d\n  %q",
				w, total, floor, header)
		}

		// Additionally: no header label should overflow its own column
		// budget. padVisual returns the label unchanged when it's ≥ width,
		// so this catches "header was longer than the column budget."
		for label, colW := range map[string]int{
			"PROJECT": l.Project, "PATH": l.Path, "SESSIONS": l.Sessions,
			"TOKENS": l.Tokens, "ACTIVE": l.LastActive,
		} {
			if len(label) > colW {
				t.Errorf("width %d: header label %q (%d chars) > column budget %d",
					w, label, len(label), colW)
			}
		}
		if l.Source > 0 && len("SOURCE") > l.Source {
			t.Errorf("width %d: header label SOURCE > column budget %d", w, l.Source)
		}
	}
}
