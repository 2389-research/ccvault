// ABOUTME: Width-aware column-layout helpers shared across TUI models.
// ABOUTME: Picks a projects/sessions layout tier from terminal width and formats compact cells.

package tui

import (
	"strings"
	"unicode/utf8"

	"github.com/2389-research/ccvault/internal/compact"
)

// projectsLayout describes the column widths a Projects-shaped table
// should use at a given terminal width. Sessions has its own layout;
// dashboards and analytics reuse the projects one since they render the
// same PROJECT / PATH pair.
type projectsLayout struct {
	// project label column width (Class A label)
	Project int
	// path column width (tilde-abbreviated + smart-initialed under pressure)
	Path int
	// source label column; 0 means "drop the column entirely"
	Source int
	// numeric columns
	Sessions   int
	Tokens     int
	LastActive int
}

// pickProjectsLayout returns the column widths for a Projects-shaped
// table given the terminal width. Budgets account for 2 chars of row
// padding (headerStyle + normalStyle both use Padding(0, 1)) plus one
// space per column separator (5 for the 6-column layout, 4 when SOURCE
// is dropped). Sum-of-columns + separators + padding must be ≤
// terminalWidth at the LOWER bound of each tier — the parametrized
// TestProjectsLayoutFitsWithinBudget guards this arithmetic.
//
// Prioritization at narrow widths: SOURCE shrinks first (most
// compressible + least identity-critical), PATH last (identity signal
// for same-basename projects). PROJECT stays ≥ 14 so typical basenames
// render without truncation.
func pickProjectsLayout(terminalWidth int) projectsLayout {
	switch {
	case terminalWidth >= 105:
		// 24+30+12+8+10+12=96, +5 seps=101, +2 pad=103 ≤ 105 ✓
		return projectsLayout{Project: 24, Path: 30, Source: 12, Sessions: 8, Tokens: 10, LastActive: 12}
	case terminalWidth >= 90:
		// 20+26+8+8+10+10=82, +5=87, +2=89 ≤ 90 ✓
		return projectsLayout{Project: 20, Path: 26, Source: 8, Sessions: 8, Tokens: 10, LastActive: 10}
	case terminalWidth >= 80:
		// 18+20+6+8+8+8=68, +5=73, +2=75 ≤ 80 ✓
		return projectsLayout{Project: 18, Path: 20, Source: 6, Sessions: 8, Tokens: 8, LastActive: 8}
	default:
		// Very narrow (≤79): drop SOURCE. 14+13+8+7+6=48, +4=52, +2=54 ≤ 55.
		return projectsLayout{Project: 14, Path: 13, Source: 0, Sessions: 8, Tokens: 7, LastActive: 6}
	}
}

// sessionsLayout describes column widths for the Sessions list.
// Sessions has its own priority order: STARTED and SESSION are identity;
// SOURCE + MODEL shrink first under pressure.
type sessionsLayout struct {
	Project int // shown only when unfiltered
	Started int
	Source  int // 0 → drop
	Turns   int
	Tokens  int
	Model   int
}

func pickSessionsLayout(terminalWidth int, showProject bool) sessionsLayout {
	// showProject=true: 6 cols, 5 seps, +2 pad ≤ terminalWidth
	// showProject=false: 5 cols, 4 seps, +2 pad ≤ terminalWidth
	// Every tier verified by TestSessionsLayoutFitsWithinBudget.
	switch {
	case terminalWidth >= 110:
		if showProject {
			// 22+20+10+6+10+24=92, +5=97, +2=99 ≤ 110 ✓
			return sessionsLayout{Project: 22, Started: 20, Source: 10, Turns: 6, Tokens: 10, Model: 24}
		}
		// 20+12+6+10+30=78, +4=82, +2=84 ≤ 110 ✓
		return sessionsLayout{Project: 0, Started: 20, Source: 12, Turns: 6, Tokens: 10, Model: 30}
	case terminalWidth >= 90:
		if showProject {
			// 18+16+8+6+10+20=78, +5=83, +2=85 ≤ 90 ✓
			return sessionsLayout{Project: 18, Started: 16, Source: 8, Turns: 6, Tokens: 10, Model: 20}
		}
		// 20+8+6+10+24=68, +4=72, +2=74 ≤ 90 ✓
		return sessionsLayout{Project: 0, Started: 20, Source: 8, Turns: 6, Tokens: 10, Model: 24}
	case terminalWidth >= 80:
		if showProject {
			// 16+14+6+5+8+18=67, +5=72, +2=74 ≤ 80 ✓
			return sessionsLayout{Project: 16, Started: 14, Source: 6, Turns: 5, Tokens: 8, Model: 18}
		}
		// 20+8+6+10+20=64, +4=68, +2=70 ≤ 80 ✓
		return sessionsLayout{Project: 0, Started: 20, Source: 8, Turns: 6, Tokens: 10, Model: 20}
	default:
		if showProject {
			// 14+12+0+4+6+12=48, +5=53, +2=55 ≤ 55 ✓
			return sessionsLayout{Project: 14, Started: 12, Source: 0, Turns: 4, Tokens: 6, Model: 12}
		}
		// 14+6+4+6+14=44, +4=48, +2=50 ≤ 55 ✓
		return sessionsLayout{Project: 0, Started: 14, Source: 6, Turns: 4, Tokens: 6, Model: 14}
	}
}

// projectsBudget returns the total visible width a projects-layout row
// would occupy (sum of column widths + separators + row padding).
// Exposed for the parametrized layout-fit test.
func (l projectsLayout) totalWidth() int {
	cols := l.Project + l.Path + l.Sessions + l.Tokens + l.LastActive
	seps := 4 // between the 5 always-present columns
	if l.Source > 0 {
		cols += l.Source
		seps++ // extra separator for the extra column
	}
	return cols + seps + 2 // 2 = row padding (headerStyle/normalStyle Padding(0,1))
}

// totalWidth for sessionsLayout, same shape.
func (l sessionsLayout) totalWidth() int {
	cols := l.Started + l.Turns + l.Tokens + l.Model
	seps := 3 // between Started/Turns/Tokens/Model
	if l.Project > 0 {
		cols += l.Project
		seps++
	}
	if l.Source > 0 {
		cols += l.Source
		seps++
	}
	return cols + seps + 2
}

// padVisual left-pads s with spaces to visual column width. Uses rune
// count, not byte length — sprintf's %-Ns would over-count "…" (3 bytes
// but 1 visible col) and produce misaligned columns.
func padVisual(s string, width int) string {
	visW := utf8.RuneCountInString(s)
	if visW >= width {
		return s
	}
	return s + strings.Repeat(" ", width-visW)
}

// cellText returns Result.Text padded to width with visual-aware spacing,
// and wrapped in compactStyle when the value was shortened. Pad-first-
// then-wrap so ANSI escape codes don't confuse downstream layout width
// arithmetic.
func cellText(r compact.Result, width int) string {
	padded := padVisual(r.Text, width)
	if r.Shortened {
		return compactStyle.Render(padded)
	}
	return padded
}
