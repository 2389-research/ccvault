// ABOUTME: Dashboard view for ccvault TUI
// ABOUTME: Shows summary statistics and navigation options

package tui

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/2389-research/ccvault/internal/db"
	"github.com/2389-research/ccvault/pkg/models"
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// DashboardModel holds dashboard state
type DashboardModel struct {
	db           *db.DB
	width        int
	height       int
	stats        *Stats
	topProjects  []models.Project
	selectedItem int
	loading      bool
}

// Stats holds dashboard statistics
type Stats struct {
	Projects      int
	Sessions      int
	Turns         int
	TotalTokens   int64
	TokensByModel map[string]int64
	FirstActivity time.Time
	LastActivity  time.Time
}

// Menu items
var dashboardItems = []string{
	"Browse Projects",
	"Recent Sessions",
	"Search",
	"Analytics",
	"Sync Conversations",
}

// NewDashboardModel creates a new dashboard model
func NewDashboardModel(database *db.DB) *DashboardModel {
	return &DashboardModel{
		db:      database,
		loading: true,
	}
}

// Init loads dashboard data
func (m *DashboardModel) Init() tea.Cmd {
	return m.loadStats
}

// loadStats fetches statistics from the database
func (m *DashboardModel) loadStats() tea.Msg {
	projectCount, _, err := m.db.GetProjectStats()
	if err != nil {
		return ErrorMsg{Err: err}
	}

	sessionCount, turnCount, totalTokens, err := m.db.GetSessionStats()
	if err != nil {
		return ErrorMsg{Err: err}
	}

	projects, err := m.db.GetProjects("activity", 5)
	if err != nil {
		return ErrorMsg{Err: err}
	}

	tokensByModel, err := m.db.GetTokensByModel()
	if err != nil {
		return ErrorMsg{Err: err}
	}

	firstActivity, lastActivity, err := m.db.GetFirstAndLastActivity()
	if err != nil {
		// Non-fatal, just use zero times
		firstActivity = time.Time{}
		lastActivity = time.Time{}
	}

	return statsLoadedMsg{
		stats: &Stats{
			Projects:      projectCount,
			Sessions:      sessionCount,
			Turns:         turnCount,
			TotalTokens:   totalTokens,
			TokensByModel: tokensByModel,
			FirstActivity: firstActivity,
			LastActivity:  lastActivity,
		},
		topProjects: projects,
	}
}

type statsLoadedMsg struct {
	stats       *Stats
	topProjects []models.Project
}

// Update handles dashboard events
func (m *DashboardModel) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case statsLoadedMsg:
		m.stats = msg.stats
		m.topProjects = msg.topProjects
		m.loading = false
		return nil

	case tea.KeyMsg:
		switch {
		case key.Matches(msg, keys.Up):
			if m.selectedItem > 0 {
				m.selectedItem--
			}
		case key.Matches(msg, keys.Down):
			if m.selectedItem < len(dashboardItems)-1 {
				m.selectedItem++
			}
		case key.Matches(msg, keys.Enter):
			return m.selectItem()
		case key.Matches(msg, keys.Refresh):
			m.loading = true
			return m.loadStats
		}
	}
	return nil
}

// selectItem handles menu selection
func (m *DashboardModel) selectItem() tea.Cmd {
	switch m.selectedItem {
	case 0: // Browse Projects
		return func() tea.Msg {
			return NavigateMsg{View: ProjectsView}
		}
	case 1: // Recent Sessions
		return func() tea.Msg {
			return NavigateMsg{View: SessionsView}
		}
	case 2: // Search
		return func() tea.Msg {
			return NavigateMsg{View: SearchView}
		}
	case 3: // Analytics
		return func() tea.Msg {
			return NavigateMsg{View: AnalyticsView}
		}
	case 4: // Sync Conversations
		return func() tea.Msg {
			return NavigateMsg{View: SyncingView}
		}
	}
	return nil
}

// SetSize sets the viewport size
func (m *DashboardModel) SetSize(width, height int) {
	m.width = width
	m.height = height
}

