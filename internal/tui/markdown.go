// ABOUTME: Markdown style selection for the TUI's glamour renderers.
// ABOUTME: Resolves the style from the environment so nothing ever queries the terminal.

package tui

import (
	"os"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/styles"
)

// markdownStyleName picks the glamour style to render conversations with.
//
// The obvious choice, glamour.WithAutoStyle(), is the cause of issue #49: it
// resolves to the "auto" style, which asks termenv for the terminal's
// background colour. termenv writes an OSC 11 query to the tty and then waits
// up to OSCTimeout (5 seconds) for *each byte* of the answer, in a loop that
// keeps reading until it sees an ESC. A local terminal answers in
// microseconds; over mosh+tmux the answer need never arrive, and the query
// also races Bubble Tea's own reader for the same descriptor. The archive
// owner saw 60+ seconds of it before the dashboard drew a frame.
//
// So the style comes from the environment instead. GLAMOUR_STYLE is glamour's
// own convention — a builtin style name or a path to a style JSON file — and
// the default is dark, matching the fixed palette in styles.go. That costs a
// light-terminal user one environment variable and costs nobody a stall.
func markdownStyleName() string {
	if style := os.Getenv("GLAMOUR_STYLE"); style != "" {
		return style
	}
	return styles.DarkStyle
}

// markdownStyleOption is markdownStyleName as a renderer option. WithStylePath
// rather than WithStandardStyle so a GLAMOUR_STYLE pointing at a file works
// the same way it does in glamour's own WithEnvironmentConfig.
func markdownStyleOption() glamour.TermRendererOption {
	return glamour.WithStylePath(markdownStyleName())
}
