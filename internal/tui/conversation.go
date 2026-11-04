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
	db        *db.DB
	width     int
	height    int
	sessionID string
	session   *models.Session
	turns     []models.Turn
	viewport  viewport.Model
	loading   bool
	ready     bool
	// mdRenderer is built on first use by markdownRenderer, not in the
	// constructor — see the comment there. mdWrapWidth is the width it was
	// built for, so a resize rebuilds it and nothing else does.
	mdRenderer  *glamour.TermRenderer
	mdWrapWidth int
	statusMsg   string
	statusClear time.Time

	// toolCalls holds this session's tool calls grouped by the turn that issued
	// them, in the order each turn made them, read from tool_uses (#179). It is
	// where the view gets an assistant turn's calls: the stored rows are filled
	// identically by every source, where raw_json's block shape is not.
	//
	// Each call carries its own failure flag, which is why nothing here is
	// keyed on the provider's tool_use id any more. Matching on that id was the
	// cost of drawing the call out of raw_json and the failure out of the
	// table, and it left a call whose source minted no id unmarked — jeff
	// leaves it empty on 633 of 742. A call read from its own row has both.
	toolCalls map[string][]db.ToolCall
}

// NewConversationModel creates a new conversation model
func NewConversationModel(database *db.DB) *ConversationModel {
	return &ConversationModel{
		db:      database,
		loading: true,
	}
}

// markdownWrapWidth is the column the markdown renderer wraps at, derived from
// the terminal width with room for the view's indentation.
func (m *ConversationModel) markdownWrapWidth() int {
	wrapWidth := m.width - 4
	if wrapWidth < 20 {
		wrapWidth = 20 // Minimum reasonable wrap width
	}
	return wrapWidth
}

// markdownRenderer returns the renderer for the current wrap width, building
// it on first use.
//
// Construction is deferred because it is the one thing the conversation view
// does that touches terminal state, and it used to happen twice before the
// dashboard drew its first frame: once in the constructor, once in the
// SetSize that the startup WindowSizeMsg triggers. The dashboard needs no
// markdown renderer to draw itself, so nothing should build one until the
// conversation view is actually opened (#49). See markdownStyleName for what
// that construction can cost.
//
// Deferring it moves the construction onto the Update path, which Bubble Tea
// requires not to block — so the style must stay one that resolves without
// asking the terminal anything. Do not reach for glamour.WithAutoStyle() here.
func (m *ConversationModel) markdownRenderer() *glamour.TermRenderer {
	wrapWidth := m.markdownWrapWidth()
	if m.mdRenderer != nil && m.mdWrapWidth == wrapWidth {
		return m.mdRenderer
	}

	renderer, err := glamour.NewTermRenderer(
		markdownStyleOption(),
		glamour.WithWordWrap(wrapWidth),
	)
	if err != nil {
		// Keep whatever renderer we already had; renderMarkdown falls back to
		// plain wrapped text when there is none.
		return m.mdRenderer
	}
	m.mdRenderer = renderer
	m.mdWrapWidth = wrapWidth
	return renderer
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

	// What each turn called, read once here rather than per rendered turn, in
	// the same command that already fetches the turns — so opening a
	// conversation still costs one round of loading.
	toolCalls, err := m.db.SessionToolCallsByTurn(m.sessionID)
	if err != nil {
		return ErrorMsg{Err: err}
	}

	return conversationLoadedMsg{session: session, turns: turns, toolCalls: toolCalls}
}

type conversationLoadedMsg struct {
	session   *models.Session
	turns     []models.Turn
	toolCalls map[string][]db.ToolCall
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
	// The same grouping the view renders from, so an exported file says what
	// the screen said.
	exporter := export.NewMarkdownExporter(export.WithToolCalls(m.toolCalls))
	if err := exporter.Export(f, m.session, m.turns, projectPath); err != nil {
		return exportCompleteMsg{err: err}
	}

	return exportCompleteMsg{path: exportPath}
}

