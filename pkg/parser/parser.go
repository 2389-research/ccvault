// ABOUTME: JSONL parser for Claude Code conversation files
// ABOUTME: Converts raw JSONL entries into structured Turn objects

package parser

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/2389-research/ccvault/pkg/models"
)

// ParseSession reads a session JSONL file and returns all turns
func ParseSession(path string) ([]models.Turn, *models.Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open session file: %w", err)
	}
	defer func() { _ = f.Close() }()

	return ParseSessionReader(f, path)
}

// ParseSessionReader parses a session from an io.Reader.
//
// Uses bufio.Reader.ReadBytes rather than bufio.Scanner so per-line size is
// unbounded. Claude Code embeds base64-encoded PDF pages inside single JSONL
// lines (see issue #11), which routinely exceeds any fixed token cap and, with
// a Scanner, aborts the whole file. Mirrors the reader pattern used by the
// Codex adapter (pkg/adapter/codex + pkg/adapter/util.go:ReadLine); duplicated
// here rather than shared because pkg/adapter wraps pkg/parser, not vice versa.
func ParseSessionReader(r io.Reader, sourcePath string) ([]models.Turn, *models.Session, error) {
	var turns []models.Turn
	session := &models.Session{
		SourceFile: sourcePath,
	}

	reader := bufio.NewReader(r)

	for {
		line, readErr := readLine(reader)
		if len(line) > 0 {
			turn, raw, parseErr := parseTurnInternal(line)
			if parseErr == nil && turn != nil {
				turns = append(turns, *turn)
				updateSessionMetadata(session, turn, raw)
			}
			// parseErr on an individual line is silently skipped — matches
			// prior behavior. Skipped-line accounting lands in a follow-up.
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return nil, nil, fmt.Errorf("read session file: %w", readErr)
		}
	}

	// Set session ID from first turn if available
	if len(turns) > 0 {
		session.ID = turns[0].SessionID
	}

	// Set end time from last turn
	if len(turns) > 0 {
		session.EndedAt = turns[len(turns)-1].Timestamp
	}

	session.TurnCount = len(turns)

	return turns, session, nil
}

// readLine returns the next newline-terminated line from r with trailing CR/LF
// stripped. A non-nil error (including io.EOF) may be paired with a non-empty
// final line when the file does not end with a newline. There is no per-line
// size cap.
func readLine(r *bufio.Reader) ([]byte, error) {
	line, err := r.ReadBytes('\n')
	if len(line) > 0 && line[len(line)-1] == '\n' {
		line = line[:len(line)-1]
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
	}
	return line, err
}

// ParseTurn parses a single JSONL line into a Turn
func ParseTurn(data []byte) (*models.Turn, error) {
	turn, _, err := parseTurnInternal(data)
	return turn, err
}

// parseTurnInternal parses a JSONL line and returns both the Turn and the
// intermediate RawTurn so callers can reuse it without re-unmarshalling.
func parseTurnInternal(data []byte) (*models.Turn, *models.RawTurn, error) {
	var raw models.RawTurn
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, nil, fmt.Errorf("unmarshal turn: %w", err)
	}

	// Skip entries without UUID (like file-history-snapshot)
	if raw.UUID == "" {
		return nil, &raw, nil
	}

	// Parse timestamp
	ts, err := parseTimestamp(raw.Timestamp)
	if err != nil {
		ts = time.Now() // Fallback
	}

	turn := &models.Turn{
		ID:        raw.UUID,
		SessionID: raw.SessionID,
		ParentID:  raw.ParentUUID,
		Type:      raw.Type,
		Timestamp: ts,
		RawJSON:   data,
	}

	// Extract content based on type
	turn.Content = extractContent(raw)

	// Extract token usage for assistant messages
	if raw.Type == "assistant" && raw.Message != nil {
		var msg models.RawAssistantMessage
		if err := json.Unmarshal(raw.Message, &msg); err == nil {
			if msg.Usage != nil {
				turn.InputTokens = msg.Usage.InputTokens
				turn.OutputTokens = msg.Usage.OutputTokens
			}
		}
	}

	return turn, &raw, nil
}

