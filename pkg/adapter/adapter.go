// ABOUTME: Defines the SourceAdapter interface and shared types for parsing conversation sessions.
// ABOUTME: All source-specific adapters (Claude Code, Cursor, etc.) implement SourceAdapter.

package adapter

import (
	"encoding/json"
	"time"
)

// SessionFile represents a discovered session file on disk.
type SessionFile struct {
	Path        string
	ProjectPath string
	ModTime     time.Time
}

// ParsedSession holds the fully parsed contents of a single conversation session.
type ParsedSession struct {
	ID          string
	ProjectPath string
	DisplayName string // Human-readable project name for display in the UI

	// Turns must be in the order the source recorded them — file order for a
	// JSONL transcript. That slice order *is* the session's turn order: sync
	// derives each turn's ordinal from the index, so an adapter that returns
	// turns sorted any other way (by timestamp, say) renumbers the
	// conversation rather than merely presenting it differently. There is no
	// position field to carry instead, deliberately: one source of order
	// cannot disagree with itself.
	Turns      []ParsedTurn
	Model      string
	GitBranch  string
	StartedAt  time.Time
	EndedAt    time.Time
	SourceName string
	Metadata   map[string]any
}

// ParsedTurn represents one turn (message) within a session.
type ParsedTurn struct {
	ID           string
	ParentID     string
	Type         string // "user", "assistant", "system"
	Timestamp    time.Time
	Content      string
	RawJSON      json.RawMessage
	InputTokens  int64
	OutputTokens int64
	ToolUses     []ParsedToolUse
	HasError     bool
}

// ParsedToolUse captures a single tool invocation within a turn.
type ParsedToolUse struct {
	ToolName string
	FilePath string
}

// SourceAdapter is the interface that all conversation source backends must implement.
// Discover finds session files under a root directory; Parse reads one file into a ParsedSession.
type SourceAdapter interface {
	Name() string
	Discover(root string) ([]SessionFile, error)
	Parse(path string) (*ParsedSession, error)
}