func (m *ConversationModel) copyToClipboard() tea.Msg {
	if len(m.turns) == 0 {
		return copyCompleteMsg{err: fmt.Errorf("no conversation to copy")}
	}

	if err := clipboard.WriteAll(m.plainTextTranscript()); err != nil {
		return copyCompleteMsg{err: fmt.Errorf("copy to clipboard: %w", err)}
	}

	return copyCompleteMsg{count: len(m.turns)}
}

// plainTextTranscript is the loaded conversation as unstyled text, which is
// what the clipboard gets.
//
// Separate from copyToClipboard so it can be asserted on without writing to
// the machine's clipboard during a test run.
//
// A type with no label of its own is labelled with the type as stored and
// copied if it carries content, the same rule renderConversation applies — see
// turnTypeLabel. Copying used to drop those turns, so pasting a nanoclaw
// session into a bug report lost the prompt that explained it.
func (m *ConversationModel) plainTextTranscript() string {
	var b strings.Builder
	for _, t := range m.turns {
		label, ok := turnTypeLabel(t)
		if !ok {
			continue
		}

		b.WriteString(label)
		b.WriteString(" ")
		b.WriteString(t.Timestamp.Format("15:04:05"))
		b.WriteString("\n")
		b.WriteString(t.Content)
		b.WriteString("\n\n")
	}
	return b.String()
}

// turnTypeLabel is the bracketed label a turn is shown under, and whether it is
// shown at all.
//
// The three named types have a label and a renderer of their own. Everything
// else is labelled with the type as stored — a reader needs to know what
// produced the text, and nothing here knows enough about a type it has no
// renderer for to rename it — and is shown only if it carries content.
//
// Keying the decision on content rather than on the type name is the point.
// docs/plans/2026-02-13-ccvault-design.md declared the turn types as user,
// assistant, tool_use, tool_result and progress; measured on the author's
// archive, tool_use and tool_result have no rows at all, while `system` and
// `attachment` are present in volume and were never declared. Both switches in
// this file were written from the declaration and had no default, so every
// undeclared type was dropped without a word.
//
// Content splits the archive cleanly where type names do not: 51,794 nanoclaw
// `system` turns carry content — each the agent prompt that explains why the
// session did anything — and the ~169,000 progress, attachment and
// claude-code system turns carry none. A switch extended one type name at a
// time would need telling about each, and #179's lesson was that being told is
// the part that does not happen.
func turnTypeLabel(t models.Turn) (string, bool) {
	switch t.Type {
	case "user":
		return "[USER]", true
	case "assistant":
		return "[ASSISTANT]", true
	case "tool_result":
		return "[TOOL RESULT]", true
	default:
		return "[" + strings.ToUpper(t.Type) + "]", t.Content != ""
	}
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
		m.toolCalls = msg.toolCalls
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
		case msg.String() == "a":
			// Expand this session's subagents. Nothing to expand is not an
			// error — it just doesn't navigate.
			if m.session != nil && m.session.SubagentCount > 0 {
				parent := m.session.ID
				return func() tea.Msg {
					return NavigateMsg{View: SessionsView, Data: SubagentsOf(parent)}
				}
			}
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

	// The renderer is not rebuilt here. markdownRenderer notices that the wrap
	// width moved and rebuilds on the next render — which keeps renderer
	// construction off the startup path, where the first WindowSizeMsg lands
	// before the dashboard's first frame (#49).
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
		// Subagent transcripts are hidden from the session list, so the
		// detail view is where both directions of the relationship surface:
		// how much work this session dispatched, and what dispatched it.
		if m.session.SubagentCount > 0 {
			parts = append(parts, fmt.Sprintf("%d subagents (a)", m.session.SubagentCount))
		}
		if m.session.ParentSessionID != "" {
			parts = append(parts, fmt.Sprintf("subagent of %s", m.session.ParentSessionID))
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
	help := fmt.Sprintf("↑/↓/pgup/pgdn: scroll • c: copy • e: export • %d%% • esc/q: back • ctrl+c: quit", scrollPercent)
	if m.session != nil && m.session.SubagentCount > 0 {
		help = fmt.Sprintf("↑/↓/pgup/pgdn: scroll • a: subagents • c: copy • e: export • %d%% • esc/q: back • ctrl+c: quit", scrollPercent)
	}
	b.WriteString(helpStyle.Render(help))

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

		default:
			// A type with no renderer of its own: show its stored content if it
			// has any, and nothing at all if it has none. See turnTypeLabel for
			// why the decision is content and not the type name.
			label, show := turnTypeLabel(t)
			if !show {
				continue
			}
			b.WriteString(secondaryStyle.Render(label))
			b.WriteString(" ")
			b.WriteString(timestampStyle.Render(t.Timestamp.Format("15:04:05")))
			b.WriteString("\n")
			b.WriteString(m.renderMarkdown(t.Content))
			b.WriteString("\n\n")
		}
	}

	return b.String()
}

