// ABOUTME: Sessions list view for ccvault TUI
// ABOUTME: Shows sessions for a project with navigation

package tui

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/2389-research/ccvault/internal/compact"
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
		showProject := m.project == nil
		layout := pickSessionsLayout(m.width, showProject)

		// Build the header from the layout — same set of cells shown in
		// the rows, so tier decisions carry through automatically.
		var headerParts []string
		if layout.Project > 0 {
			headerParts = append(headerParts, padVisual("PROJECT", layout.Project))
		}
		headerParts = append(headerParts, padVisual("STARTED", layout.Started))
		if layout.Source > 0 {
			headerParts = append(headerParts, padVisual("SOURCE", layout.Source))
		}
		headerParts = append(headerParts,
			padVisual("TURNS", layout.Turns),
			padVisual("TOKENS", layout.Tokens),
			padVisual("MODEL", layout.Model),
		)
		b.WriteString(headerStyle.Render(strings.Join(headerParts, " ")))
		b.WriteString("\n")

		// Load all projects once so adapter DisplayNames (jeff, hex,
		// nanoclaw) surface in the PROJECT column instead of falling
		// through to basename via a synthetic Project stub.
		projectsList, _ := m.db.GetProjects("activity", 0)
		byPath := projectref.ProjectsByPath(projectsList)

		// List
		visibleRows := m.visibleRows()
		end := m.offset + visibleRows
		if end > len(m.sessions) {
			end = len(m.sessions)
		}

		for i := m.offset; i < end; i++ {
			s := m.sessions[i]
			tokens := s.InputTokens + s.OutputTokens

			var parts []string
			if layout.Project > 0 {
				project := projectref.LabelFromPath(s.ProjectPath, byPath)
				parts = append(parts, cellText(compact.Truncate(project, layout.Project), layout.Project))
			}
			// STARTED — date + time in a single cell. Renders differently
			// based on how much space we've been given.
			startedText := s.StartedAt.Format("2006-01-02 15:04")
			// Use rune count for consistency with the rest of the compact
			// discipline. startedText is ASCII so bytes==runes here, but
			// staying uniform means future format changes won't misalign.
			if layout.Started < utf8.RuneCountInString(startedText) {
				// Drop the time to fit
				startedText = compact.Date(s.StartedAt, layout.Started).Text
				parts = append(parts, cellText(compact.Result{Text: startedText, Shortened: true}, layout.Started))
			} else {
				parts = append(parts, padVisual(startedText, layout.Started))
			}
			if layout.Source > 0 {
				parts = append(parts, cellText(compact.Source(s.Source, layout.Source), layout.Source))
			}
			parts = append(parts,
				padVisual(fmt.Sprintf("%d", s.TurnCount), layout.Turns),
				padVisual(formatTokensPlain(tokens), layout.Tokens),
				cellText(compact.Model(s.Model, layout.Model), layout.Model),
			)

			line := strings.Join(parts, " ")
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
