// ABOUTME: Search view for ccvault TUI
// ABOUTME: Provides full-text search across conversations with result navigation

package tui

import (
	"fmt"
	"strings"

	"github.com/2389-research/ccvault/internal/compact"
	"github.com/2389-research/ccvault/internal/db"
	"github.com/2389-research/ccvault/internal/projectref"
	"github.com/2389-research/ccvault/internal/search"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// SearchModel holds search view state
type SearchModel struct {
	db       *db.DB
	width    int
	height   int
	input    textinput.Model
	results  []search.Result
	cursor   int
	offset   int
	loading  bool
	searched bool
	focused  bool // true = input focused, false = results focused
}

// NewSearchModel creates a new search model
func NewSearchModel(database *db.DB) *SearchModel {
	ti := textinput.New()
	ti.Placeholder = "Type to search..."
	ti.Focus()
	ti.CharLimit = 256
	ti.Width = 60

	return &SearchModel{
		db:      database,
		input:   ti,
		focused: true,
	}
}

// InputFocused reports whether the query input holds keyboard focus. The app
// consults this before claiming a printable rune for global navigation.
func (m *SearchModel) InputFocused() bool {
	return m.focused
}

// focusInput moves keyboard focus from the results list to the query box.
func (m *SearchModel) focusInput() {
	m.focused = true
	m.input.Focus()
}

// updateInput hands a key to the query box.
//
// The switch in Update claims several key names for results navigation that
// the input also wants — home, end, ctrl+u, ctrl+d — so those have to be
// forwarded explicitly from the branch that claimed them. Without it,
// cursor-to-start, cursor-to-end and line editing did nothing while typing
// (#59).
func (m *SearchModel) updateInput(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return cmd
}

// Init initializes the search model
func (m *SearchModel) Init() tea.Cmd {
	m.input.Focus()
	m.focused = true
	return textinput.Blink
}

// Update handles search view events
func (m *SearchModel) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case searchResultsMsg:
		m.results = msg.results
		m.loading = false
		m.searched = true
		m.cursor = 0
		m.offset = 0
		// Auto-focus results if we have any
		if len(m.results) > 0 {
			m.focused = false
			m.input.Blur()
		}
		return nil

	case tea.KeyMsg:
		// While the query input holds focus, printable runes are text and go
		// straight to it. The single-key shortcuts below would otherwise claim
		// some of them — "/" refocuses the input, so it was untypable, and the
		// vim keys g/G belong to results browsing only.
		if m.focused && isTextRune(msg) {
			return m.updateInput(msg)
		}

		switch msg.String() {
		case "ctrl+c":
			return nil // Let parent handle quit

		// esc is deliberately absent: it means "back out of this view" here
		// exactly as it does everywhere else in the app, and app.go pops the
		// view before delegating. The way back from the results list to the
		// query box is "/", which the results footer advertises (#59).

		case "enter":
			if m.focused {
				// Execute search
				if m.input.Value() != "" {
					m.loading = true
					return m.doSearch
				}
			} else {
				// Navigate to selected result
				if len(m.results) > 0 && m.cursor < len(m.results) {
					result := m.results[m.cursor]
					return func() tea.Msg {
						return NavigateMsg{View: ConversationView, Data: result.SessionID}
					}
				}
			}

		case "tab", "down":
			if m.focused && len(m.results) > 0 {
				// Switch to results
				m.focused = false
				m.input.Blur()
				return nil
			}
			if !m.focused {
				// Navigate down in results
				if m.cursor < len(m.results)-1 {
					m.cursor++
					m.ensureVisible()
				}
			}

		case "up":
			if !m.focused {
				if m.cursor > 0 {
					m.cursor--
					m.ensureVisible()
				} else {
					m.focusInput()
				}
			}

		case "shift+tab":
			if !m.focused {
				m.focusInput()
			}

		case "/":
			if !m.focused {
				m.focusInput()
				return nil
			}

		case "pgup", "ctrl+u":
			if m.focused {
				// ctrl+u clears the query back to the cursor.
				return m.updateInput(msg)
			}
			if len(m.results) > 0 {
				m.cursor -= m.visibleRows()
				if m.cursor < 0 {
					m.cursor = 0
				}
				m.ensureVisible()
			}

		case "pgdown", "ctrl+d":
			if m.focused {
				// ctrl+d deletes the character under the cursor.
				return m.updateInput(msg)
			}
			if len(m.results) > 0 {
				m.cursor += m.visibleRows()
				if m.cursor >= len(m.results) {
					m.cursor = len(m.results) - 1
				}
				m.ensureVisible()
			}

		case "home":
			if m.focused {
				// Cursor to the start of the query.
				return m.updateInput(msg)
			}
			if len(m.results) > 0 {
				m.cursor = 0
				m.offset = 0
			}

		case "end":
			if m.focused {
				// Cursor to the end of the query.
				return m.updateInput(msg)
			}
			if len(m.results) > 0 {
				m.cursor = len(m.results) - 1
				m.ensureVisible()
			}

		default:
			if m.focused {
				return m.updateInput(msg)
			}
			// vim-style navigation when not typing
			switch msg.String() {
			case "g":
				if len(m.results) > 0 {
					m.cursor = 0
					m.offset = 0
				}
			case "G":
				if len(m.results) > 0 {
					m.cursor = len(m.results) - 1
					m.ensureVisible()
				}
			}
		}

	case tea.MouseMsg:
		switch msg.Button {
		case tea.MouseButtonWheelUp:
			if !m.focused && len(m.results) > 0 {
				if m.cursor > 0 {
					m.cursor--
					m.ensureVisible()
				}
			}
		case tea.MouseButtonWheelDown:
			if !m.focused && len(m.results) > 0 {
				if m.cursor < len(m.results)-1 {
					m.cursor++
					m.ensureVisible()
				}
			}
		case tea.MouseButtonLeft:
			// Click on a result to select it
			if !m.focused && msg.Y >= 5 { // Below header area
				resultIdx := m.offset + (msg.Y-5)/3 // Each result is ~3 lines
				if resultIdx < len(m.results) {
					m.cursor = resultIdx
				}
			}
		}
	}

	return nil
}

