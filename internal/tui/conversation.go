// ABOUTME: Conversation view for ccvault TUI
// ABOUTME: Shows a session's conversation with scrolling

package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/2389-research/ccvault/internal/db"
	"github.com/2389-research/ccvault/internal/export"
	"github.com/2389-research/ccvault/pkg/models"
	"github.com/atotto/clipboard"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
)

// ConversationModel holds conversation view state
type ConversationModel struct {
	db          *db.DB
	width       int
	height      int
	sessionID   string
	session     *models.Session
	turns       []models.Turn
	viewport    viewport.Model
	loading     bool
	ready       bool
	mdRenderer  *glamour.TermRenderer
	statusMsg   string
	statusClear time.Time
}

// NewConversationModel creates a new conversation model
func NewConversationModel(database *db.DB) *ConversationModel {
	// Create markdown renderer with dark style
	renderer, _ := glamour.NewTermRenderer(
		glamour.WithAutoStyle(),
		glamour.WithWordWrap(80),
	)

	return &ConversationModel{
		db:         database,
		loading:    true,
		mdRenderer: renderer,
	}
}

// SetSession sets the session to display
func (m *ConversationModel) SetSession(sessionID string) {
	m.sessionID = sessionID
	m.ready = false
}

// Init loads conversation data
func (m *ConversationModel) Init() tea.Cmd {
	return m.loadConversation
}

func (m *ConversationModel) loadConversation() tea.Msg {
	session, err := m.db.GetSession(m.sessionID)
	if err != nil {
		return ErrorMsg{Err: err}
	}

	turns, err := m.db.GetTurns(m.sessionID)
	if err != nil {
		return ErrorMsg{Err: err}
	}

	return conversationLoadedMsg{session: session, turns: turns}
}

type conversationLoadedMsg struct {
	session *models.Session
	turns   []models.Turn
}

type exportCompleteMsg struct {
	path string
	err  error
}

type copyCompleteMsg struct {
	count int
	err   error
}

func (m *ConversationModel) exportSession() tea.Msg {
	if m.session == nil {
		return exportCompleteMsg{err: fmt.Errorf("no session loaded")}
	}

	// Get project path for metadata
	var projectPath string
	if m.session.ProjectID > 0 {
		project, err := m.db.GetProject(m.session.ProjectID)
		if err == nil && project != nil {
			projectPath = project.Path
		}
	}

	// Create filename with timestamp
	timestamp := m.session.StartedAt.Format("2006-01-02_150405")
	shortID := m.session.ID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}
	filename := fmt.Sprintf("session_%s_%s.md", timestamp, shortID)

	// Export to current directory or home directory
	exportPath := filename
	if cwd, err := os.Getwd(); err == nil {
		exportPath = filepath.Join(cwd, filename)
	}

	// Create file
	f, err := os.Create(exportPath)
	if err != nil {
		// Try home directory as fallback
		home, _ := os.UserHomeDir()
		exportPath = filepath.Join(home, filename)
		f, err = os.Create(exportPath)
		if err != nil {
			return exportCompleteMsg{err: fmt.Errorf("create file: %w", err)}
		}
	}
	defer func() { _ = f.Close() }()

	// Export
	exporter := export.NewMarkdownExporter()
	if err := exporter.Export(f, m.session, m.turns, projectPath); err != nil {
		return exportCompleteMsg{err: err}
	}

	return exportCompleteMsg{path: exportPath}
}

func (m *ConversationModel) copyToClipboard() tea.Msg {
	if len(m.turns) == 0 {
		return copyCompleteMsg{err: fmt.Errorf("no conversation to copy")}
	}

	// Build plain text content
	var b strings.Builder
	for _, t := range m.turns {
		switch t.Type {
		case "user":
			b.WriteString("[USER] ")
			b.WriteString(t.Timestamp.Format("15:04:05"))
			b.WriteString("\n")
			b.WriteString(t.Content)
			b.WriteString("\n\n")
		case "assistant":
			b.WriteString("[ASSISTANT] ")
			b.WriteString(t.Timestamp.Format("15:04:05"))
			b.WriteString("\n")
			b.WriteString(t.Content)
			b.WriteString("\n\n")
		case "tool_result":
			b.WriteString("[TOOL RESULT] ")
			b.WriteString(t.Timestamp.Format("15:04:05"))
			b.WriteString("\n")
			b.WriteString(t.Content)
			b.WriteString("\n\n")
		}
	}

	if err := clipboard.WriteAll(b.String()); err != nil {
		return copyCompleteMsg{err: fmt.Errorf("copy to clipboard: %w", err)}
	}

	return copyCompleteMsg{count: len(m.turns)}
}