// View renders the dashboard
func (m *DashboardModel) View() string {
	if m.loading {
		return titleStyle.Render("Loading...")
	}

	var b strings.Builder

	// Title
	b.WriteString(titleStyle.Render("Claude Code Conversation Archive"))
	b.WriteString("\n\n")

	// Stats box
	if m.stats != nil {
		statsBox := m.renderStats()
		b.WriteString(statsBox)
		b.WriteString("\n\n")
	}

	// Menu
	b.WriteString(lipgloss.NewStyle().Bold(true).Render("Navigation"))
	b.WriteString("\n")
	for i, item := range dashboardItems {
		if i == m.selectedItem {
			b.WriteString(selectedStyle.Render("▶ " + item))
		} else {
			b.WriteString(normalStyle.Render("  " + item))
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")

	// Top projects
	if len(m.topProjects) > 0 {
		home, _ := os.UserHomeDir()
		b.WriteString(lipgloss.NewStyle().Bold(true).Render("Recent Projects"))
		b.WriteString("\n")
		for _, p := range m.topProjects {
			name := p.DisplayName
			if len(name) > 22 {
				name = "..." + name[len(name)-19:]
			}
			path := p.Path
			if home != "" && strings.HasPrefix(path, home) {
				path = "~" + path[len(home):]
			}
			if len(path) > 35 {
				path = "..." + path[len(path)-32:]
			}
			b.WriteString(normalStyle.Render(fmt.Sprintf("  %-24s %-37s %3d sessions", name, path, p.SessionCount)))
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	// Help
	b.WriteString(helpStyle.Render("↑/↓: navigate • enter: select • r: refresh • q: quit"))

	return b.String()
}

// renderStats creates the statistics display
func (m *DashboardModel) renderStats() string {
	var lines []string

	// Basic stats
	lines = append(lines, fmt.Sprintf("%s %s",
		statLabelStyle.Render("Projects:"),
		statValueStyle.Render(fmt.Sprintf("%d", m.stats.Projects))))

	lines = append(lines, fmt.Sprintf("%s %s",
		statLabelStyle.Render("Sessions:"),
		statValueStyle.Render(fmt.Sprintf("%d", m.stats.Sessions))))

	lines = append(lines, fmt.Sprintf("%s %s",
		statLabelStyle.Render("Turns:"),
		statValueStyle.Render(fmt.Sprintf("%d", m.stats.Turns))))

	lines = append(lines, fmt.Sprintf("%s %s",
		statLabelStyle.Render("Total Tokens:"),
		formatTokens(m.stats.TotalTokens)))

	// Date span
	if !m.stats.FirstActivity.IsZero() && !m.stats.LastActivity.IsZero() {
		dateSpan := fmt.Sprintf("%s to %s",
			m.stats.FirstActivity.Format("Jan 2, 2006"),
			m.stats.LastActivity.Format("Jan 2, 2006"))
		days := int(m.stats.LastActivity.Sub(m.stats.FirstActivity).Hours() / 24)
		if days > 0 {
			dateSpan += fmt.Sprintf(" (%d days)", days)
		}
		lines = append(lines, fmt.Sprintf("%s %s",
			statLabelStyle.Render("Date Span:"),
			statValueStyle.Render(dateSpan)))
	}

	// Models used with tokens
	if len(m.stats.TokensByModel) > 0 {
		lines = append(lines, "")
		lines = append(lines, lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#E5E7EB")).Render("Models Used:"))

		// Sort models by token count (descending)
		type modelTokens struct {
			model  string
			tokens int64
		}
		var sorted []modelTokens
		for model, tokens := range m.stats.TokensByModel {
			sorted = append(sorted, modelTokens{model, tokens})
		}
		sort.Slice(sorted, func(i, j int) bool {
			return sorted[i].tokens > sorted[j].tokens
		})

		for _, mt := range sorted {
			// Shorten model name for display
			modelName := mt.model
			if len(modelName) > 30 {
				// Extract meaningful part (e.g., "opus-4" from "claude-opus-4-...")
				parts := strings.Split(modelName, "-")
				if len(parts) >= 3 {
					modelName = strings.Join(parts[1:3], "-")
				} else {
					modelName = modelName[:27] + "..."
				}
			}
			lines = append(lines, fmt.Sprintf("  %-20s %s",
				modelName,
				formatTokens(mt.tokens)))
		}
	}

	return boxStyle.Render(strings.Join(lines, "\n"))
}
