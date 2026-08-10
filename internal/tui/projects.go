// ABOUTME: Projects list view for ccvault TUI
// ABOUTME: Shows all indexed projects with navigation

package tui

import (
	"fmt"
	"strings"

	"github.com/2389-research/ccvault/internal/compact"
	"github.com/2389-research/ccvault/internal/db"
	"github.com/2389-research/ccvault/internal/projectref"
	"github.com/2389-research/ccvault/pkg/models"
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
)

// ProjectsModel holds projects list state
type ProjectsModel struct {
	db       *db.DB
	width    int
	height   int
	projects []models.Project
	cursor   int
	offset   int
	loading  bool
	pageSize int
}

// NewProjectsModel creates a new projects model
func NewProjectsModel(database *db.DB) *ProjectsModel {
	return &ProjectsModel{
		db:       database,
		loading:  true,
		pageSize: 20,
	}
}

// Init loads projects data
func (m *ProjectsModel) Init() tea.Cmd {
	return m.loadProjects
}

func (m *ProjectsModel) loadProjects() tea.Msg {
	projects, err := m.db.GetProjects("activity", 0)
	if err != nil {
		return ErrorMsg{Err: err}
	}
	return projectsLoadedMsg{projects: projects}
}

type projectsLoadedMsg struct {
	projects []models.Project
}

// Update handles projects view events
func (m *ProjectsModel) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case projectsLoadedMsg:
		m.projects = msg.projects
		m.loading = false
		m.cursor = 0
		m.offset = 0
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
			if m.cursor < len(m.projects)-1 {
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
			if m.cursor >= len(m.projects) {
				m.cursor = len(m.projects) - 1
			}
			if m.cursor >= m.offset+m.visibleRows() {
				m.offset = m.cursor - m.visibleRows() + 1
			}
		case key.Matches(msg, keys.Enter):
			if len(m.projects) > 0 {
				project := m.projects[m.cursor]
				return func() tea.Msg {
					return NavigateMsg{View: SessionsView, Data: project.ID}
				}
			}
		case key.Matches(msg, keys.Refresh):
			m.loading = true
			return m.loadProjects
		}
	}
	return nil
}

func (m *ProjectsModel) visibleRows() int {
	rows := m.height - 8 // Header, footer, etc.
	if rows < 5 {
		rows = 5
	}
	return rows
}

// SetSize sets the viewport size
func (m *ProjectsModel) SetSize(width, height int) {
	m.width = width
	m.height = height
}

// View renders the projects list
func (m *ProjectsModel) View() string {
	if m.loading {
		return titleStyle.Render("Loading projects...")
	}

	var b strings.Builder

	// Title
	b.WriteString(titleStyle.Render("Projects"))
	b.WriteString("\n")
	b.WriteString(subtitleStyle.Render(fmt.Sprintf("%d projects indexed", len(m.projects))))
	b.WriteString("\n\n")

	if len(m.projects) == 0 {
		b.WriteString(normalStyle.Render("No projects found. Run 'ccvault sync' first."))
		b.WriteString("\n")
	} else {
		layout := pickProjectsLayout(m.width)

		// Header — SOURCE column is dropped entirely when the layout
		// tier assigns it width 0.
		var header string
		if layout.Source > 0 {
			header = fmt.Sprintf("%s %s %s %s %s %s",
				padVisual("PROJECT", layout.Project),
				padVisual("PATH", layout.Path),
				padVisual("SOURCE", layout.Source),
				padVisual("SESSIONS", layout.Sessions),
				padVisual("TOKENS", layout.Tokens),
				padVisual("ACTIVE", layout.LastActive),
			)
		} else {
			header = fmt.Sprintf("%s %s %s %s %s",
				padVisual("PROJECT", layout.Project),
				padVisual("PATH", layout.Path),
				padVisual("SESSIONS", layout.Sessions),
				padVisual("TOKENS", layout.Tokens),
				padVisual("ACTIVE", layout.LastActive),
			)
		}
		b.WriteString(headerStyle.Render(header))
		b.WriteString("\n")

		// List
		visibleRows := m.visibleRows()
		end := m.offset + visibleRows
		if end > len(m.projects) {
			end = len(m.projects)
		}

		for i := m.offset; i < end; i++ {
			p := m.projects[i]
			// Class A — Label for the short column, Path shown separately.
			// Each cell is a compact.Result so the row visibly signals
			// which columns had to be shortened at this width.
			nameCell := cellText(compact.Truncate(projectref.Label(&p), layout.Project), layout.Project)
			pathCell := cellText(compact.Path(p.Path, layout.Path), layout.Path)
			sessionsCell := padVisual(fmt.Sprintf("%d", p.SessionCount), layout.Sessions)
			tokensCell := padVisual(formatTokensPlain(p.TotalTokens), layout.Tokens)
			lastActiveCell := cellText(compact.Date(p.LastActivityAt, layout.LastActive), layout.LastActive)

			var line string
			if layout.Source > 0 {
				sourceCell := cellText(compact.Source(p.Source, layout.Source), layout.Source)
				line = fmt.Sprintf("%s %s %s %s %s %s",
					nameCell, pathCell, sourceCell,
					sessionsCell, tokensCell, lastActiveCell)
			} else {
				line = fmt.Sprintf("%s %s %s %s %s",
					nameCell, pathCell,
					sessionsCell, tokensCell, lastActiveCell)
			}

			if i == m.cursor {
				b.WriteString(selectedStyle.Render(line))
			} else {
				b.WriteString(normalStyle.Render(line))
			}
			b.WriteString("\n")
		}

		// Scroll indicator
		if len(m.projects) > visibleRows {
			b.WriteString(subtitleStyle.Render(fmt.Sprintf("\n  Showing %d-%d of %d",
				m.offset+1, end, len(m.projects))))
		}
	}

	b.WriteString("\n")
	b.WriteString(helpStyle.Render("↑/↓: navigate • enter: view sessions • pgup/pgdn: page • esc: back • q: quit"))

	return b.String()
}

// formatTokensPlain formats tokens without styling (for table alignment)
func formatTokensPlain(n int64) string {
	if n >= 1_000_000_000 {
		return fmt.Sprintf("%.1fB", float64(n)/1_000_000_000)
	}
	if n >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
	if n >= 1_000 {
		return fmt.Sprintf("%.1fK", float64(n)/1_000)
	}
	return fmt.Sprintf("%d", n)
}
