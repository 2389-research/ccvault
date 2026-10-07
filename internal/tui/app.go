// ABOUTME: Main Bubble Tea application for ccvault TUI
// ABOUTME: Manages views, navigation, and global state

package tui

import (
	"fmt"
	"os"
	"time"

	"github.com/2389-research/ccvault/internal/config"
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
	Up        key.Binding
	Down      key.Binding
	Left      key.Binding
	Right     key.Binding
	Enter     key.Binding
	Back      key.Binding
	Quit      key.Binding
	ForceQuit key.Binding
	Help      key.Binding
	Search    key.Binding
	Refresh   key.Binding
	PageUp    key.Binding
	PageDown  key.Binding
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
	// q is context-sensitive: back in a nested view, quit on the dashboard.
	// ctrl+c is the universal interrupt and always quits, from any view.
	Quit: key.NewBinding(
		key.WithKeys("q"),
		key.WithHelp("q", "quit"),
	),
	ForceQuit: key.NewBinding(
		key.WithKeys("ctrl+c"),
		key.WithHelp("ctrl+c", "quit"),
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

// isTextRune reports whether a key event carries printable text rather than a
// command. A single space arrives as tea.KeySpace rather than KeyRunes, and an
// alt-modified rune is a command combo, not a character.
func isTextRune(msg tea.KeyMsg) bool {
	if msg.Alt {
		return false
	}
	return msg.Type == tea.KeyRunes || msg.Type == tea.KeySpace
}

// textInputFocused reports whether the current view has a text input holding
// keyboard focus, in which case printable runes are that input's to consume.
func (m *Model) textInputFocused() bool {
	// Search is the only view with a text input; the rest navigate with
	// single keys. Add a case here when another view grows one.
	if m.view == SearchView {
		return m.search.InputFocused()
	}
	return false
}

// Model is the main TUI model
type Model struct {
	db       *db.DB
	cacheDir string
	sources  []config.SourceConfig
	view     View
	width    int
	height   int
	err      error
	showHelp bool

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
func New(database *db.DB, cacheDir string, sources []config.SourceConfig) *Model {
	m := &Model{
		db:        database,
		cacheDir:  cacheDir,
		sources:   sources,
		view:      DashboardView,
		viewStack: []View{DashboardView},
	}

	m.dashboard = NewDashboardModel(database)
	m.projects = NewProjectsModel(database)
	m.sessions = NewSessionsModel(database)
	m.conversation = NewConversationModel(database)
	m.search = NewSearchModel(database)
	m.analytics = NewAnalyticsModel(database, cacheDir)
	m.syncing = NewSyncingModel(database, sources)

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
			m.syncing = NewSyncingModel(m.db, m.sources)
			return m, m.syncing.Init()
		}
		// No sync needed, proceed to dashboard
		return m, m.dashboard.Init()

	case syncTickMsg:
		// The tick belongs to the sync's lifecycle, not to whichever view holds
		// the screen: it is the only thing that drains the progress channels,
		// and nothing but its own handler schedules the next one. Delegating it
		// by view killed the chain the moment "/" pushed the search view on top
		// of a running sync — SearchModel has no case for it — after which
		// progress stopped, doneCh was never drained, and the goroutine blocked
		// forever on a full channel still holding the SQLite writer (#99).
		// syncCompleteMsg has always been handled here; this is the same
		// reasoning applied to the message that leads up to it.
		return m, m.syncing.Update(msg)

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

		// Now that the tick survives a view change (#99), a sync can finish
		// while the user is somewhere else — typing a query, say. Yanking them
		// to the dashboard would throw that work away, so only the syncing view
		// itself gets moved on. Either way the finished syncing view stops being
		// what "back" returns to: it is the stack's root while a sync runs, and
		// the dashboard takes its place.
		for i, v := range m.viewStack {
			if v == SyncingView {
				m.viewStack[i] = DashboardView
			}
		}
		if m.view != SyncingView {
			return m, m.dashboard.Init()
		}
		m.view = DashboardView
		m.viewStack = []View{DashboardView}
		return m, m.dashboard.Init()

	case tea.KeyMsg:
		// ctrl+c quits from anywhere, ahead of every other key handling —
		// including the sync short-circuit below, so it works mid-sync. The
		// sync goroutine is cancelled here; tui.Run then waits for it to
		// unwind before the caller closes the database.
		if key.Matches(msg, keys.ForceQuit) {
			m.syncing.Cancel()
			return m, tea.Quit
		}

		// The help overlay is modal: it swallows whatever key dismisses it, so
		// closing it can't also act on the view underneath.
		if m.showHelp {
			m.showHelp = false
			return m, nil
		}

		// If syncing is done and has error, any key continues
		if m.view == SyncingView && m.syncing.IsDone() {
			m.dashboard = NewDashboardModel(m.db)
			m.view = DashboardView
			m.viewStack = []View{DashboardView}
			return m, m.dashboard.Init()
		}

		// A focused text input owns every printable rune. Without this,
		// global single-key navigation makes those characters untypable:
		// q popped the search view before the input ever saw it, so no
		// query containing "q" could be entered. ctrl+c is handled above
		// and stays universal; esc, tab and the arrows are not text, so
		// they keep navigating.
		if isTextRune(msg) && m.textInputFocused() {
			break
		}

		// Global key handling
		switch {
		case key.Matches(msg, keys.Help):
			// Reached only when no text input claimed the rune above, so "?"
			// stays typeable in the search box.
			m.showHelp = true
			return m, nil

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

	if m.showHelp {
		return helpView(m.view, m.width)
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
		switch d := data.(type) {
		case int64:
			m.sessions.SetProject(d)
		case SubagentsOf:
			m.sessions.SetParentSession(string(d))
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
		// Replacing the model drops the only handle on its goroutine, so
		// retire the old one first rather than orphaning it.
		m.syncing.Cancel()
		m.syncing = NewSyncingModel(m.db, m.sources)
		cmd = m.syncing.Init()
	}

	return m, cmd
}

// popView goes back to the previous view
func (m *Model) popView() (*Model, tea.Cmd) {
	if len(m.viewStack) > 1 {
		// Leaving the syncing view means SyncingModel.Update stops being
		// called, so nothing drains the progress channels any more. Without
		// this the sync goroutine blocks on send and never returns.
		if m.view == SyncingView {
			m.syncing.Cancel()
		}
		m.viewStack = m.viewStack[:len(m.viewStack)-1]
		m.view = m.viewStack[len(m.viewStack)-1]
	}
	return m, nil
}

// shutdownSync cancels any running sync and waits for its goroutine to
// return. Called once the Bubble Tea program has stopped, before the caller
// closes the database: a sync goroutine still mid-transaction when the
// connection closes is the data-corruption-adjacent half of #24.
func (m *Model) shutdownSync() {
	if m.syncing == nil {
		return
	}
	m.syncing.Cancel()
	if !m.syncing.WaitForExit(syncShutdownTimeout) {
		// Give up waiting rather than holding the user's exit open forever, but
		// say so: the database is about to close under a writer that is still
		// running, and the archive's counters may not have been reconciled.
		fmt.Fprintln(os.Stderr,
			"warning: sync did not stop within "+syncShutdownTimeout.String()+
				"; run `ccvault sync` to bring the archive back in line")
	}
}

// NavigateMsg is sent to navigate to a different view
type NavigateMsg struct {
	View View
	Data interface{}
}

// SubagentsOf is NavigateMsg.Data for SessionsView when the list should show
// one session's subagents rather than a project's top-level sessions. A named
// type rather than a bare string so the two cases can't be confused — an
// int64 means "project", a SubagentsOf means "this parent's children".
type SubagentsOf string

// ErrorMsg is sent when an error occurs
type ErrorMsg struct {
	Err error
}

// syncDoneTransitionMsg signals time to transition from sync to dashboard
type syncDoneTransitionMsg struct{}

// syncShutdownTimeout bounds how long a quit waits for a cancelled sync
// goroutine to unwind. One session's transaction is the unit of work it has to
// finish rolling back, so this is generous.
const syncShutdownTimeout = 5 * time.Second

// Run starts the TUI
func Run(database *db.DB, cacheDir string, sources []config.SourceConfig) error {
	m := New(database, cacheDir, sources)
	p := tea.NewProgram(
		m,
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)

	_, err := p.Run()

	// The program loop has stopped, so nothing drains the sync channels any
	// more. Unwind the sync goroutine here, while the database is still open:
	// our caller closes it the moment we return.
	m.shutdownSync()

	return err
}