// renderAssistantContent renders an assistant turn: what it said, then what it
// called.
//
// The two halves come from different places on purpose (#179). Prose is read
// out of the turn's raw_json, because that is where the formatting survives —
// the `content` column holds a flattened summary. Tool calls are read out of
// tool_uses, because that is the one place every source agrees: codex records
// each call on a transcript line of its own, so a codex turn's calls are not in
// its raw_json at all, and pi writes a `toolCall` block the Claude Code shape
// does not have. Rendering calls from the stored rows makes this right for a
// source nobody has written yet, where a switch over block types renders a
// blank line until somebody remembers to extend it.
//
// Calls render after the prose rather than interleaved with it. Interleaving
// was only ever available for the one source whose blocks arrive in order, and
// an assistant turn says what it is about to do before it does it.
func (m *ConversationModel) renderAssistantContent(t models.Turn) string {
	parts := m.renderAssistantProse(t)
	for _, call := range m.toolCalls[t.ID] {
		parts = append(parts, m.formatToolUseBlock(call.ToolName, json.RawMessage(call.InputJSON), call.Failed))
	}
	return strings.Join(parts, "\n")
}

// renderAssistantProse renders what an assistant turn said — its text and its
// thinking, not its calls.
//
// Two sources for one answer, and which one is used turns on whether the
// turn's raw_json carried a content block this knows. It did: render those
// blocks, and leave the `content` column alone even if no prose came out of
// them. A turn whose only block was a `tool_use` has `content` stored as
// ccvault's own summary of that call — "[Tool: Bash]" — and printing it
// directly above the call it summarises is noise.
//
// It did not: fall back to the `content` column. This is the half #179 turned
// on. The old code consulted `content` only when raw_json was absent or
// unparseable, and codex's raw_json is neither — it parses perfectly into a
// shape with no `message` object, yielding zero blocks and an empty line under
// the turn's header, for all 7,035 codex assistant turns in the author's
// archive. The fallback has to fire when parsing succeeds and recognises
// nothing, which is also what lets an unrecognised future shape degrade to its
// stored text instead of to nothing.
func (m *ConversationModel) renderAssistantProse(t models.Turn) []string {
	var parts []string

	if len(t.RawJSON) > 0 {
		// Parse raw JSON for the prose blocks this view knows.
		var raw struct {
			Message struct {
				Content []struct {
					Type     string `json:"type"`
					Text     string `json:"text,omitempty"`
					Thinking string `json:"thinking,omitempty"`
				} `json:"content"`
			} `json:"message"`
		}

		if err := json.Unmarshal(t.RawJSON, &raw); err == nil {
			// recognised means a block type this switch knows appeared, empty
			// or not — including tool_use, which is rendered from tool_uses
			// but still says the shape was understood. See the comment above
			// on why that distinction is the one that matters.
			recognised := false
			for _, c := range raw.Message.Content {
				switch c.Type {
				case "text":
					recognised = true
					if c.Text != "" {
						text := c.Text
						if len(text) > 2000 {
							text = text[:2000] + "\n... (truncated)"
						}
						parts = append(parts, m.renderMarkdown(text))
					}
				case "thinking":
					recognised = true
					if c.Thinking != "" {
						thinking := c.Thinking
						if len(thinking) > 500 {
							thinking = thinking[:500] + "..."
						}
						parts = append(parts, thinkingStyle.Render("💭 "+wrapText(thinking, m.width-6)))
					}
				case "tool_use":
					recognised = true
				}
			}
			if recognised {
				return parts
			}
		}
	}

	if t.Content == "" {
		return parts
	}
	content := t.Content
	if len(content) > 2000 {
		content = content[:2000] + "\n... (truncated)"
	}
	return append(parts, m.renderMarkdown(content))
}

