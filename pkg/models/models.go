// ABOUTME: Core data models for ccvault representing projects, sessions, and turns
// ABOUTME: Mirrors Claude Code's JSONL format with additional computed fields

package models

import (
	"encoding/json"
	"time"
)

// Project represents a Claude Code project (working directory)
type Project struct {
	ID   int64  `json:"id"`
	Path string `json:"path"` // Full filesystem path; the identifier

	// DisplayName is the adapter-provided label for this project. It is a
	// label, not an identifier — two projects can share a DisplayName and
	// still be different projects. Any display or matching code should
	// go through internal/projectref (Label / Inline / Ref / ResolveAll)
	// rather than reading this field directly, so the four-class rendering
	// discipline stays consistent across CLI, TUI, and MCP surfaces. An
	// AST allowlist test enforces this — direct reads outside the helper
	// package will fail CI.
	DisplayName string `json:"display_name"`

	FirstSeenAt    time.Time `json:"first_seen_at"`
	LastActivityAt time.Time `json:"last_activity_at"`
	SessionCount   int       `json:"session_count"`
	TotalTokens    int64     `json:"total_tokens"`
	Source         string    `json:"source"` // Which AI tool produced this data (e.g. "claude-code", "codex")
}

// Session represents a single Claude Code conversation session
type Session struct {
	ID               string    `json:"id"` // UUID from Claude Code
	ProjectID        int64     `json:"project_id"`
	ProjectPath      string    `json:"project_path,omitempty"` // For convenience before DB insert
	StartedAt        time.Time `json:"started_at"`
	EndedAt          time.Time `json:"ended_at,omitempty"`
	Model            string    `json:"model,omitempty"`
	GitBranch        string    `json:"git_branch,omitempty"`
	TurnCount        int       `json:"turn_count"`
	InputTokens      int64     `json:"input_tokens"`
	OutputTokens     int64     `json:"output_tokens"`
	CacheReadTokens  int64     `json:"cache_read_tokens"`
	CacheWriteTokens int64     `json:"cache_write_tokens"`
	SourceFile       string    `json:"source_file"` // Path to .jsonl file
	HasError         bool      `json:"has_error"`
	HasSubagent      bool      `json:"has_subagent"`
	Source           string    `json:"source"` // Which AI tool produced this data (e.g. "claude-code", "codex")
}

// TotalTokens returns the sum of all token usage
func (s *Session) TotalTokens() int64 {
	return s.InputTokens + s.OutputTokens + s.CacheReadTokens + s.CacheWriteTokens
}

// Turn represents a single entry in a conversation
type Turn struct {
	ID           string          `json:"id"` // UUID
	SessionID    string          `json:"session_id"`
	ParentID     string          `json:"parent_id,omitempty"`
	Type         string          `json:"type"` // user, assistant, tool_use, tool_result, progress
	Timestamp    time.Time       `json:"timestamp"`
	Content      string          `json:"content,omitempty"`  // Extracted text for search
	RawJSON      json.RawMessage `json:"raw_json,omitempty"` // Original JSONL entry
	InputTokens  int             `json:"input_tokens,omitempty"`
	OutputTokens int             `json:"output_tokens,omitempty"`
}

// ToolUse represents a tool invocation within a session
type ToolUse struct {
	ID        int64     `json:"id"`
	TurnID    string    `json:"turn_id"`
	SessionID string    `json:"session_id"`
	ToolName  string    `json:"tool_name"`
	FilePath  string    `json:"file_path,omitempty"` // For file-related tools
	Timestamp time.Time `json:"timestamp"`
}

// RawTurn represents the raw JSONL entry from Claude Code
// This is used for parsing before converting to our internal Turn type
type RawTurn struct {
	UUID       string          `json:"uuid"`
	ParentUUID string          `json:"parentUuid,omitempty"`
	SessionID  string          `json:"sessionId"`
	Type       string          `json:"type"`
	Timestamp  string          `json:"timestamp"` // ISO 8601 string
	Message    json.RawMessage `json:"message,omitempty"`
	Data       json.RawMessage `json:"data,omitempty"`
	CWD        string          `json:"cwd,omitempty"`
	Version    string          `json:"version,omitempty"`
	GitBranch  string          `json:"gitBranch,omitempty"`
}

// RawUserMessage represents a user message in Claude Code format
// Content can be a string or an array of content blocks
type RawUserMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// UserContentBlock represents a content block in a user message array
type UserContentBlock struct {
	Type      string `json:"type"` // text, tool_result
	Text      string `json:"text,omitempty"`
	Content   string `json:"content,omitempty"` // For tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	IsError   bool   `json:"is_error,omitempty"` // For tool_result
}

// RawAssistantMessage represents an assistant message in Claude Code format
type RawAssistantMessage struct {
	ID         string             `json:"id"`
	Model      string             `json:"model"`
	Role       string             `json:"role"`
	Content    []AssistantContent `json:"content"`
	Usage      *TokenUsage        `json:"usage,omitempty"`
	StopReason string             `json:"stop_reason,omitempty"`
}

// AssistantContent represents a content block in an assistant message
type AssistantContent struct {
	Type     string          `json:"type"` // text, thinking, tool_use
	Text     string          `json:"text,omitempty"`
	Thinking string          `json:"thinking,omitempty"`
	ID       string          `json:"id,omitempty"`   // For tool_use
	Name     string          `json:"name,omitempty"` // Tool name
	Input    json.RawMessage `json:"input,omitempty"`
}

// TokenUsage represents token counts from Claude API
type TokenUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
}

// HistoryEntry represents an entry from ~/.claude/history.jsonl
type HistoryEntry struct {
	Display        string            `json:"display"`
	PastedContents map[string]string `json:"pastedContents,omitempty"`
	Timestamp      int64             `json:"timestamp"` // Unix milliseconds
	Project        string            `json:"project"`
	SessionID      string            `json:"sessionId"`
}

// SearchResult represents a search result with context
type SearchResult struct {
	Turn    Turn    `json:"turn"`
	Session Session `json:"session"`
	Project Project `json:"project"`
	Snippet string  `json:"snippet"` // Matched text with context
	Score   float64 `json:"score"`   // Relevance score
}

// Stats represents archive statistics
type Stats struct {
	TotalProjects int              `json:"total_projects"`
	TotalSessions int              `json:"total_sessions"`
	TotalTurns    int              `json:"total_turns"`
	TotalTokens   int64            `json:"total_tokens"`
	FirstActivity time.Time        `json:"first_activity"`
	LastActivity  time.Time        `json:"last_activity"`
	TopProjects   []Project        `json:"top_projects,omitempty"`
	TokensByModel map[string]int64 `json:"tokens_by_model,omitempty"`
}
