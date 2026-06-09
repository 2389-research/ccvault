// ABOUTME: Analytics view for ccvault TUI
// ABOUTME: Shows DuckDB-powered usage statistics and trends

package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/2389-research/ccvault/internal/analytics"
	"github.com/2389-research/ccvault/internal/db"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// AnalyticsModel holds analytics view state
type AnalyticsModel struct {
	db          *db.DB
	cacheDir    string
	width       int
	height      int
	loading     bool
	err         error
	dailyTokens []analytics.DailyTokens
	topProjects []analytics.ProjectStats
	modelStats  []analytics.ModelStats
	summary     *analytics.Summary
	selectedTab int
	viewport    viewport.Model
	ready       bool
}

const (
	tabSummary = iota
	tabDaily
	tabProjects
	tabModels
)

var tabNames = []string{"Summary", "Daily Usage", "Top Projects", "By Model"}

// NewAnalyticsModel creates a new analytics model
func NewAnalyticsModel(database *db.DB, cacheDir string) *AnalyticsModel {
	return &AnalyticsModel{
		db:       database,
		cacheDir: cacheDir,
		loading:  true,
	}
}

// Init loads analytics data
func (m *AnalyticsModel) Init() tea.Cmd {
	return m.loadAnalytics
}

type analyticsLoadedMsg struct {
	dailyTokens []analytics.DailyTokens
	topProjects []analytics.ProjectStats
	modelStats  []analytics.ModelStats
	summary     *analytics.Summary
	err         error
}

func (m *AnalyticsModel) loadAnalytics() tea.Msg {
	// Auto-build cache if parquet file is missing or predates multi-source support
	sessionsPath := filepath.Join(m.cacheDir, "sessions.parquet")
	needsRebuild := false
	if _, err := os.Stat(sessionsPath); os.IsNotExist(err) {
		needsRebuild = true
	}

	analyzer, err := analytics.NewAnalyzer(m.cacheDir)
	if err != nil {
		return analyticsLoadedMsg{err: err}
	}
	defer func() { _ = analyzer.Close() }()

	if !needsRebuild {
		hasSource, err := analyzer.SessionsParquetHasSource()
		if err != nil {
			return analyticsLoadedMsg{err: fmt.Errorf("check analytics cache schema: %w", err)}
		}
		if !hasSource {
			needsRebuild = true
		}
	}

	if needsRebuild {
		exporter := analytics.NewExporter(m.db, m.cacheDir)
		if err := exporter.Export(); err != nil {
			return analyticsLoadedMsg{err: fmt.Errorf("build analytics cache: %w", err)}
		}
	}

	// Load all analytics data
	summary, err := analyzer.GetSummary()
	if err != nil {
		return analyticsLoadedMsg{err: fmt.Errorf("get summary: %w", err)}
	}

	dailyTokens, err := analyzer.GetTokensByDay(90)
	if err != nil {
		return analyticsLoadedMsg{err: fmt.Errorf("get daily tokens: %w", err)}
	}

	topProjects, err := analyzer.GetTopProjects(20)
	if err != nil {
		return analyticsLoadedMsg{err: fmt.Errorf("get top projects: %w", err)}
	}

	modelStats, err := analyzer.GetTokensByModel()
	if err != nil {
		return analyticsLoadedMsg{err: fmt.Errorf("get model stats: %w", err)}
	}

	return analyticsLoadedMsg{
		dailyTokens: dailyTokens,
		topProjects: topProjects,
		modelStats:  modelStats,
		summary:     summary,
	}
}

// Update handles analytics view events
func (m *AnalyticsModel) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case analyticsLoadedMsg:
		m.loading = false
		if msg.err != nil {
			m.err = msg.err
			return nil
		}
		m.dailyTokens = msg.dailyTokens
		m.topProjects = msg.topProjects
		m.modelStats = msg.modelStats
		m.summary = msg.summary
		m.updateViewport()
		return nil

	case tea.KeyMsg:
		oldTab := m.selectedTab
		switch {
		case key.Matches(msg, keys.Left):
			if m.selectedTab > 0 {
				m.selectedTab--
			}
		case key.Matches(msg, keys.Right):
			if m.selectedTab < len(tabNames)-1 {
				m.selectedTab++
			}
		case key.Matches(msg, keys.Up):
			m.viewport.ScrollUp(1)
		case key.Matches(msg, keys.Down):
			m.viewport.ScrollDown(1)
		case key.Matches(msg, keys.PageUp):
			m.viewport.HalfPageUp()
		case key.Matches(msg, keys.PageDown):
			m.viewport.HalfPageDown()
		case key.Matches(msg, keys.Refresh):
			m.loading = true
			m.err = nil
			return m.loadAnalytics
		case msg.String() == "1":
			m.selectedTab = tabSummary
		case msg.String() == "2":
			m.selectedTab = tabDaily
		case msg.String() == "3":
			m.selectedTab = tabProjects
		case msg.String() == "4":
			m.selectedTab = tabModels
		}
		// Update viewport content when tab changes
		if oldTab != m.selectedTab {
			m.updateViewport()
		}

	case tea.MouseMsg:
		if m.ready {
			switch msg.Button {
			case tea.MouseButtonWheelUp:
				m.viewport.ScrollUp(3)
			case tea.MouseButtonWheelDown:
				m.viewport.ScrollDown(3)
			}
		}
	}
	return nil
}

