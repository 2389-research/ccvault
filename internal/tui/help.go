// ABOUTME: Help overlay listing the key bindings that apply to the current view.
// ABOUTME: Carries what the 80-column footers have no room to advertise inline.

package tui

import (
	"strings"
	"unicode/utf8"
)

// helpBinding is one row of the overlay: the keys, and what they do.
type helpBinding struct {
	keys string
	what string
}

// helpSection is a titled group of bindings, plus any facts about the view that
// are not keys at all — a view configured by the environment has nowhere else
// in the app to say so.
type helpSection struct {
	title    string
	bindings []helpBinding
	notes    []string
}

// globalHelp lists the bindings that work from every view.
//
// q, esc and / are listed unconditionally because the overlay can only be
// reached by a "?" that was not claimed as text — which means no text input
// holds focus, which is exactly when these three navigate.
var globalHelp = helpSection{
	title: "Anywhere",
	bindings: []helpBinding{
		{"?", "open or close this help"},
		{"ctrl+c", "quit, from any view"},
		{"q", "back one view (quit on the dashboard)"},
		{"esc", "back one view"},
		{"/", "search"},
	},
}

// viewHelp lists what each view handles on top of the global set, including
// the bindings the footers have no room for.
var viewHelp = map[View]helpSection{
	DashboardView: {
		title: "Dashboard",
		bindings: []helpBinding{
			{"↑/↓, k/j", "move the menu cursor"},
			{"enter", "open the selected item"},
			{"r", "reload the statistics"},
		},
	},
	ProjectsView: {
		title: "Projects",
		bindings: []helpBinding{
			{"↑/↓, k/j", "move the cursor"},
			{"pgup/pgdn", "page (also ctrl+u / ctrl+d)"},
			{"enter", "open the project's sessions"},
			{"r", "reload the project list"},
		},
	},
	SessionsView: {
		title: "Sessions",
		bindings: []helpBinding{
			{"↑/↓, k/j", "move the cursor"},
			{"pgup/pgdn", "page (also ctrl+u / ctrl+d)"},
			{"enter", "open the conversation"},
			{"r", "reload the session list"},
		},
	},
	ConversationView: {
		title: "Conversation",
		bindings: []helpBinding{
			{"↑/↓, k/j", "scroll one line"},
			{"pgup/pgdn", "scroll half a page (also ctrl+u / ctrl+d)"},
			{"wheel", "scroll"},
			{"a", "list this session's subagents"},
			{"c", "copy the transcript to the clipboard"},
			{"e or x", "export the transcript as Markdown"},
			{"r", "reload the conversation"},
		},
		// The only place in the app that says this. Markdown here is always
		// rendered dark, because detecting the terminal's real background meant
		// an OSC query that could hang for a minute (#49); GLAMOUR_STYLE is the
		// replacement and was otherwise discoverable only in the source (#102).
		// No token here is longer than "light-background": a 20-column
		// terminal has to be able to wrap the note without overflowing.
		notes: []string{
			"Markdown is rendered with glamour's dark colours. On a light-background " +
				"terminal, set GLAMOUR_STYLE to light in your shell.",
		},
	},
	SearchView: {
		title: "Search",
		bindings: []helpBinding{
			{"enter", "run the search, or open the selected result"},
			{"tab, ↓", "move from the query box to the results"},
			{"shift+tab, /", "back to the query box"},
			{"↑", "previous result, or the query box from the top row"},
			{"pgup/pgdn", "page through the results"},
			{"g, G", "first / last result"},
			{"home, end", "first / last result, or line start / end when typing"},
			{"ctrl+u", "clear the query up to the cursor"},
			{"ctrl+d", "delete the character under the cursor"},
		},
	},
	AnalyticsView: {
		title: "Analytics",
		bindings: []helpBinding{
			{"←/→, 1-4", "switch tab"},
			{"↑/↓, k/j", "scroll one line"},
			{"pgup/pgdn", "scroll half a page"},
			{"wheel", "scroll"},
			{"r", "rebuild the analytics cache"},
		},
	},
	SyncingView: {
		title: "Sync",
		bindings: []helpBinding{
			{"esc, q", "cancel the sync and go back"},
			{"any key", "continue to the dashboard, once the sync has finished"},
		},
	},
}

// helpIndent is the left margin each binding row sits at.
const helpIndent = "  "

// helpWrapIndent is the left margin a wrapped description sits at in the narrow
// layout, under the keys it belongs to.
const helpWrapIndent = "    "

// helpGutter is the gap between the two columns, in cells.
const helpGutter = 2

// twoColumnWidth reports how many cells the two-column table needs for a group
// of bindings: the indent, the widest key string, the gutter, and the longest
// description. Measured rather than hard-coded so that adding a long
// description narrows the overlay on its own instead of wrapping it.
func twoColumnWidth(bindings []helpBinding) int {
	keyWidth, whatWidth := 0, 0
	for _, bd := range bindings {
		if w := utf8.RuneCountInString(bd.keys); w > keyWidth {
			keyWidth = w
		}
		if w := utf8.RuneCountInString(bd.what); w > whatWidth {
			whatWidth = w
		}
	}
	if keyWidth == 0 {
		return 0
	}
	return utf8.RuneCountInString(helpIndent) + keyWidth + helpGutter + whatWidth
}

