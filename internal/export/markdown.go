// ABOUTME: Markdown export functionality for sessions
// ABOUTME: Converts session conversations to readable markdown files

package export

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/2389-research/ccvault/internal/db"
	"github.com/2389-research/ccvault/pkg/models"
)

// MarkdownExporter exports sessions to markdown format
type MarkdownExporter struct {
	includeToolResults bool
	includeThinking    bool
	includeRawJSON     bool

	// toolCalls holds the session's tool calls grouped by the turn that issued
	// them, in the order each turn made them, as read from tool_uses (#179).
	// It is where an assistant turn's calls come from: the stored rows are
	// filled identically by every source, where raw_json's block shape is not.
	//
	// Nil for a caller that supplies none, which exports a turn's prose and no
	// calls rather than failing — a caller with no database handy can still
	// export.
	toolCalls map[string][]db.ToolCall
}

// MarkdownOption configures the markdown exporter
type MarkdownOption func(*MarkdownExporter)

// WithToolResults includes tool results in the export
func WithToolResults(include bool) MarkdownOption {
	return func(e *MarkdownExporter) {
		e.includeToolResults = include
	}
}

// WithThinking includes thinking blocks in the export
func WithThinking(include bool) MarkdownOption {
	return func(e *MarkdownExporter) {
		e.includeThinking = include
	}
}

// WithRawJSON includes raw JSON in the export
func WithRawJSON(include bool) MarkdownOption {
	return func(e *MarkdownExporter) {
		e.includeRawJSON = include
	}
}

// WithToolCalls supplies the session's tool calls grouped by the turn that
// issued them, which is what an assistant turn's calls are rendered from.
//
// Read them with db.SessionToolCallsByTurn. Passing them in rather than having
// the exporter query keeps this package free of a database handle it would
// otherwise need only for this, and lets a caller that already holds the
// grouping — the conversation view does — hand over what it has.
func WithToolCalls(byTurn map[string][]db.ToolCall) MarkdownOption {
	return func(e *MarkdownExporter) {
		e.toolCalls = byTurn
	}
}