// Update handles conversation view events
func (m *ConversationModel) Update(msg tea.Msg) tea.Cmd {
	// Clear status message after timeout
	if m.statusMsg != "" && time.Now().After(m.statusClear) {
		m.statusMsg = ""
	}

	switch msg := msg.(type) {
	case conversationLoadedMsg:
		m.session = msg.session
		m.turns = msg.turns
		m.loading = false
		m.initViewport()
		return nil

	case exportCompleteMsg:
		if msg.err != nil {
			m.statusMsg = fmt.Sprintf("Export failed: %v", msg.err)
		} else {
			m.statusMsg = fmt.Sprintf("Exported to %s", msg.path)
		}
		m.statusClear = time.Now().Add(5 * time.Second)
		return nil

	case copyCompleteMsg:
		if msg.err != nil {
			m.statusMsg = fmt.Sprintf("Copy failed: %v", msg.err)
		} else {
			m.statusMsg = fmt.Sprintf("Copied %d turns to clipboard", msg.count)
		}
		m.statusClear = time.Now().Add(3 * time.Second)
		return nil

	case tea.KeyMsg:
		switch {
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
			return m.loadConversation
		case msg.String() == "e" || msg.String() == "x":
			return m.exportSession
		case msg.String() == "c":
			return m.copyToClipboard
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

func (m *ConversationModel) initViewport() {
	headerHeight := 5
	footerHeight := 2
	viewportHeight := m.height - headerHeight - footerHeight
	if viewportHeight < 5 {
		viewportHeight = 5
	}

	m.viewport = viewport.New(m.width, viewportHeight)
	m.viewport.SetContent(m.renderConversation())
	m.ready = true
}

// SetSize sets the viewport size
func (m *ConversationModel) SetSize(width, height int) {
	m.width = width
	m.height = height

	// Update markdown renderer with new width
	wrapWidth := width - 4
	if wrapWidth < 20 {
		wrapWidth = 20 // Minimum reasonable wrap width
	}
	renderer, err := glamour.NewTermRenderer(
		glamour.WithAutoStyle(),
		glamour.WithWordWrap(wrapWidth),
	)
	if err == nil {
		m.mdRenderer = renderer
	}

	if m.ready {
		m.initViewport()
	}
}

// View renders the conversation
func (m *ConversationModel) View() string {
	if m.loading {
		return titleStyle.Render("Loading conversation...")
	}

	var b strings.Builder

	// Header
	if m.session != nil {
		b.WriteString(titleStyle.Render("Conversation"))
		b.WriteString("\n")
		parts := []string{
			m.session.StartedAt.Format("2006-01-02 15:04"),
			fmt.Sprintf("%d turns", len(m.turns)),
		}
		if m.session.Model != "" {
			parts = append(parts, m.session.Model)
		}
		if m.session.Source != "" {
			parts = append(parts, m.session.Source)
		}
		b.WriteString(subtitleStyle.Render(strings.Join(parts, " • ")))
		b.WriteString("\n\n")
	}

	// Viewport
	if m.ready {
		b.WriteString(m.viewport.View())
	}

	// Footer
	b.WriteString("\n")

	// Show status message if present
	if m.statusMsg != "" {
		b.WriteString(secondaryStyle.Render(m.statusMsg))
		b.WriteString("\n")
	}

	scrollPercent := 0
	if m.ready {
		scrollPercent = int(m.viewport.ScrollPercent() * 100)
	}
	b.WriteString(helpStyle.Render(fmt.Sprintf("↑/↓: scroll • pgup/pgdn: page • c: copy • e: export • %d%% • esc: back", scrollPercent)))

	return b.String()
}

// renderConversation creates the formatted conversation content
func (m *ConversationModel) renderConversation() string {
	var b strings.Builder

	for _, t := range m.turns {
		switch t.Type {
		case "user":
			b.WriteString(userStyle.Render("[USER]"))
			b.WriteString(" ")
			b.WriteString(timestampStyle.Render(t.Timestamp.Format("15:04:05")))
			b.WriteString("\n")
			content := m.renderMarkdown(t.Content)
			b.WriteString(content)
			b.WriteString("\n\n")

		case "assistant":
			b.WriteString(assistantStyle.Render("[ASSISTANT]"))
			b.WriteString(" ")
			b.WriteString(timestampStyle.Render(t.Timestamp.Format("15:04:05")))
			b.WriteString("\n")
			// Render with tool details from raw JSON
			content := m.renderAssistantContent(t)
			b.WriteString(content)
			b.WriteString("\n\n")

		case "tool_result":
			// Show tool results with details
			result := m.renderToolResult(t)
			if result != "" {
				b.WriteString(toolStyle.Render("[TOOL RESULT]"))
				b.WriteString(" ")
				b.WriteString(timestampStyle.Render(t.Timestamp.Format("15:04:05")))
				b.WriteString("\n")
				b.WriteString(result)
				b.WriteString("\n\n")
			}
		}
	}

	return b.String()
}

// renderAssistantContent renders assistant message with tool details
func (m *ConversationModel) renderAssistantContent(t models.Turn) string {
	if len(t.RawJSON) == 0 {
		content := t.Content
		if len(content) > 2000 {
			content = content[:2000] + "\n... (truncated)"
		}
		return m.renderMarkdown(content)
	}

	// Parse raw JSON for detailed tool info
	var raw struct {
		Message struct {
			Content []struct {
				Type     string          `json:"type"`
				Text     string          `json:"text,omitempty"`
				Thinking string          `json:"thinking,omitempty"`
				Name     string          `json:"name,omitempty"`
				Input    json.RawMessage `json:"input,omitempty"`
			} `json:"content"`
		} `json:"message"`
	}

	if err := json.Unmarshal(t.RawJSON, &raw); err != nil {
		content := t.Content
		if len(content) > 2000 {
			content = content[:2000] + "\n... (truncated)"
		}
		return m.renderMarkdown(content)
	}

	var parts []string
	for _, c := range raw.Message.Content {
		switch c.Type {
		case "text":
			if c.Text != "" {
				text := c.Text
				if len(text) > 2000 {
					text = text[:2000] + "\n... (truncated)"
				}
				parts = append(parts, m.renderMarkdown(text))
			}
		case "thinking":
			if c.Thinking != "" {
				thinking := c.Thinking
				if len(thinking) > 500 {
					thinking = thinking[:500] + "..."
				}
				parts = append(parts, thinkingStyle.Render("💭 "+wrapText(thinking, m.width-6)))
			}
		case "tool_use":
			toolBlock := m.formatToolUseBlock(c.Name, c.Input)
			parts = append(parts, toolBlock)
		}
	}

	return strings.Join(parts, "\n")
}

// formatToolUseBlock formats a tool use with input details
func (m *ConversationModel) formatToolUseBlock(toolName string, input json.RawMessage) string {
	var b strings.Builder

	b.WriteString(toolStyle.Render(fmt.Sprintf("🔧 %s", toolName)))
	b.WriteString("\n")

	if len(input) == 0 {
		return b.String()
	}

	var params map[string]interface{}
	if err := json.Unmarshal(input, &params); err != nil {
		return b.String()
	}

	// Format input based on tool type
	switch toolName {
	case "Bash":
		if cmd, ok := params["command"].(string); ok {
			b.WriteString(codeStyle.Render("$ " + wrapText(cmd, m.width-6)))
		}
		if desc, ok := params["description"].(string); ok && desc != "" {
			b.WriteString("\n")
			b.WriteString(dimStyle.Render("# " + desc))
		}
	case "Read":
		if fp, ok := params["file_path"].(string); ok {
			b.WriteString(pathStyle.Render("📄 " + fp))
		}
	case "Write":
		if fp, ok := params["file_path"].(string); ok {
			b.WriteString(pathStyle.Render("📝 " + fp))
			if content, ok := params["content"].(string); ok {
				lines := strings.Split(content, "\n")
				b.WriteString(dimStyle.Render(fmt.Sprintf(" (%d lines)", len(lines))))
			}
		}
	case "Edit":
		if fp, ok := params["file_path"].(string); ok {
			b.WriteString(pathStyle.Render("✏️  " + fp))
		}
		if old, ok := params["old_string"].(string); ok {
			preview := old
			if len(preview) > 100 {
				preview = preview[:100] + "..."
			}
			preview = strings.ReplaceAll(preview, "\n", "↵")
			b.WriteString("\n")
			b.WriteString(dimStyle.Render("- " + preview))
		}
	case "Glob":
		if pattern, ok := params["pattern"].(string); ok {
			b.WriteString(codeStyle.Render(pattern))
		}
	case "Grep":
		if pattern, ok := params["pattern"].(string); ok {
			b.WriteString(codeStyle.Render("/" + pattern + "/"))
		}
		if path, ok := params["path"].(string); ok {
			b.WriteString(" in ")
			b.WriteString(pathStyle.Render(path))
		}
	case "WebFetch":
		if url, ok := params["url"].(string); ok {
			b.WriteString(linkStyle.Render("🌐 " + url))
		}
	case "WebSearch":
		if query, ok := params["query"].(string); ok {
			b.WriteString(codeStyle.Render("🔍 \"" + query + "\""))
		}
	case "Task":
		if desc, ok := params["description"].(string); ok {
			b.WriteString(dimStyle.Render("📋 " + desc))
		}
		if prompt, ok := params["prompt"].(string); ok {
			preview := prompt
			if len(preview) > 200 {
				preview = preview[:200] + "..."
			}
			b.WriteString("\n")
			b.WriteString(dimStyle.Render(wrapText(preview, m.width-6)))
		}
	default:
		// Generic parameter display
		for k, v := range params {
			if str, ok := v.(string); ok && str != "" {
				if len(str) > 100 {
					str = str[:100] + "..."
				}
				b.WriteString(dimStyle.Render(fmt.Sprintf("%s: %s", k, str)))
				b.WriteString("\n")
			}
		}
	}

	return b.String()
}

// renderToolResult renders a tool result with output
func (m *ConversationModel) renderToolResult(t models.Turn) string {
	if len(t.RawJSON) == 0 {
		return t.Content
	}

	var raw struct {
		ToolUseID string          `json:"toolUseId"`
		Data      json.RawMessage `json:"data"`
	}

	if err := json.Unmarshal(t.RawJSON, &raw); err != nil {
		return t.Content
	}

	if len(raw.Data) == 0 {
		return ""
	}

	// Try to extract output from data
	var data struct {
		Output   string `json:"output"`
		Content  string `json:"content"`
		Result   string `json:"result"`
		ExitCode int    `json:"exit_code"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
	}

	if err := json.Unmarshal(raw.Data, &data); err != nil {
		// Just show truncated raw data
		result := string(raw.Data)
		if len(result) > 500 {
			result = result[:500] + "..."
		}
		return dimStyle.Render(wrapText(result, m.width-4))
	}

	var output string
	switch {
	case data.Output != "":
		output = data.Output
	case data.Stdout != "":
		output = data.Stdout
		if data.Stderr != "" {
			output += "\n" + data.Stderr
		}
	case data.Content != "":
		output = data.Content
	case data.Result != "":
		output = data.Result
	}

	if output == "" {
		return ""
	}

	if len(output) > 1000 {
		output = output[:1000] + "\n... (truncated)"
	}

	return codeStyle.Render(wrapText(output, m.width-4))
}

// renderMarkdown renders markdown text with glamour
func (m *ConversationModel) renderMarkdown(text string) string {
	if m.mdRenderer == nil || text == "" {
		return wrapText(text, m.width-4)
	}

	// Render markdown
	rendered, err := m.mdRenderer.Render(text)
	if err != nil {
		return wrapText(text, m.width-4)
	}

	// Trim trailing newlines that glamour adds
	return strings.TrimRight(rendered, "\n")
}

// wrapText wraps text to the specified width
func wrapText(text string, width int) string {
	if width <= 0 {
		width = 80
	}

	var result strings.Builder
	lines := strings.Split(text, "\n")

	for i, line := range lines {
		if i > 0 {
			result.WriteString("\n")
		}

		// Simple word wrapping
		words := strings.Fields(line)
		if len(words) == 0 {
			continue
		}

		currentLine := words[0]
		for _, word := range words[1:] {
			if len(currentLine)+1+len(word) <= width {
				currentLine += " " + word
			} else {
				result.WriteString(currentLine)
				result.WriteString("\n")
				currentLine = word
			}
		}
		result.WriteString(currentLine)
	}

	return result.String()
}