// helpView renders the overlay for one view, laid out for a terminal `width`
// cells wide. Per-view rather than one long list so it fits a short terminal,
// and so the keys on offer are the keys that actually do something where the
// user is standing.
//
// A width of zero means "unknown" — the app has not had a WindowSizeMsg yet —
// and gets the two-column layout, the same as a wide terminal.
func helpView(view View, width int) string {
	section, ok := viewHelp[view]
	if !ok {
		section = helpSection{title: "This view"}
	}
	// The two-column table is only worth keeping while it fits. Below that the
	// rows wrap mid-description, and the overlay is denser than a footer so it
	// wraps worse (#102) — the single-column layout takes over instead.
	needed := twoColumnWidth(globalHelp.bindings)
	if w := twoColumnWidth(section.bindings); w > needed {
		needed = w
	}
	narrow := width > 0 && width < needed

	// The heading drops the view name in the narrow layout: at 20 columns
	// "Keyboard help — Conversation" is a third wider than the terminal, and the
	// name is already the second section's own heading a few rows down.
	title := "Keyboard help — " + section.title
	closing := "Press any key to close."
	if narrow {
		title = "Keyboard help"
		closing = "any key: close"
	}

	// titleStyle and helpStyle carry their own vertical margins, so the only
	// explicit blank line here is the one between the two sections.
	var b strings.Builder
	b.WriteString(titleStyle.Render(title))
	b.WriteString("\n")

	for i, s := range []helpSection{globalHelp, section} {
		if len(s.bindings) == 0 && len(s.notes) == 0 {
			continue
		}
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(helpSectionStyle.Render(s.title))
		b.WriteString("\n")
		if narrow {
			b.WriteString(renderHelpBindingsNarrow(s.bindings, width))
		} else {
			b.WriteString(renderHelpBindings(s.bindings))
		}
		b.WriteString(renderHelpNotes(s.notes, width))
	}

	b.WriteString(helpStyle.Render(closing))
	return b.String()
}

// renderHelpBindings lays the rows out as a two-column table, sized to the
// widest key string in the group.
//
// The padding is computed from rune counts rather than byte lengths, and
// applied outside helpKeyStyle.Render: the key labels carry arrows, and a
// styled string carries ANSI bytes, so neither can be measured by length.
func renderHelpBindings(bindings []helpBinding) string {
	keyWidth := 0
	for _, bd := range bindings {
		if w := utf8.RuneCountInString(bd.keys); w > keyWidth {
			keyWidth = w
		}
	}

	var b strings.Builder
	for _, bd := range bindings {
		b.WriteString(helpIndent)
		b.WriteString(helpKeyStyle.Render(bd.keys))
		b.WriteString(strings.Repeat(" ", keyWidth-utf8.RuneCountInString(bd.keys)+2))
		b.WriteString(bd.what)
		b.WriteString("\n")
	}
	return b.String()
}

// renderHelpBindingsNarrow drops the second column: each binding's keys get a
// row of their own and the description follows underneath, indented and wrapped
// to the terminal. Twice the rows, but nothing runs off the right edge — the
// overlay is read top to bottom either way.
func renderHelpBindingsNarrow(bindings []helpBinding, width int) string {
	var b strings.Builder
	for _, bd := range bindings {
		b.WriteString(helpIndent)
		b.WriteString(helpKeyStyle.Render(bd.keys))
		b.WriteString("\n")
		for _, line := range wrapWords(bd.what, width-utf8.RuneCountInString(helpWrapIndent)) {
			b.WriteString(helpWrapIndent)
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// renderHelpNotes renders a section's notes as wrapped paragraphs, one blank
// line above them so they read as prose rather than as another binding row.
//
// Wrapped to the narrower of the terminal and the 80-column budget every
// footer is measured against — prose set across a 200-column terminal is a
// single unreadable line.
func renderHelpNotes(notes []string, width int) string {
	if len(notes) == 0 {
		return ""
	}
	if width <= 0 || width > 80 {
		width = 80
	}

	var b strings.Builder
	for _, note := range notes {
		b.WriteString("\n")
		for _, line := range wrapWords(note, width-utf8.RuneCountInString(helpIndent)) {
			b.WriteString(helpIndent)
			b.WriteString(helpNoteStyle.Render(line))
			b.WriteString("\n")
		}
	}
	return b.String()
}

// wrapWords breaks text into lines of at most width visible cells, splitting on
// spaces only. A word longer than width gets a line to itself and overflows:
// the alternative is hyphenating a key name like "ctrl+u" mid-token, which
// would be worse than a wrapped line.
//
// Measured in runes rather than bytes, since the descriptions carry arrows.
func wrapWords(text string, width int) []string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return nil
	}
	if width < 1 {
		return []string{strings.Join(words, " ")}
	}

	var lines []string
	line := words[0]
	for _, word := range words[1:] {
		if utf8.RuneCountInString(line)+1+utf8.RuneCountInString(word) <= width {
			line += " " + word
			continue
		}
		lines = append(lines, line)
		line = word
	}
	return append(lines, line)
}
