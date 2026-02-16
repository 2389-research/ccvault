// ABOUTME: Main Bubble Tea application for ccvault TUI
// ABOUTME: Manages views, navigation, and global state

package tui

import (
	"fmt"
	"time"

	"github.com/2389-research/ccvault/internal/db"
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
)

// View represents the current view
type View int

const (
	DashboardView View = iota
	ProjectsView
	SessionsView
	ConversationView
	SearchView
	AnalyticsView
	SyncingView
)

// KeyMap defines keyboard shortcuts
type KeyMap struct {
	Up       key.Binding
	Down     key.Binding
	Left     key.Binding
	Right    key.Binding
	Enter    key.Binding
	Back     key.Binding
	Quit     key.Binding
	Help     key.Binding
	Search   key.Binding
	Refresh  key.Binding
	PageUp   key.Binding
	PageDown key.Binding
}

var keys = KeyMap{
	Up: key.NewBinding(
		key.WithKeys("up", "k"),
		key.WithHelp("↑/k", "up"),
	),
	Down: key.NewBinding(
		key.WithKeys("down", "j"),
		key.WithHelp("↓/j", "down"),
	),
	Left: key.NewBinding(
		key.WithKeys("left", "h"),
		key.WithHelp("←/h", "left"),
	),
	Right: key.NewBinding(
		key.WithKeys("right", "l"),
		key.WithHelp("→/l", "right"),
	),
	Enter: key.NewBinding(
		key.WithKeys("enter"),
		key.WithHelp("enter", "select"),
	),
	Back: key.NewBinding(
		key.WithKeys("esc", "backspace"),
		key.WithHelp("esc", "back"),
	),
	Quit: key.NewBinding(
		key.WithKeys("q", "ctrl+c"),
		key.WithHelp("q", "quit"),
	),
	Help: key.NewBinding(
		key.WithKeys("?"),
		key.WithHelp("?", "help"),
	),
	Search: key.NewBinding(
		key.WithKeys("/"),
		key.WithHelp("/", "search"),
	),
	Refresh: key.NewBinding(
		key.WithKeys("r"),
		key.WithHelp("r", "refresh"),
	),
	PageUp: key.NewBinding(
		key.WithKeys("pgup", "ctrl+u"),
		key.WithHelp("pgup", "page up"),
	),
	PageDown: key.NewBinding(
		key.WithKeys("pgdown", "ctrl+d"),
		key.WithHelp("pgdown", "page down"),
	),
}

// Model is the main TUI model
type Model struct {
	db         *db.DB
	cacheDir   string
	claudeHome string
	view       View
	width      int
	height     int
	err        error

	// View-specific state
	dashboard    *DashboardModel
	projects     *ProjectsModel
	sessions     *SessionsModel
	conversation *ConversationModel
	search       *SearchModel
	analytics    *AnalyticsModel
	syncing      *SyncingModel

	// Navigation stack
	viewStack []View
}

// New creates a new TUI model
func New(database *db.DB, cacheDir, claudeHome string) *Model {
	m := &Model{
		db:         database,
		cacheDir:   cacheDir,
		claudeHome: claudeHome,
		view:       DashboardView,
		viewStack:  []View{DashboardView},
	}

	m.dashboard = NewDashboardModel(database)
	m.projects = NewProjectsModel(database)
	m.sessions = NewSessionsModel(database)
	m.conversation = NewConversationModel(database)
	m.search = NewSearchModel(database)
	m.analytics = NewAnalyticsModel(database)
	m.syncing = NewSyncingModel(database, claudeHome)

	return m
}

// syncCheckMsg is sent after checking sync status
type syncCheckMsg struct {
	needsSync bool
	reason    string
}

// Init implements tea.Model
func (m *Model) Init() tea.Cmd {
	return m.checkSyncStatus
}

// checkSyncStatus checks if a sync is needed
func (m *Model) checkSyncStatus() tea.Msg {
	// Check if we have any sessions
	count, _, _, err := m.db.GetSessionStats()
	if err != nil {
		// If we can't check, proceed to dashboard
		return syncCheckMsg{needsSync: false}
	}

	// If no sessions, we definitely need to sync
	if count == 0 {
		return syncCheckMsg{needsSync: true, reason: "No conversations found. Running initial sync..."}
	}

	// Check if last activity is stale (more than 24 hours since last sync)
	_, lastActivity, err := m.db.GetFirstAndLastActivity()
	if err != nil {
		return syncCheckMsg{needsSync: false}
	}

	// If last activity is more than 24 hours ago, suggest sync
	if time.Since(lastActivity) > 24*time.Hour {
		return syncCheckMsg{needsSync: true, reason: "Conversations may be out of date. Running sync..."}
	}

	return syncCheckMsg{needsSync: false}
}

