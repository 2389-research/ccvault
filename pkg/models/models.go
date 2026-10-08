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

	// ParentSessionID is the id of the session that dispatched this one, empty
	// for a top-level session. A subagent transcript is its own session row
	// (see db migration 007), identified by a minted composite id, and this is
	// the column that carries the relationship.
	ParentSessionID string `json:"parent_session_id,omitempty"`

	// LastEntryUUID is the id of the turn at this session's highest ordinal,
	// empty for a session with no turns. It is what lets a re-parse confirm
	// it is resuming from the right place: if the transcript it just read
	// does not contain this turn, the file was rewritten upstream rather
	// than appended to, and a row count or an mtime cannot tell the
	// difference.
	LastEntryUUID string `json:"last_entry_uuid,omitempty"`

	// SubagentCount is how many sessions name this one as their parent. It is
	// derived at read time, not stored. Default listings show top-level
	// sessions only, and this count is what keeps that from hiding anything:
	// a row that says "subagents: 3" is filtered, not secret.
	SubagentCount int `json:"subagent_count"`
}

// TotalTokens returns the sum of all token usage
func (s *Session) TotalTokens() int64 {
	return s.InputTokens + s.OutputTokens + s.CacheReadTokens + s.CacheWriteTokens
}

// Turn represents a single entry in a conversation
type Turn struct {
	ID        string    `json:"id"` // UUID
	SessionID string    `json:"session_id"`
	ParentID  string    `json:"parent_id,omitempty"`
	Type      string    `json:"type"` // user, assistant, tool_use, tool_result, progress
	Timestamp time.Time `json:"timestamp"`

	// Ordinal is the turn's position within its session, counting from 0 with
	// no gaps, over every turn type. It is the ordering key — Timestamp ties
	// to the millisecond inside one assistant response and goes backwards
	// when a clock skews — and the cursor a caller uses to ask for "the turns
	// after position N".
	//
	// Not omitempty: ordinal 0 is the first turn of every session, and
	// dropping it from the JSON would make the one position a consumer is
	// most likely to start from the one position it cannot read.
	Ordinal int `json:"ordinal"`

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

	// ToolUseID is the id the source gave this call, verbatim — toolu_… for
	// Claude Code and nanoclaw, call_… for codex, jeff's tool_id (empty on 85%
	// of real jeff requests). Empty for a source that mints no id.
	//
	// The provider's own id rather than a surrogate, because its job is to be
	// joinable against ids recorded elsewhere. A subagent transcript's
	// meta.json carries a toolUseId naming the exact call that dispatched it
	// (#31, resolving for 87 of 95 real subagents), and that link had nowhere
	// to land until this column existed.
	ToolUseID string `json:"tool_use_id,omitempty"`

	// InputJSON is the call's arguments, stored whole. Inputs are never
	// omitted: 259,836 of them total 78.6 MB across the author's archive, p50
	// 72 bytes, largest 99,792.
	//
	// Not guaranteed to parse as JSON, despite the name — which follows the
	// column agentsview established. It holds the arguments exactly as the
	// source recorded them, and one source does not record JSON: codex's
	// custom_tool_call carries a plain-text body in `input`, which is how
	// apply_patch sends a diff. Treat it as text; json_extract it only after
	// checking json_valid.
	InputJSON string `json:"input_json,omitempty"`

	// InputLength is len(InputJSON). Stored rather than derived so a consumer
	// reading a row can size the payload without fetching it.
	InputLength int `json:"input_length,omitempty"`

	// HasResult reports whether the source recorded a result for this call. It
	// separates "the tool answered with nothing" from "no answer was ever
	// recorded" — an interrupted call, which the empty string cannot express.
	HasResult bool `json:"has_result"`

	// ResultContent is the result text, empty when the content was left out
	// (see ResultOmittedReason) or when the tool genuinely returned nothing.
	ResultContent string `json:"result_content,omitempty"`

	// ResultLength is the result's size as the transcript carries it,
	// recorded whether or not the content was stored. See
	// toolpayload.Result.Length for the exact definition.
	ResultLength int `json:"result_length,omitempty"`

	// ResultOmittedReason names why ResultContent was left out — one of
	// toolpayload's Omit* values, empty when the content is stored. A consumer
	// reads this instead of re-deriving the classification from tool_name and
	// ResultLength.
	ResultOmittedReason string `json:"result_omitted_reason,omitempty"`

	// IsError reports whether the result said the call failed. Three states,
	// all of them real (#83):
	//
	//   nil    nothing is known. Either no result ever answered the call, or
	//          the source records no error flag at all — codex's
	//          function_call_output and jeff's tool_result both carry the
	//          output and nothing about its success.
	//   false  a result arrived and did not report a failure. Claude Code
	//          writes is_error: false on 44,533 of the author's result blocks
	//          and omits the key on 217,581 more; both mean the same thing.
	//   true   the result reported a failure. 10,909 of them in the author's
	//          archive, whose text was already stored by #28 but only
	//          findable by guessing at the wording.
	//
	// A pointer rather than the HasResult/ResultLength pair above, because
	// that pair exists only to give an int a nil it does not have. A bool does
	// have one, and a second gating field would encode three states in four.
	//
	// Not derivable from ResultContent: a tool that fails having printed
	// nothing, and a Read whose body the policy omits, both leave no text to
	// read a failure out of. The flag is the only thing those rows carry.
	IsError *bool `json:"is_error,omitempty"`
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