// NewMarkdownExporter creates a new markdown exporter
func NewMarkdownExporter(opts ...MarkdownOption) *MarkdownExporter {
	e := &MarkdownExporter{
		includeToolResults: true,
		includeThinking:    true,
		includeRawJSON:     false,
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// Export writes the session and turns to markdown format
func (e *MarkdownExporter) Export(w io.Writer, session *models.Session, turns []models.Turn, projectPath string) error {
	// Write header
	_, _ = fmt.Fprintf(w, "# Conversation: %s\n\n", session.ID)

	// Metadata section
	_, _ = fmt.Fprintf(w, "## Metadata\n\n")
	_, _ = fmt.Fprintf(w, "| Field | Value |\n")
	_, _ = fmt.Fprintf(w, "|-------|-------|\n")
	_, _ = fmt.Fprintf(w, "| **Session ID** | `%s` |\n", session.ID)
	if projectPath != "" {
		_, _ = fmt.Fprintf(w, "| **Project** | `%s` |\n", projectPath)
	}
	if session.Model != "" {
		_, _ = fmt.Fprintf(w, "| **Model** | %s |\n", session.Model)
	}
	_, _ = fmt.Fprintf(w, "| **Started** | %s |\n", session.StartedAt.Format("2006-01-02 15:04:05 MST"))
	if !session.EndedAt.IsZero() {
		_, _ = fmt.Fprintf(w, "| **Ended** | %s |\n", session.EndedAt.Format("2006-01-02 15:04:05 MST"))
		duration := session.EndedAt.Sub(session.StartedAt)
		_, _ = fmt.Fprintf(w, "| **Duration** | %s |\n", duration.Round(1e9).String())
	}
	_, _ = fmt.Fprintf(w, "| **Turns** | %d |\n", len(turns))
	if session.InputTokens > 0 || session.OutputTokens > 0 {
		_, _ = fmt.Fprintf(w, "| **Input Tokens** | %s |\n", formatTokens(session.InputTokens))
		_, _ = fmt.Fprintf(w, "| **Output Tokens** | %s |\n", formatTokens(session.OutputTokens))
		_, _ = fmt.Fprintf(w, "| **Total Tokens** | %s |\n", formatTokens(session.TotalTokens()))
	}
	if session.GitBranch != "" {
		_, _ = fmt.Fprintf(w, "| **Git Branch** | `%s` |\n", session.GitBranch)
	}
	if session.Source != "" {
		_, _ = fmt.Fprintf(w, "| **Source** | `%s` |\n", escapeTableCell(session.Source))
	}
	_, _ = fmt.Fprintf(w, "\n")

	// Conversation section
	_, _ = fmt.Fprintf(w, "## Conversation\n\n")

	for _, t := range turns {
		if err := e.exportTurn(w, t); err != nil {
			return fmt.Errorf("export turn %s: %w", t.ID, err)
		}
	}

	return nil
}

func (e *MarkdownExporter) exportTurn(w io.Writer, t models.Turn) error {
	switch t.Type {
	case "user":
		return e.exportUserTurn(w, t)
	case "assistant":
		return e.exportAssistantTurn(w, t)
	case "tool_result":
		if e.includeToolResults {
			return e.exportToolResult(w, t)
		}
	}
	return nil
}

func (e *MarkdownExporter) exportUserTurn(w io.Writer, t models.Turn) error {
	_, _ = fmt.Fprintf(w, "### 👤 User\n")
	_, _ = fmt.Fprintf(w, "*%s*\n\n", t.Timestamp.Format("15:04:05"))

	content := t.Content
	if content == "" && len(t.RawJSON) > 0 {
		content = extractUserContent(t.RawJSON)
	}

	if content != "" {
		_, _ = fmt.Fprintf(w, "%s\n\n", content)
	}

	_, _ = fmt.Fprintf(w, "---\n\n")
	return nil
}

// exportAssistantTurn writes an assistant turn: what it said, then what it
// called.
//
// The two halves come from different places on purpose (#179). Prose is read
// out of the turn's raw_json, because that is where the formatting survives —
// the `content` column holds a flattened summary. Tool calls are read out of
// tool_uses, because that is the one place every source agrees: codex records
// each call on a transcript line of its own, so a codex turn's calls are not
// in its raw_json at all, and pi writes a `toolCall` block the Claude Code
// shape does not have. Exporting calls from the stored rows makes this right
// for a source nobody has written yet, where a switch over block types writes
// nothing until somebody remembers to extend it.
//
// Calls are written after the prose rather than interleaved with it.
// Interleaving was only ever available for the one source whose blocks arrive
// in order, and an assistant turn says what it is about to do before it does
// it.
func (e *MarkdownExporter) exportAssistantTurn(w io.Writer, t models.Turn) error {
	_, _ = fmt.Fprintf(w, "### 🤖 Assistant\n")
	_, _ = fmt.Fprintf(w, "*%s*\n\n", t.Timestamp.Format("15:04:05"))

	e.exportAssistantProse(w, t)
	for _, call := range e.toolCalls[t.ID] {
		e.exportToolUse(w, call.ToolName, json.RawMessage(call.InputJSON))
	}

	_, _ = fmt.Fprintf(w, "---\n\n")
	return nil
}

// exportAssistantProse writes what an assistant turn said — its text and its
// thinking, not its calls.
//
// Two sources for one answer, and which one is used turns on whether the
// turn's raw_json carried a content block this knows. It did: write those
// blocks, and leave the `content` column alone even if no prose came out of
// them. A turn whose only block was a `tool_use` has `content` stored as
// ccvault's own summary of that call — "[Tool: Bash]" — and printing it
// directly above the call it summarises is noise.
//
// It did not: fall back to the `content` column. This is the half #179 turned
// on in the conversation view, and the defect this fixes in export. The old
// code consulted `content` only when raw_json was absent or unparseable, and
// codex's raw_json is neither — it parses perfectly into a shape with no
// `message` object, yielding zero blocks and an empty body under the turn's
// header, for all 7,035 codex assistant turns in the author's archive. The
// fallback has to fire when parsing succeeds and recognises nothing, which is
// also what lets an unrecognised future shape degrade to its stored text
// instead of to nothing.
func (e *MarkdownExporter) exportAssistantProse(w io.Writer, t models.Turn) {
	if len(t.RawJSON) > 0 && e.exportRecognisedProseBlocks(w, t.RawJSON) {
		return
	}

	if t.Content != "" {
		_, _ = fmt.Fprintf(w, "%s\n\n", t.Content)
	}
}

// exportRecognisedProseBlocks writes the prose blocks of a Claude Code shaped
// raw_json, reporting whether the shape was recognised at all.
//
// Recognised means a block type this switch knows appeared, empty or not —
// including tool_use, which is exported from tool_uses but still says the shape
// was understood. See the comment on exportAssistantProse for why that
// distinction is the one that matters.
func (e *MarkdownExporter) exportRecognisedProseBlocks(w io.Writer, rawJSON json.RawMessage) bool {
	var raw struct {
		Message struct {
			Content []struct {
				Type     string `json:"type"`
				Text     string `json:"text,omitempty"`
				Thinking string `json:"thinking,omitempty"`
			} `json:"content"`
		} `json:"message"`
	}

	if err := json.Unmarshal(rawJSON, &raw); err != nil {
		return false
	}

	recognised := false
	for _, c := range raw.Message.Content {
		switch c.Type {
		case "text":
			recognised = true
			if c.Text != "" {
				_, _ = fmt.Fprintf(w, "%s\n\n", c.Text)
			}
		case "thinking":
			recognised = true
			if c.Thinking != "" && e.includeThinking {
				_, _ = fmt.Fprintf(w, "<details>\n<summary>💭 Thinking</summary>\n\n%s\n\n</details>\n\n", c.Thinking)
			}
		case "tool_use":
			recognised = true
		}
	}

	return recognised
}

func (e *MarkdownExporter) exportToolUse(w io.Writer, toolName string, input json.RawMessage) {
	_, _ = fmt.Fprintf(w, "#### 🔧 Tool: %s\n\n", toolName)

	if len(input) == 0 {
		return
	}

	// Arguments that are not a JSON object are written as the text they are.
	// Most of the archive's codex calls are exactly that — 26,552 of 33,294 —
	// because codex's custom_tool_call carries a plain-text body: a JavaScript
	// snippet for `exec`, a diff for `apply_patch`. Dropping them, which is
	// what returning here used to do, answers "a command ran" without
	// answering which one.
	//
	// Previewed at the same 50 lines the Write renderer uses, that being this
	// file's convention for a body whose substance is the point rather than a
	// prompt to glance at. A line cut cannot split a rune, so no UTF-8
	// repair is needed here.
	var params map[string]interface{}
	if err := json.Unmarshal(input, &params); err != nil {
		_, _ = fmt.Fprintf(w, "```\n%s\n```\n\n", previewLines(string(input), 50))
		return
	}

	switch toolName {
	case "Bash":
		if cmd, ok := params["command"].(string); ok {
			_, _ = fmt.Fprintf(w, "```bash\n%s\n```\n\n", cmd)
		}
		if desc, ok := params["description"].(string); ok && desc != "" {
			_, _ = fmt.Fprintf(w, "> %s\n\n", desc)
		}

	case "Read":
		if fp, ok := params["file_path"].(string); ok {
			_, _ = fmt.Fprintf(w, "📄 Reading: `%s`\n\n", fp)
		}

	case "Write":
		if fp, ok := params["file_path"].(string); ok {
			_, _ = fmt.Fprintf(w, "📝 Writing: `%s`\n\n", fp)
			if content, ok := params["content"].(string); ok {
				_, _ = fmt.Fprintf(w, "```\n%s\n```\n\n", previewLines(content, 50))
			}
		}

	case "Edit":
		if fp, ok := params["file_path"].(string); ok {
			_, _ = fmt.Fprintf(w, "✏️ Editing: `%s`\n\n", fp)
		}
		if old, ok := params["old_string"].(string); ok {
			if len(old) > 200 {
				old = old[:200] + "..."
			}
			_, _ = fmt.Fprintf(w, "**Replace:**\n```\n%s\n```\n\n", old)
		}
		if newStr, ok := params["new_string"].(string); ok {
			if len(newStr) > 200 {
				newStr = newStr[:200] + "..."
			}
			_, _ = fmt.Fprintf(w, "**With:**\n```\n%s\n```\n\n", newStr)
		}

	case "Glob":
		if pattern, ok := params["pattern"].(string); ok {
			_, _ = fmt.Fprintf(w, "🔍 Pattern: `%s`\n\n", pattern)
		}

	case "Grep":
		if pattern, ok := params["pattern"].(string); ok {
			_, _ = fmt.Fprintf(w, "🔍 Search: `%s`\n\n", pattern)
		}
		if path, ok := params["path"].(string); ok {
			_, _ = fmt.Fprintf(w, "In: `%s`\n\n", path)
		}

	case "WebFetch":
		if url, ok := params["url"].(string); ok {
			_, _ = fmt.Fprintf(w, "🌐 Fetching: %s\n\n", url)
		}

	case "WebSearch":
		if query, ok := params["query"].(string); ok {
			_, _ = fmt.Fprintf(w, "🔍 Searching: \"%s\"\n\n", query)
		}

	case "Task":
		if desc, ok := params["description"].(string); ok {
			_, _ = fmt.Fprintf(w, "📋 Task: %s\n\n", desc)
		}
		if prompt, ok := params["prompt"].(string); ok {
			if len(prompt) > 500 {
				prompt = prompt[:500] + "..."
			}
			_, _ = fmt.Fprintf(w, "<details>\n<summary>Task Prompt</summary>\n\n%s\n\n</details>\n\n", prompt)
		}

	default:
		// Generic: show all params
		for k, v := range params {
			if str, ok := v.(string); ok && str != "" {
				if len(str) > 100 {
					str = str[:100] + "..."
				}
				_, _ = fmt.Fprintf(w, "- **%s**: %s\n", k, str)
			}
		}
		_, _ = fmt.Fprintf(w, "\n")
	}
}

func (e *MarkdownExporter) exportToolResult(w io.Writer, t models.Turn) error {
	if len(t.RawJSON) == 0 {
		return nil
	}

	var raw struct {
		ToolUseID string          `json:"toolUseId"`
		Data      json.RawMessage `json:"data"`
	}

	if err := json.Unmarshal(t.RawJSON, &raw); err != nil {
		return nil
	}

	if len(raw.Data) == 0 {
		return nil
	}

	// Try to extract output
	var data struct {
		Output   string `json:"output"`
		Content  string `json:"content"`
		Result   string `json:"result"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
		ExitCode int    `json:"exit_code"`
	}

	if err := json.Unmarshal(raw.Data, &data); err != nil {
		return nil
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
		return nil
	}

	_, _ = fmt.Fprintf(w, "#### 📤 Tool Result\n\n")

	// Truncate very long outputs
	lines := strings.Split(output, "\n")
	if len(lines) > 100 {
		output = strings.Join(lines[:100], "\n") + fmt.Sprintf("\n... (%d more lines)", len(lines)-100)
	}

	_, _ = fmt.Fprintf(w, "```\n%s\n```\n\n", output)
	return nil
}

// previewLines returns the first max lines of s, followed by a line saying how
// many were dropped. Shorter input is returned unchanged, with no notice — a
// notice claiming a truncation that did not happen is as misleading as a
// truncation with no notice.
func previewLines(s string, max int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= max {
		return s
	}
	return fmt.Sprintf("%s\n... (%d more lines)", strings.Join(lines[:max], "\n"), len(lines)-max)
}

// extractUserContent extracts text from user message raw JSON
func extractUserContent(rawJSON json.RawMessage) string {
	var raw struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}

	if err := json.Unmarshal(rawJSON, &raw); err != nil {
		return ""
	}

	// Try as string first
	var str string
	if err := json.Unmarshal(raw.Message.Content, &str); err == nil {
		return str
	}

	// Try as array of blocks
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text,omitempty"`
	}
	if err := json.Unmarshal(raw.Message.Content, &blocks); err == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}

	return ""
}

// escapeTableCell sanitizes a value for inclusion in a markdown table cell:
// pipes are escaped and any embedded newlines collapsed to spaces.
func escapeTableCell(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return s
}

// formatTokens formats a token count for display
func formatTokens(n int64) string {
	if n >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
	if n >= 1_000 {
		return fmt.Sprintf("%.1fK", float64(n)/1_000)
	}
	return fmt.Sprintf("%d", n)
}