// Update implements tea.Model
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case syncCheckMsg:
		if msg.needsSync {
			// Start syncing
			m.view = SyncingView
			m.viewStack = []View{SyncingView}
			m.syncing = NewSyncingModel(m.db, m.claudeHome)
			return m, m.syncing.Init()
		}
		// No sync needed, proceed to dashboard
		return m, m.dashboard.Init()

	case syncCompleteMsg:
		// Sync is done, transition to dashboard
		m.syncing.Update(msg)
		// Give a moment to show completion, then transition
		return m, tea.Tick(500*time.Millisecond, func(t time.Time) tea.Msg {
			return syncDoneTransitionMsg{}
		})

	case syncDoneTransitionMsg:
		// Refresh dashboard with new data
		m.dashboard = NewDashboardModel(m.db)
		m.view = DashboardView
		m.viewStack = []View{DashboardView}
		return m, m.dashboard.Init()

	case tea.KeyMsg:
		// If syncing is done and has error, any key continues
		if m.view == SyncingView && m.syncing.IsDone() {
			m.dashboard = NewDashboardModel(m.db)
			m.view = DashboardView
			m.viewStack = []View{DashboardView}
			return m, m.dashboard.Init()
		}

		// Global key handling
		switch {
		case key.Matches(msg, keys.Quit):
			if m.view == DashboardView {
				return m, tea.Quit
			}
			// Go back
			return m.popView()

		case key.Matches(msg, keys.Back):
			// In search view, only handle esc - let backspace pass through to input
			if m.view == SearchView {
				if msg.String() == "esc" {
					// Only pop if esc is pressed (not backspace)
					return m.popView()
				}
				// Let backspace pass through to search input
				break
			}
			return m.popView()

		case key.Matches(msg, keys.Search):
			if m.view != SearchView {
				return m.pushView(SearchView, nil)
			}
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		// Propagate to child views
		m.dashboard.SetSize(msg.Width, msg.Height)
		m.projects.SetSize(msg.Width, msg.Height)
		m.sessions.SetSize(msg.Width, msg.Height)
		m.conversation.SetSize(msg.Width, msg.Height)
		m.search.SetSize(msg.Width, msg.Height)
		m.analytics.SetSize(msg.Width, msg.Height)
		return m, nil

	case NavigateMsg:
		return m.pushView(msg.View, msg.Data)

	case ErrorMsg:
		m.err = msg.Err
		return m, nil
	}

	// Delegate to current view
	var cmd tea.Cmd
	switch m.view {
	case DashboardView:
		cmd = m.dashboard.Update(msg)
	case ProjectsView:
		cmd = m.projects.Update(msg)
	case SessionsView:
		cmd = m.sessions.Update(msg)
	case ConversationView:
		cmd = m.conversation.Update(msg)
	case SearchView:
		cmd = m.search.Update(msg)
	case AnalyticsView:
		cmd = m.analytics.Update(msg)
	case SyncingView:
		cmd = m.syncing.Update(msg)
	}

	return m, cmd
}

// View implements tea.Model
func (m *Model) View() string {
	if m.err != nil {
		return errorStyle.Render(fmt.Sprintf("Error: %v\n\nPress q to quit.", m.err))
	}

	switch m.view {
	case DashboardView:
		return m.dashboard.View()
	case ProjectsView:
		return m.projects.View()
	case SessionsView:
		return m.sessions.View()
	case ConversationView:
		return m.conversation.View()
	case SearchView:
		return m.search.View()
	case AnalyticsView:
		return m.analytics.View()
	case SyncingView:
		return m.syncing.View()
	default:
		return "Unknown view"
	}
}

// pushView navigates to a new view
func (m *Model) pushView(view View, data interface{}) (*Model, tea.Cmd) {
	m.viewStack = append(m.viewStack, view)
	m.view = view

	var cmd tea.Cmd
	switch view {
	case ProjectsView:
		cmd = m.projects.Init()
	case SessionsView:
		if projectID, ok := data.(int64); ok {
			m.sessions.SetProject(projectID)
		}
		cmd = m.sessions.Init()
	case ConversationView:
		if sessionID, ok := data.(string); ok {
			m.conversation.SetSession(sessionID)
		}
		cmd = m.conversation.Init()
	case SearchView:
		cmd = m.search.Init()
	case AnalyticsView:
		cmd = m.analytics.Init()
	case SyncingView:
		m.syncing = NewSyncingModel(m.db, m.claudeHome)
		cmd = m.syncing.Init()
	}

	return m, cmd
}

// popView goes back to the previous view
func (m *Model) popView() (*Model, tea.Cmd) {
	if len(m.viewStack) > 1 {
		m.viewStack = m.viewStack[:len(m.viewStack)-1]
		m.view = m.viewStack[len(m.viewStack)-1]
	}
	return m, nil
}

// NavigateMsg is sent to navigate to a different view
type NavigateMsg struct {
	View View
	Data interface{}
}

// ErrorMsg is sent when an error occurs
type ErrorMsg struct {
	Err error
}

// syncDoneTransitionMsg signals time to transition from sync to dashboard
type syncDoneTransitionMsg struct{}

// Run starts the TUI
func Run(database *db.DB, cacheDir, claudeHome string) error {
	p := tea.NewProgram(
		New(database, cacheDir, claudeHome),
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)

	_, err := p.Run()
	return err
}
