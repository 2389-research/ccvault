// ABOUTME: Sessions list view for ccvault TUI
// ABOUTME: Shows sessions for a project with navigation

package tui

import (
	"fmt"
	"strings"

	"github.com/2389-research/ccvault/internal/db"
	"github.com/2389-research/ccvault/internal/projectref"
	"github.com/2389-research/ccvault/pkg/models"
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
)

// SessionsModel holds sessions list state
type SessionsModel struct {
	db        *db.DB
	width     int
	height    int
	projectID int64
	project   *models.Project
	sessions  []models.Session
	cursor    int
	offset    int
	loading   bool
}

// NewSessionsModel creates a new sessions model
func NewSessionsModel(database *db.DB) *SessionsModel {
	return &SessionsModel{
		db:      database,
		loading: true,
	}
}

// SetProject sets the project to show sessions for
func (m *SessionsModel) SetProject(projectID int64) {
	m.projectID = projectID
	m.cursor = 0
	m.offset = 0
}

// Init loads sessions data
func (m *SessionsModel) Init() tea.Cmd {
	return m.loadSessions
}

func (m *SessionsModel) loadSessions() tea.Msg {
	var project *models.Project
	if m.projectID > 0 {
		var err error
		project, err = m.db.GetProject(m.projectID)
		if err != nil {
			return ErrorMsg{Err: err}
		}
	}

	sessions, err := m.db.GetSessions(m.projectID, 0)
	if err != nil {
		return ErrorMsg{Err: err}
	}
	return sessionsLoadedMsg{sessions: sessions, project: project}
}

type sessionsLoadedMsg struct {
	sessions []models.Session
	project  *models.Project
}

// Update handles sessions view events
func (m *SessionsModel) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case sessionsLoadedMsg:
		m.sessions = msg.sessions
		m.project = msg.project
		m.loading = false
		return nil

	case tea.KeyMsg:
		switch {
		case key.Matches(msg, keys.Up):
			if m.cursor > 0 {
				m.cursor--
				if m.cursor < m.offset {
					m.offset = m.cursor
				}
			}
		case key.Matches(msg, keys.Down):
			if m.cursor < len(m.sessions)-1 {
				m.cursor++
				if m.cursor >= m.offset+m.visibleRows() {
					m.offset = m.cursor - m.visibleRows() + 1
				}
			}
		case key.Matches(msg, keys.PageUp):
			m.cursor -= m.visibleRows()
			if m.cursor < 0 {
				m.cursor = 0
			}
			m.offset = m.cursor
		case key.Matches(msg, keys.PageDown):
			m.cursor += m.visibleRows()
			if m.cursor >= len(m.sessions) {
				m.cursor = len(m.sessions) - 1
			}
			if m.cursor >= m.offset+m.visibleRows() {
				m.offset = m.cursor - m.visibleRows() + 1
			}
		case key.Matches(msg, keys.Enter):
			if len(m.sessions) > 0 {
				session := m.sessions[m.cursor]
				return func() tea.Msg {
					return NavigateMsg{View: ConversationView, Data: session.ID}
				}
			}
		case key.Matches(msg, keys.Refresh):
			m.loading = true
			return m.loadSessions
		}
	}
	return nil
}

func (m *SessionsModel) visibleRows() int {
	rows := m.height - 8
	if rows < 5 {
		rows = 5
	}
	return rows
}

// SetSize sets the viewport size
func (m *SessionsModel) SetSize(width, height int) {
	m.width = width
	m.height = height
}

// View renders the sessions list
func (m *SessionsModel) View() string {
	if m.loading {
		return titleStyle.Render("Loading sessions...")
	}

	var b strings.Builder

	// Title — Class B (inline), path shown so same-basename projects don't
	// render an ambiguous title when the user drills into one of them.
	title := "Sessions"
	if m.project != nil {
		title = fmt.Sprintf("Sessions: %s", projectref.Inline(m.project))
	}
	b.WriteString(titleStyle.Render(title))
	b.WriteString("\n")
	b.WriteString(subtitleStyle.Render(fmt.Sprintf("%d sessions", len(m.sessions))))
	b.WriteString("\n\n")

	if len(m.sessions) == 0 {
		b.WriteString(normalStyle.Render("No sessions found."))
		b.WriteString("\n")
	} else {
		// Header — show project column when not filtered to a specific project
		showProject := m.project == nil
		var header string
		if showProject {
			header = fmt.Sprintf("%-22s %-20s %-12s %6s %10s %-30s", "PROJECT", "STARTED", "SOURCE", "TURNS", "TOKENS", "MODEL")
		} else {
			header = fmt.Sprintf("%-20s %-12s %6s %10s %-30s", "STARTED", "SOURCE", "TURNS", "TOKENS", "MODEL")
		}
		b.WriteString(headerStyle.Render(header))
		b.WriteString("\n")

		// List
		visibleRows := m.visibleRows()
		end := m.offset + visibleRows
		if end > len(m.sessions) {
			end = len(m.sessions)
		}

		for i := m.offset; i < end; i++ {
			s := m.sessions[i]
			model := s.Model
			if len(model) > 28 {
				model = model[:25] + "..."
			}
			tokens := s.InputTokens + s.OutputTokens

			source := s.Source
			if len(source) > 10 {
				source = source[:10] + ".."
			}

			var line string
			if showProject {
				// Class A — short label; session cell has its own row
				// identifiers, same-basename ambiguity is acceptable here.
				project := projectref.Label(&models.Project{Path: s.ProjectPath})
				if len(project) > 20 {
					project = "..." + project[len(project)-17:]
				}
				line = fmt.Sprintf("%-22s %-20s %-12s %6d %10s %-30s",
					project,
					s.StartedAt.Format("2006-01-02 15:04"),
					source,
					s.TurnCount,
					formatTokensPlain(tokens),
					model)
			} else {
				line = fmt.Sprintf("%-20s %-12s %6d %10s %-30s",
					s.StartedAt.Format("2006-01-02 15:04"),
					source,
					s.TurnCount,
					formatTokensPlain(tokens),
					model)
			}

			if i == m.cursor {
				b.WriteString(selectedStyle.Render(line))
			} else {
				b.WriteString(normalStyle.Render(line))
			}
			b.WriteString("\n")
		}

		// Scroll indicator
		if len(m.sessions) > visibleRows {
			b.WriteString(subtitleStyle.Render(fmt.Sprintf("\n  Showing %d-%d of %d",
				m.offset+1, end, len(m.sessions))))
		}
	}

	b.WriteString("\n")
	b.WriteString(helpStyle.Render("↑/↓: navigate • enter: view conversation • pgup/pgdn: page • esc: back"))

	return b.String()
}