// extractContent extracts searchable text content from a turn
func extractContent(raw models.RawTurn) string {
	switch raw.Type {
	case "user":
		if raw.Message != nil {
			var msg models.RawUserMessage
			if err := json.Unmarshal(raw.Message, &msg); err == nil {
				return extractUserContent(msg.Content)
			}
		}
	case "assistant":
		if raw.Message != nil {
			var msg models.RawAssistantMessage
			if err := json.Unmarshal(raw.Message, &msg); err == nil {
				var parts []string
				for _, c := range msg.Content {
					switch c.Type {
					case "text":
						parts = append(parts, c.Text)
					case "thinking":
						parts = append(parts, c.Thinking)
					case "tool_use":
						parts = append(parts, formatToolUse(c.Name, c.Input))
					}
				}
				return strings.Join(parts, "\n")
			}
		}
	case "tool_result":
		if raw.Data != nil {
			// Tool results can be complex - just store a summary
			return "[Tool Result]"
		}
	}
	return ""
}

// updateSessionMetadata updates session metadata from a turn and its parsed raw entry
func updateSessionMetadata(session *models.Session, turn *models.Turn, raw *models.RawTurn) {
	// Set session ID
	if session.ID == "" && turn.SessionID != "" {
		session.ID = turn.SessionID
	}

	// Set start time from first turn
	if session.StartedAt.IsZero() {
		session.StartedAt = turn.Timestamp
	}

	// Accumulate tokens
	session.InputTokens += int64(turn.InputTokens)
	session.OutputTokens += int64(turn.OutputTokens)

	// Extract model from assistant messages
	if turn.Type == "assistant" && session.Model == "" && raw.Message != nil {
		var msg models.RawAssistantMessage
		if err := json.Unmarshal(raw.Message, &msg); err == nil && msg.Model != "" {
			session.Model = msg.Model
		}
	}

	if raw.GitBranch != "" && session.GitBranch == "" {
		session.GitBranch = raw.GitBranch
	}

	// CWD is the ground truth for project path; lock to first non-empty value
	if raw.CWD != "" && session.ProjectPath == "" {
		session.ProjectPath = raw.CWD
	}
}

// parseTimestamp parses ISO 8601 timestamp string
func parseTimestamp(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, fmt.Errorf("empty timestamp")
	}

	// Try RFC3339 first (most common)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}

	// Try RFC3339Nano
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}

	// Try ISO 8601 with milliseconds
	if t, err := time.Parse("2006-01-02T15:04:05.000Z", s); err == nil {
		return t, nil
	}

	return time.Time{}, fmt.Errorf("unknown timestamp format: %s", s)
}

// ExtractToolUses extracts tool usage information from turns
func ExtractToolUses(turns []models.Turn) []models.ToolUse {
	var toolUses []models.ToolUse

	for _, turn := range turns {
		if turn.Type != "assistant" {
			continue
		}

		var raw models.RawTurn
		if err := json.Unmarshal(turn.RawJSON, &raw); err != nil || raw.Message == nil {
			continue
		}

		var msg models.RawAssistantMessage
		if err := json.Unmarshal(raw.Message, &msg); err != nil {
			continue
		}

		for _, content := range msg.Content {
			if content.Type != "tool_use" {
				continue
			}

			toolUse := models.ToolUse{
				TurnID:    turn.ID,
				SessionID: turn.SessionID,
				ToolName:  content.Name,
				Timestamp: turn.Timestamp,
			}

			// Extract file path for file-related tools
			if content.Input != nil {
				toolUse.FilePath = extractFilePath(content.Name, content.Input)
			}

			toolUses = append(toolUses, toolUse)
		}
	}

	return toolUses
}