func (m *AnalyticsModel) updateViewport() {
	if !m.ready {
		return
	}
	var content string
	switch m.selectedTab {
	case tabSummary:
		content = m.renderSummary()
	case tabDaily:
		content = m.renderDailyUsage()
	case tabProjects:
		content = m.renderTopProjects()
	case tabModels:
		content = m.renderModelStats()
	}
	m.viewport.SetContent(content)
	m.viewport.GotoTop()
}

// SetSize sets the viewport size
func (m *AnalyticsModel) SetSize(width, height int) {
	m.width = width
	m.height = height

	// Header (title + tabs) takes ~5 lines, footer takes ~2 lines
	headerHeight := 5
	footerHeight := 2
	viewportHeight := height - headerHeight - footerHeight
	if viewportHeight < 5 {
		viewportHeight = 5
	}

	m.viewport = viewport.New(width, viewportHeight)
	m.ready = true
	m.updateViewport()
}

// View renders the analytics view
func (m *AnalyticsModel) View() string {
	if m.loading {
		return titleStyle.Render("Loading analytics...")
	}

	if m.err != nil {
		var b strings.Builder
		b.WriteString(titleStyle.Render("Analytics"))
		b.WriteString("\n\n")
		b.WriteString(errorStyle.Render(fmt.Sprintf("Error: %v", m.err)))
		b.WriteString("\n\n")
		b.WriteString(helpStyle.Render("r: retry • esc: back"))
		return b.String()
	}

	var b strings.Builder

	// Title
	b.WriteString(titleStyle.Render("Analytics"))
	b.WriteString("\n\n")

	// Tabs
	b.WriteString(m.renderTabs())
	b.WriteString("\n\n")

	// Viewport content (scrollable)
	if m.ready {
		b.WriteString(m.viewport.View())
	}

	b.WriteString("\n")
	scrollPercent := 0
	if m.ready {
		scrollPercent = int(m.viewport.ScrollPercent() * 100)
	}
	b.WriteString(helpStyle.Render(fmt.Sprintf("←/→ or 1-4: tabs • ↑/↓: scroll • %d%% • r: refresh • esc: back", scrollPercent)))

	return b.String()
}

func (m *AnalyticsModel) renderTabs() string {
	var tabs []string
	for i, name := range tabNames {
		if i == m.selectedTab {
			tabs = append(tabs, selectedStyle.Render(fmt.Sprintf("[%d] %s", i+1, name)))
		} else {
			tabs = append(tabs, dimStyle.Render(fmt.Sprintf(" %d  %s", i+1, name)))
		}
	}
	return strings.Join(tabs, "  ")
}

func (m *AnalyticsModel) renderSummary() string {
	if m.summary == nil {
		return dimStyle.Render("No data available")
	}

	var lines []string

	lines = append(lines, fmt.Sprintf("%s %s",
		statLabelStyle.Render("Total Sessions:"),
		statValueStyle.Render(fmt.Sprintf("%d", m.summary.TotalSessions))))

	lines = append(lines, fmt.Sprintf("%s %s",
		statLabelStyle.Render("Total Tokens:"),
		formatTokens(m.summary.TotalTokens)))

	lines = append(lines, fmt.Sprintf("%s %s",
		statLabelStyle.Render("Unique Models:"),
		statValueStyle.Render(fmt.Sprintf("%d", m.summary.UniqueModels))))

	if !m.summary.FirstSession.IsZero() {
		lines = append(lines, fmt.Sprintf("%s %s",
			statLabelStyle.Render("First Session:"),
			statValueStyle.Render(m.summary.FirstSession.Format("Jan 2, 2006"))))
	}

	if !m.summary.LastSession.IsZero() {
		lines = append(lines, fmt.Sprintf("%s %s",
			statLabelStyle.Render("Last Session:"),
			statValueStyle.Render(m.summary.LastSession.Format("Jan 2, 2006"))))

		days := int(time.Since(m.summary.FirstSession).Hours() / 24)
		if days > 0 {
			avgPerDay := m.summary.TotalTokens / int64(days)
			lines = append(lines, fmt.Sprintf("%s %s",
				statLabelStyle.Render("Avg Tokens/Day:"),
				formatTokens(avgPerDay)))
		}
	}

	return boxStyle.Render(strings.Join(lines, "\n"))
}

