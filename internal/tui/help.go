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

// helpSection is a titled group of bindings.
type helpSection struct {
	title    string
	bindings []helpBinding
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

// helpView renders the overlay for one view. Per-view rather than one long
// list so it fits a short terminal, and so the keys on offer are the keys that
// actually do something where the user is standing.
func helpView(view View) string {
	section, ok := viewHelp[view]
	if !ok {
		section = helpSection{title: "This view"}
	}

	// titleStyle and helpStyle carry their own vertical margins, so the only
	// explicit blank line here is the one between the two sections.
	var b strings.Builder
	b.WriteString(titleStyle.Render("Keyboard help — " + section.title))
	b.WriteString("\n")

	for i, s := range []helpSection{globalHelp, section} {
		if len(s.bindings) == 0 {
			continue
		}
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(helpSectionStyle.Render(s.title))
		b.WriteString("\n")
		b.WriteString(renderHelpBindings(s.bindings))
	}

	b.WriteString(helpStyle.Render("Press any key to close."))
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
