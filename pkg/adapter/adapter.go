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

// ParsedToolUse captures a single tool invocation within a turn, including the
// payloads it carried. See models.ToolUse for what each field means and
// pkg/toolpayload for the policy that decides whether a result's content is
// stored or reduced to its length.
//
// A source that records none of a field leaves it zero — hex has no tool calls
// at all, and jeff records no usable id for 85% of its requests.
type ParsedToolUse struct {
	ToolName string
	FilePath string

	ToolUseID   string
	InputJSON   string
	InputLength int

	HasResult           bool
	ResultContent       string
	ResultLength        int
	ResultOmittedReason string

	// IsError is the tri-state failure flag — see models.ToolUse.IsError. A
	// source whose transcripts carry no error flag leaves it nil, which is why
	// it is not folded into HasResult: codex and jeff record a result and say
	// nothing about whether it failed, and false would assert success they
	// never claimed.
	IsError *bool
}

// SourceAdapter is the interface that all conversation source backends must implement.
// Discover finds session files under a root directory; Parse reads one file into a ParsedSession.
type SourceAdapter interface {
	Name() string
	Discover(root string) ([]SessionFile, error)
	Parse(path string) (*ParsedSession, error)
}