// failedCallMarker is what a failed call is labelled with. Text rather than
// colour alone: the view is read over SSH, piped, and copied to the clipboard,
// and a mark that survives none of those is not a mark.
const failedCallMarker = "✗ FAILED"

// formatToolUseBlock formats a tool use with input details, labelling it when
// the call's result reported a failure (#83).
//
// The label goes on the call's own header line, not on the turn, because one
// assistant turn renders every call it made into one block — a turn-level mark
// would point a reader at whichever call they noticed first.
func (m *ConversationModel) formatToolUseBlock(toolName string, input json.RawMessage, failed bool) string {
	var b strings.Builder

	b.WriteString(toolStyle.Render(fmt.Sprintf("🔧 %s", toolName)))
	if failed {
		b.WriteString(" ")
		b.WriteString(errorStyle.Render(failedCallMarker))
	}
	b.WriteString("\n")

	if len(input) == 0 {
		return b.String()
	}

	// Arguments that are not a JSON object are rendered as the text they are.
	// Most of the archive's codex calls are exactly that — 26,552 of 33,294 —
	// because codex's custom_tool_call carries a plain-text body: a JavaScript
	// snippet for `exec`, a diff for `apply_patch`. Dropping them would answer
	// "a command ran" without answering which one.
	//
	// Truncated at the same 500 the thinking block uses. These bodies average
	// 666 bytes and reach 32,469, and the whole of one is in the turn's
	// raw_json for a reader who needs it.
	//
	// ToValidUTF8 because this is the one truncation in the file that cuts
	// bytes nobody validated — a diff or a shell command, where a 500-byte cut
	// can land inside a multi-byte rune and leave the terminal a fragment to
	// draw.
	var params map[string]interface{}
	if err := json.Unmarshal(input, &params); err != nil {
		text := string(input)
		if len(text) > 500 {
			text = strings.ToValidUTF8(text[:500], "") + "..."
		}
		b.WriteString(codeStyle.Render(wrapText(text, m.width-6)))
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

// renderToolResult renders what a tool returned.
//
// Three sources, in order of how likely they are to hold the answer. The
// Claude Code shape first: a tool result is a message whose content blocks are
// tool_result blocks, and #185 is what made turns of this type exist at all —
// before it the type had no rows and this renderer had never run. Then the
// $.toolUseId/$.data shape, which is what this function used to read on its
// own and which the design doc described; no source in the repo writes it, so
// it is tried second rather than first. Then the turn's stored content, which
// is a bounded preview and is all a turn has left when its oversized line was
// replaced by a placeholder.
//
// Falling back to content rather than returning the empty string is what keeps
// such a turn in the view at all: renderConversation skips a tool_result whose
// render comes out empty, so an empty return deletes the turn from the
// conversation instead of showing it with nothing under it.
func (m *ConversationModel) renderToolResult(t models.Turn) string {
	output := toolResultOutput(t.RawJSON)
	if output == "" {
		output = t.Content
	}
	if output == "" {
		return ""
	}

	// Shown unconditionally, with no toggle, which #185 makes a decision
	// rather than an inheritance: this renderer had no rows until the retag.
	// The view already shows every other type that carries text (#184), so
	// suppressing the one type that holds what the tools answered would be the
	// inconsistency. Measured, the results take 20,478 characters of a real
	// nanoclaw session against 14,516 in its other turns' content, and 42,820
	// against 67,233 across three claude-code sessions — a substantial share of
	// a view whose subject is what the agent did. A keybinding to hide them is
	// human-facing surface nobody has asked for, and nothing yet measures the
	// view as too noisy to read.
	//
	// Truncated at 1,000 characters — the existing cap, left where it was.
	// Counted as characters rather than bytes so a cut cannot land inside a
	// multi-byte rune and leave the terminal a fragment to draw. ToValidUTF8
	// besides, because the body is tool output nobody validated. The whole of
	// it is in the turn's raw_json for a reader who needs it.
	runes := []rune(output)
	if len(runes) > 1000 {
		output = string(runes[:1000]) + "\n... (truncated)"
	}
	output = strings.ToValidUTF8(output, "")

	return codeStyle.Render(wrapText(output, m.width-4))
}

// toolResultOutput reads a tool result's body out of a turn's raw JSON, empty
// when neither known shape carries one.
func toolResultOutput(rawJSON json.RawMessage) string {
	if len(rawJSON) == 0 {
		return ""
	}

	if body := claudeToolResultBody(rawJSON); body != "" {
		return body
	}
	return legacyToolResultBody(rawJSON)
}

// claudeToolResultBody joins the bodies of a message's tool_result blocks.
//
// Image payloads are dropped and their captions kept. This is a view and not
// an index, so the partial that issue #28 refuses to store is the right thing
// to show: 1-2 MB of base64 answers nothing, and "Screenshot taken at
// 1440x700" answers what the call did.
func claudeToolResultBody(rawJSON json.RawMessage) string {
	var raw struct {
		Message struct {
			Content []struct {
				Type    string          `json:"type"`
				Content json.RawMessage `json:"content"`
			} `json:"content"`
		} `json:"message"`
	}

	if err := json.Unmarshal(rawJSON, &raw); err != nil {
		return ""
	}

	var parts []string
	for _, block := range raw.Message.Content {
		if block.Type != "tool_result" {
			continue
		}
		if body := resultBlockText(block.Content); body != "" {
			parts = append(parts, body)
		}
	}
	return strings.Join(parts, "\n")
}

// resultBlockText reads one tool_result block's content, which the transcripts
// write either as a JSON string or as an array of content blocks.
func resultBlockText(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}

	var str string
	if err := json.Unmarshal(content, &str); err == nil {
		return str
	}

	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &blocks); err != nil {
		return ""
	}

	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// legacyToolResultBody reads the $.toolUseId/$.data shape the design doc
// declared for a tool_result turn. Kept because it is what this renderer was
// written against; no source in the repo emits it.
func legacyToolResultBody(rawJSON json.RawMessage) string {
	var raw struct {
		ToolUseID string          `json:"toolUseId"`
		Data      json.RawMessage `json:"data"`
	}

	if err := json.Unmarshal(rawJSON, &raw); err != nil || len(raw.Data) == 0 {
		return ""
	}

	var data struct {
		Output   string `json:"output"`
		Content  string `json:"content"`
		Result   string `json:"result"`
		ExitCode int    `json:"exit_code"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
	}

	if err := json.Unmarshal(raw.Data, &data); err != nil {
		// A data object this does not understand: show it as the text it is.
		return string(raw.Data)
	}

	switch {
	case data.Output != "":
		return data.Output
	case data.Stdout != "":
		if data.Stderr != "" {
			return data.Stdout + "\n" + data.Stderr
		}
		return data.Stdout
	case data.Content != "":
		return data.Content
	case data.Result != "":
		return data.Result
	}
	return ""
}

// renderMarkdown renders markdown text with glamour
func (m *ConversationModel) renderMarkdown(text string) string {
	if text == "" {
		return wrapText(text, m.width-4)
	}

	renderer := m.markdownRenderer()
	if renderer == nil {
		return wrapText(text, m.width-4)
	}

	// Render markdown
	rendered, err := renderer.Render(text)
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