func (m *AnalyticsModel) renderDailyUsage() string {
	if len(m.dailyTokens) == 0 {
		return dimStyle.Render("No daily data available")
	}

	var lines []string

	// Header
	header := fmt.Sprintf("%-12s %12s %12s %12s %8s",
		"Date", "Input", "Output", "Total", "Sessions")
	lines = append(lines, lipgloss.NewStyle().Bold(true).Render(header))
	lines = append(lines, strings.Repeat("─", 60))

	// Find max for sparkline
	var maxTokens int64
	for _, d := range m.dailyTokens {
		if d.TotalTokens > maxTokens {
			maxTokens = d.TotalTokens
		}
	}

	// Data rows (most recent first)
	for _, d := range m.dailyTokens {
		bar := renderBar(d.TotalTokens, maxTokens, 15)
		row := fmt.Sprintf("%-12s %12s %12s %12s %8d %s",
			d.Date.Format("Jan 02"),
			formatCompact(d.InputTokens),
			formatCompact(d.OutputTokens),
			formatCompact(d.TotalTokens),
			d.SessionCount,
			bar)
		lines = append(lines, row)
	}

	return strings.Join(lines, "\n")
}

func (m *AnalyticsModel) renderTopProjects() string {
	if len(m.topProjects) == 0 {
		return dimStyle.Render("No project data available")
	}

	var lines []string

	// Header
	header := fmt.Sprintf("%4s %-38s %8s %12s %12s",
		"#", "Project", "Sessions", "Tokens", "Last Active")
	lines = append(lines, lipgloss.NewStyle().Bold(true).Render(header))
	lines = append(lines, strings.Repeat("─", 80))

	// Find max for bar
	var maxTokens int64
	for _, p := range m.topProjects {
		if p.TotalTokens > maxTokens {
			maxTokens = p.TotalTokens
		}
	}

	for i, p := range m.topProjects {
		name := shortenPath(p.ProjectPath, 36)
		bar := renderBar(p.TotalTokens, maxTokens, 10)
		row := fmt.Sprintf("%4d %-38s %8d %12s %12s %s",
			i+1,
			name,
			p.SessionCount,
			formatCompact(p.TotalTokens),
			p.LastActive.Format("Jan 02"),
			bar)
		lines = append(lines, row)
	}

	return strings.Join(lines, "\n")
}

func (m *AnalyticsModel) renderModelStats() string {
	if len(m.modelStats) == 0 {
		return dimStyle.Render("No model data available")
	}

	var lines []string

	// Header
	header := fmt.Sprintf("%-35s %8s %15s",
		"Model", "Sessions", "Tokens")
	lines = append(lines, lipgloss.NewStyle().Bold(true).Render(header))
	lines = append(lines, strings.Repeat("─", 60))

	// Find max for bar
	var maxTokens int64
	for _, ms := range m.modelStats {
		if ms.TotalTokens > maxTokens {
			maxTokens = ms.TotalTokens
		}
	}

	for _, ms := range m.modelStats {
		name := shortenModelName(ms.Model, 33)
		bar := renderBar(ms.TotalTokens, maxTokens, 15)
		row := fmt.Sprintf("%-35s %8d %15s %s",
			name,
			ms.SessionCount,
			formatCompact(ms.TotalTokens),
			bar)
		lines = append(lines, row)
	}

	return strings.Join(lines, "\n")
}

// renderBar creates a simple ASCII bar chart
func renderBar(value, max int64, width int) string {
	if max == 0 {
		return strings.Repeat("░", width)
	}
	filled := int(float64(value) / float64(max) * float64(width))
	if filled > width {
		filled = width
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color("#10B981")).Render(
		strings.Repeat("█", filled) + strings.Repeat("░", width-filled))
}

// formatCompact formats numbers compactly (1.2K, 1.5M, etc.)
func formatCompact(n int64) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	if n < 1000000 {
		return fmt.Sprintf("%.1fK", float64(n)/1000)
	}
	if n < 1000000000 {
		return fmt.Sprintf("%.1fM", float64(n)/1000000)
	}
	return fmt.Sprintf("%.1fB", float64(n)/1000000000)
}

// shortenPath shortens a path for display
func shortenPath(path string, maxLen int) string {
	if maxLen < 4 {
		maxLen = 4 // Minimum to show "..."
	}
	if len(path) <= maxLen {
		return path
	}
	// Try to show the last meaningful parts
	parts := strings.Split(path, "/")
	result := path
	for i := 1; i < len(parts) && len(result) > maxLen; i++ {
		result = filepath.Join(parts[i:]...)
	}
	if len(result) > maxLen {
		// Ensure we don't get negative indices
		start := len(result) - maxLen + 3
		if start < 0 {
			start = 0
		}
		if start >= len(result) {
			return "..."
		}
		return "..." + result[start:]
	}
	return result
}

// shortenModelName shortens a model name for display
func shortenModelName(model string, maxLen int) string {
	if len(model) <= maxLen {
		return model
	}
	// Extract meaningful part (e.g., "opus-4" from "claude-opus-4-...")
	parts := strings.Split(model, "-")
	if len(parts) >= 3 {
		short := strings.Join(parts[1:3], "-")
		if len(short) <= maxLen {
			return short
		}
	}
	return model[:maxLen-3] + "..."
}
