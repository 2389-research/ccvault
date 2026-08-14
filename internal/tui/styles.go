// ABOUTME: Lip Gloss styles for the TUI
// ABOUTME: Defines consistent styling across all TUI views

package tui

import (
	"fmt"

	"github.com/charmbracelet/lipgloss"
)

var (
	// Colors
	primaryColor   = lipgloss.Color("#7C3AED") // Purple
	secondaryColor = lipgloss.Color("#10B981") // Green
	mutedColor     = lipgloss.Color("#6B7280") // Gray
	errorColor     = lipgloss.Color("#EF4444") // Red

	// Title styles
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(primaryColor).
			MarginBottom(1)

	subtitleStyle = lipgloss.NewStyle().
			Foreground(mutedColor).
			MarginBottom(1)

	// List styles
	selectedStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#FFFFFF")).
			Background(primaryColor).
			Padding(0, 1)

	normalStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#E5E7EB")).
			Padding(0, 1)

	// Stats styles
	statLabelStyle = lipgloss.NewStyle().
			Foreground(mutedColor).
			Width(15)

	statValueStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(secondaryColor)

	// Help styles
	helpStyle = lipgloss.NewStyle().
			Foreground(mutedColor).
			MarginTop(1)

	// Box styles
	boxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(mutedColor).
			Padding(1, 2)

	// Header styles. No fixed Width — the tabular views (Projects,
	// Sessions) render at whatever their layout tier assigns, and a
	// hard-coded 100-col width would force narrow terminals to wrap.
	headerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#F3F4F6")).
			Background(lipgloss.Color("#374151")).
			Padding(0, 1)

	// Content type styles
	userStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#60A5FA")) // Blue

	assistantStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#34D399")) // Green

	timestampStyle = lipgloss.NewStyle().
			Foreground(mutedColor)

	// Conversation content
	contentStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#E5E7EB")).
			PaddingLeft(2)

	// Error style
	errorStyle = lipgloss.NewStyle().
			Foreground(errorColor).
			Bold(true)

	// Tool styles
	toolStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#F59E0B")) // Amber

	thinkingStyle = lipgloss.NewStyle().
			Italic(true).
			Foreground(lipgloss.Color("#A78BFA")). // Light purple
			PaddingLeft(2)

	codeStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FCD34D")). // Yellow
			PaddingLeft(4)

	pathStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#67E8F9")). // Cyan
			PaddingLeft(4)

	dimStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#9CA3AF")). // Gray
			PaddingLeft(4)

	linkStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#60A5FA")). // Blue
			Underline(true).
			PaddingLeft(4)

	// Status message style
	secondaryStyle = lipgloss.NewStyle().
			Foreground(secondaryColor).
			Bold(true)

	// compactStyle marks a cell whose content has been shortened by
	// internal/compact — the row is telling the user "there's more
	// here you can't see at this width." Faint keeps the value fully
	// legible while signaling the abbreviation without stealing space
	// for an inline indicator character.
	compactStyle = lipgloss.NewStyle().
			Faint(true)
)

// formatTokens formats a token count for display
func formatTokens(n int64) string {
	if n >= 1_000_000_000 {
		return lipgloss.NewStyle().Render(
			statValueStyle.Render(fmt.Sprintf("%.1fB", float64(n)/1_000_000_000)),
		)
	}
	if n >= 1_000_000 {
		return statValueStyle.Render(fmt.Sprintf("%.1fM", float64(n)/1_000_000))
	}
	if n >= 1_000 {
		return statValueStyle.Render(fmt.Sprintf("%.1fK", float64(n)/1_000))
	}
	return statValueStyle.Render(fmt.Sprintf("%d", n))
}
