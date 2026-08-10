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
// padding (headerStyle + normalStyle both use Padding(0, 1)) plus 5
// single-space column separators. SESSIONS column stays ≥ 8 because
// that's the header label width. LAST ACTIVE / ACTIVE renders as
// "ACTIVE" (6 chars) at every tier to keep the header short.
//
// Prioritization at narrow widths: SOURCE shrinks first (most
// compressible + least identity-critical), PATH last (identity signal
// for same-basename projects). The PROJECT label stays at 18+ so
// typical basenames render without truncation.
func pickProjectsLayout(terminalWidth int) projectsLayout {
	switch {
	case terminalWidth >= 105:
		return projectsLayout{Project: 24, Path: 30, Source: 12, Sessions: 8, Tokens: 10, LastActive: 12}
	case terminalWidth >= 90:
		return projectsLayout{Project: 20, Path: 26, Source: 8, Sessions: 8, Tokens: 10, LastActive: 10}
	case terminalWidth >= 80:
		return projectsLayout{Project: 18, Path: 20, Source: 6, Sessions: 8, Tokens: 8, LastActive: 8}
	default:
		// Very narrow: drop SOURCE entirely, compress everything else.
		return projectsLayout{Project: 16, Path: 16, Source: 0, Sessions: 8, Tokens: 8, LastActive: 8}
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
	switch {
	case terminalWidth >= 100:
		if showProject {
			return sessionsLayout{Project: 22, Started: 20, Source: 12, Turns: 6, Tokens: 10, Model: 30}
		}
		return sessionsLayout{Project: 0, Started: 20, Source: 12, Turns: 6, Tokens: 10, Model: 30}
	case terminalWidth >= 90:
		if showProject {
			return sessionsLayout{Project: 20, Started: 20, Source: 8, Turns: 6, Tokens: 10, Model: 22}
		}
		return sessionsLayout{Project: 0, Started: 20, Source: 8, Turns: 6, Tokens: 10, Model: 22}
	case terminalWidth >= 80:
		if showProject {
			return sessionsLayout{Project: 18, Started: 16, Source: 6, Turns: 5, Tokens: 8, Model: 16}
		}
		return sessionsLayout{Project: 0, Started: 20, Source: 8, Turns: 6, Tokens: 10, Model: 22}
	default:
		if showProject {
			return sessionsLayout{Project: 16, Started: 14, Source: 0, Turns: 5, Tokens: 8, Model: 12}
		}
		return sessionsLayout{Project: 0, Started: 14, Source: 6, Turns: 5, Tokens: 8, Model: 14}
	}
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