func (m *SearchModel) ensureVisible() {
	visibleRows := m.visibleRows()
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+visibleRows {
		m.offset = m.cursor - visibleRows + 1
	}
}

func (m *SearchModel) doSearch() tea.Msg {
	query := search.Parse(m.input.Value())
	searcher := search.New(m.db.DB)
	results, err := searcher.Search(query, 100)
	if err != nil {
		return ErrorMsg{Err: err}
	}
	return searchResultsMsg{results: results}
}

type searchResultsMsg struct {
	results []search.Result
}

func (m *SearchModel) visibleRows() int {
	// Account for header (4 lines) + footer (2 lines) + input area (3 lines)
	// Each result takes ~3 lines (header + snippet + blank)
	availableHeight := m.height - 9
	rows := availableHeight / 3
	if rows < 3 {
		rows = 3
	}
	return rows
}

// SetSize sets the viewport size
func (m *SearchModel) SetSize(width, height int) {
	m.width = width
	m.height = height
	m.input.Width = width - 4
	if m.input.Width > 100 {
		m.input.Width = 100
	}
}

// View renders the search view
func (m *SearchModel) View() string {
	var b strings.Builder

	// Title
	b.WriteString(titleStyle.Render("Search Conversations"))
	b.WriteString("\n\n")

	// Search input with visual indicator
	if m.focused {
		b.WriteString("▶ ")
	} else {
		b.WriteString("  ")
	}
	b.WriteString(m.input.View())
	b.WriteString("\n\n")

	if m.loading {
		b.WriteString(subtitleStyle.Render("  Searching..."))
		b.WriteString("\n")
	} else if m.searched {
		if len(m.results) == 0 {
			b.WriteString(subtitleStyle.Render("  No results found."))
			b.WriteString("\n\n")
			b.WriteString(dimStyle.Render("  Try different keywords or filters"))
		} else {
			// Results header
			b.WriteString(subtitleStyle.Render(fmt.Sprintf("  %d results found", len(m.results))))
			if !m.focused {
				b.WriteString(subtitleStyle.Render(fmt.Sprintf(" • Selected: %d/%d", m.cursor+1, len(m.results))))
			}
			b.WriteString("\n\n")

			// Load all projects once so adapter DisplayNames surface in
			// the result rows instead of falling through to basename.
			// Best-effort — on lookup failure rows degrade to basename.
			projectsList, projectsErr := m.db.GetProjects("activity", 0)
			_ = projectsErr
			byPath := projectref.ProjectsByPath(projectsList)

			// Results list
			visibleRows := m.visibleRows()
			end := m.offset + visibleRows
			if end > len(m.results) {
				end = len(m.results)
			}

			for i := m.offset; i < end; i++ {
				r := m.results[i]
				isSelected := i == m.cursor && !m.focused

				// Selection indicator
				if isSelected {
					b.WriteString("▶ ")
				} else {
					b.WriteString("  ")
				}

				// Format result header line
				turnType := r.Turn.Type
				if turnType == "assistant" {
					turnType = "asst"
				}

				// Class A — short label in a compact cell. LabelFromPath
				// surfaces adapter DisplayName instead of always
				// synthesizing a bare Project stub. compact.Truncate
				// handles multibyte adapter labels correctly (rune
				// slicing, not byte slicing).
				project := compact.Truncate(projectref.LabelFromPath(r.ProjectPath, byPath), 25).Text

				model := r.Model
				if len(model) > 20 {
					// Extract just the model name (e.g., "opus" from "claude-opus-4-...")
					if idx := strings.Index(model, "-"); idx > 0 {
						parts := strings.Split(model, "-")
						if len(parts) >= 2 {
							model = parts[1]
						}
					}
				}

				headerLine := fmt.Sprintf("%-6s │ %-25s │ %-10s │ %s",
					turnType,
					project,
					model,
					r.Turn.Timestamp.Format("Jan 02 15:04"))

				if isSelected {
					b.WriteString(selectedStyle.Render(headerLine))
				} else {
					b.WriteString(normalStyle.Render(headerLine))
				}
				b.WriteString("\n")

				// Snippet (indented). A hit found through a stored tool
				// payload is labelled, because the text below is command
				// output or tool arguments rather than conversation.
				snippet := r.Snippet
				if r.MatchedToolName != "" {
					snippet = "[" + r.MatchedToolName + "] " + snippet
				}
				maxSnippetLen := m.width - 6
				if maxSnippetLen < 20 {
					maxSnippetLen = 20
				}
				if len(snippet) > maxSnippetLen {
					snippet = snippet[:maxSnippetLen-3] + "..."
				}
				snippet = strings.ReplaceAll(snippet, "\n", " ")

				if isSelected {
					b.WriteString("  ")
					b.WriteString(contentStyle.Render(snippet))
				} else {
					b.WriteString("  ")
					b.WriteString(dimStyle.Render(snippet))
				}
				b.WriteString("\n")
			}

			// Scroll indicator
			if len(m.results) > visibleRows {
				pct := 0
				if len(m.results) > 1 {
					pct = (m.cursor * 100) / (len(m.results) - 1)
				}
				b.WriteString(subtitleStyle.Render(fmt.Sprintf("\n  [%d%%] Showing %d-%d of %d",
					pct, m.offset+1, end, len(m.results))))
			}
		}
	} else {
		// Show search tips
		b.WriteString(dimStyle.Render("  Search syntax:"))
		b.WriteString("\n\n")
		b.WriteString(dimStyle.Render("  project:name     Filter by project path"))
		b.WriteString("\n")
		b.WriteString(dimStyle.Render("  model:opus       Filter by model name"))
		b.WriteString("\n")
		b.WriteString(dimStyle.Render("  tool:Bash        Sessions using specific tool"))
		b.WriteString("\n")
		b.WriteString(dimStyle.Render("  before:2024-01   Before date"))
		b.WriteString("\n")
		b.WriteString(dimStyle.Render("  after:2024-01    After date"))
		b.WriteString("\n")
		b.WriteString(dimStyle.Render("  source:name      Filter by source (e.g. claude-code, codex)"))
		b.WriteString("\n")
		b.WriteString(dimStyle.Render("  \"exact phrase\"   Exact phrase match"))
		b.WriteString("\n\n")
		b.WriteString(dimStyle.Render("  Example: project:myapp model:opus tool:Bash"))
	}

	// Footer with context-sensitive help
	b.WriteString("\n\n")
	if m.focused {
		// No "q: back" here: while the input has focus, q is a character.
		b.WriteString(helpStyle.Render("enter: search │ tab/↓: results │ esc: back │ ctrl+c: quit"))
	} else {
		// "/: query" is the way back to the query box. shift+tab and up-from-
		// the-top-row do it too, but there is no room for them at 80 columns —
		// the help overlay carries those.
		b.WriteString(helpStyle.Render("enter: open │ ↑/↓/pgup/pgdn: nav │ /: query │ esc/q: back │ ctrl+c: quit"))
	}

	return b.String()
}
