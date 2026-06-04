// ABOUTME: Markdown export functionality for sessions
// ABOUTME: Converts session conversations to readable markdown files

package export

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/2389-research/ccvault/pkg/models"
)

// MarkdownExporter exports sessions to markdown format
type MarkdownExporter struct {
	includeToolResults bool
	includeThinking    bool
	includeRawJSON     bool
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

func (e *MarkdownExporter) exportAssistantTurn(w io.Writer, t models.Turn) error {
	_, _ = fmt.Fprintf(w, "### 🤖 Assistant\n")
	_, _ = fmt.Fprintf(w, "*%s*\n\n", t.Timestamp.Format("15:04:05"))

	// Try to parse rich content from RawJSON
	if len(t.RawJSON) > 0 {
		if err := e.exportAssistantRichContent(w, t.RawJSON); err == nil {
			_, _ = fmt.Fprintf(w, "---\n\n")
			return nil
		}
	}

	// Fallback to plain content
	if t.Content != "" {
		_, _ = fmt.Fprintf(w, "%s\n\n", t.Content)
	}

	_, _ = fmt.Fprintf(w, "---\n\n")
	return nil
}

func (e *MarkdownExporter) exportAssistantRichContent(w io.Writer, rawJSON json.RawMessage) error {
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

	if err := json.Unmarshal(rawJSON, &raw); err != nil {
		return err
	}

	for _, c := range raw.Message.Content {
		switch c.Type {
		case "text":
			if c.Text != "" {
				_, _ = fmt.Fprintf(w, "%s\n\n", c.Text)
			}
		case "thinking":
			if c.Thinking != "" && e.includeThinking {
				_, _ = fmt.Fprintf(w, "<details>\n<summary>💭 Thinking</summary>\n\n%s\n\n</details>\n\n", c.Thinking)
			}
		case "tool_use":
			e.exportToolUse(w, c.Name, c.Input)
		}
	}

	return nil
}

func (e *MarkdownExporter) exportToolUse(w io.Writer, toolName string, input json.RawMessage) {
	_, _ = fmt.Fprintf(w, "#### 🔧 Tool: %s\n\n", toolName)

	if len(input) == 0 {
		return
	}

	var params map[string]interface{}
	if err := json.Unmarshal(input, &params); err != nil {
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
				lines := strings.Split(content, "\n")
				if len(lines) > 50 {
					preview := strings.Join(lines[:50], "\n")
					_, _ = fmt.Fprintf(w, "```\n%s\n... (%d more lines)\n```\n\n", preview, len(lines)-50)
				} else {
					_, _ = fmt.Fprintf(w, "```\n%s\n```\n\n", content)
				}
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