// extractUserContent extracts text from user message content
// Content can be a plain string or an array of content blocks
func extractUserContent(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}

	// Try parsing as a string first (most common case)
	var str string
	if err := json.Unmarshal(content, &str); err == nil {
		return str
	}

	// Try parsing as an array of content blocks
	var blocks []models.UserContentBlock
	if err := json.Unmarshal(content, &blocks); err == nil {
		var parts []string
		for _, block := range blocks {
			switch block.Type {
			case "text":
				if block.Text != "" {
					parts = append(parts, block.Text)
				}
			case "tool_result":
				// Tool results are usually system content, include for context
				if block.Content != "" {
					parts = append(parts, fmt.Sprintf("[Tool Result: %s]", truncate(block.Content, 200)))
				}
			}
		}
		return strings.Join(parts, "\n")
	}

	return ""
}

// truncate truncates a string to max length with ellipsis
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

// formatToolUse formats a tool use with key parameters
func formatToolUse(toolName string, input json.RawMessage) string {
	if len(input) == 0 {
		return fmt.Sprintf("[Tool: %s]", toolName)
	}

	var params map[string]interface{}
	if err := json.Unmarshal(input, &params); err != nil {
		return fmt.Sprintf("[Tool: %s]", toolName)
	}

	// Extract key parameters based on tool type
	var details []string

	switch toolName {
	case "Bash":
		if cmd, ok := params["command"].(string); ok {
			// Truncate long commands
			if len(cmd) > 100 {
				cmd = cmd[:97] + "..."
			}
			details = append(details, fmt.Sprintf("$ %s", cmd))
		}
	case "Read":
		if fp, ok := params["file_path"].(string); ok {
			details = append(details, fp)
		}
	case "Write":
		if fp, ok := params["file_path"].(string); ok {
			details = append(details, fmt.Sprintf("-> %s", fp))
		}
	case "Edit":
		if fp, ok := params["file_path"].(string); ok {
			details = append(details, fp)
		}
	case "Glob":
		if pattern, ok := params["pattern"].(string); ok {
			details = append(details, pattern)
		}
	case "Grep":
		if pattern, ok := params["pattern"].(string); ok {
			if len(pattern) > 50 {
				pattern = pattern[:47] + "..."
			}
			details = append(details, fmt.Sprintf("/%s/", pattern))
		}
	case "Task":
		if desc, ok := params["description"].(string); ok {
			details = append(details, desc)
		}
	case "WebFetch":
		if url, ok := params["url"].(string); ok {
			details = append(details, url)
		}
	case "WebSearch":
		if query, ok := params["query"].(string); ok {
			details = append(details, fmt.Sprintf("\"%s\"", query))
		}
	case "TodoWrite":
		if todos, ok := params["todos"].([]interface{}); ok {
			details = append(details, fmt.Sprintf("%d items", len(todos)))
		}
	case "Skill":
		if skill, ok := params["skill"].(string); ok {
			details = append(details, skill)
		}
	default:
		// For other tools, try to extract common parameters
		for _, key := range []string{"file_path", "path", "query", "command", "name"} {
			if v, ok := params[key].(string); ok && v != "" {
				if len(v) > 60 {
					v = v[:57] + "..."
				}
				details = append(details, v)
				break
			}
		}
	}

	if len(details) > 0 {
		return fmt.Sprintf("[Tool: %s] %s", toolName, strings.Join(details, " "))
	}
	return fmt.Sprintf("[Tool: %s]", toolName)
}

// extractFilePath extracts file path from tool input
func extractFilePath(toolName string, input json.RawMessage) string {
	var params map[string]interface{}
	if err := json.Unmarshal(input, &params); err != nil {
		return ""
	}

	// Different tools use different parameter names
	pathKeys := []string{"file_path", "path", "filepath", "filename"}
	for _, key := range pathKeys {
		if v, ok := params[key].(string); ok && v != "" {
			return v
		}
	}

	return ""
}
