// ABOUTME: Claude Code source adapter that wraps the existing parser and scanner.
// ABOUTME: Implements the SourceAdapter interface for discovering and parsing Claude Code JSONL sessions.

package claudecode

import (
	"encoding/json"

	"github.com/2389-research/ccvault/pkg/adapter"
	"github.com/2389-research/ccvault/pkg/models"
	"github.com/2389-research/ccvault/pkg/parser"
)

func init() {
	adapter.Register("claude-code", func() adapter.SourceAdapter {
		return New()
	})
}

// Adapter implements adapter.SourceAdapter for Claude Code JSONL session files.
type Adapter struct{}

// New creates a new Claude Code adapter.
func New() *Adapter {
	return &Adapter{}
}

// Name returns the adapter's identifier.
func (a *Adapter) Name() string {
	return "claude-code"
}

// Discover scans a Claude Code home directory for session files and returns them
// as adapter.SessionFile entries.
func (a *Adapter) Discover(root string) ([]adapter.SessionFile, error) {
	parserFiles, err := parser.ScanClaudeHome(root)
	if err != nil {
		return nil, err
	}

	files := make([]adapter.SessionFile, len(parserFiles))
	for i, pf := range parserFiles {
		files[i] = adapter.SessionFile{
			Path:        pf.Path,
			ProjectPath: pf.ProjectPath,
			ModTime:     pf.ModTime,
		}
	}
	return files, nil
}

// Parse reads a Claude Code JSONL session file and converts it to adapter.ParsedSession.
// It also detects errors and subagent usage, storing them in Metadata.
func (a *Adapter) Parse(path string) (*adapter.ParsedSession, error) {
	turns, session, skipped, err := parser.ParseSession(path)
	if err != nil {
		return nil, err
	}

	// Extract tool uses for all turns at once
	toolUses := parser.ExtractToolUses(turns)

	// Index tool uses by turn ID for efficient lookup
	toolUsesByTurn := make(map[string][]models.ToolUse)
	for _, tu := range toolUses {
		toolUsesByTurn[tu.TurnID] = append(toolUsesByTurn[tu.TurnID], tu)
	}

	metadata := make(map[string]any)
	hasError := false
	hasSubagent := false

	parsedTurns := make([]adapter.ParsedTurn, len(turns))
	for i, t := range turns {
		pt := adapter.ParsedTurn{
			ID:           t.ID,
			ParentID:     t.ParentID,
			Type:         t.Type,
			Timestamp:    t.Timestamp,
			Content:      t.Content,
			RawJSON:      t.RawJSON,
			InputTokens:  int64(t.InputTokens),
			OutputTokens: int64(t.OutputTokens),
		}

		if turnHasError(t.RawJSON) {
			pt.HasError = true
			hasError = true
		}

		// Convert tool uses for this turn
		if tus, ok := toolUsesByTurn[t.ID]; ok {
			for _, tu := range tus {
				pt.ToolUses = append(pt.ToolUses, adapter.ParsedToolUse{
					ToolName: tu.ToolName,
					FilePath: tu.FilePath,
				})
				// Subagent detection: any tool use with ToolName == "Task"
				if tu.ToolName == "Task" {
					hasSubagent = true
				}
			}
		}

		parsedTurns[i] = pt
	}

	if hasError {
		metadata["has_error"] = true
	}
	if hasSubagent {
		metadata["has_subagent"] = true
	}
	if skipped > 0 {
		metadata["skipped_lines"] = skipped
	}

	parsed := &adapter.ParsedSession{
		ID:          session.ID,
		ProjectPath: session.ProjectPath,
		DisplayName: parser.GetDisplayName(session.ProjectPath),
		Turns:       parsedTurns,
		Model:       session.Model,
		GitBranch:   session.GitBranch,
		StartedAt:   session.StartedAt,
		EndedAt:     session.EndedAt,
		SourceName:  "claude-code",
		Metadata:    metadata,
	}

	return parsed, nil
}

// turnHasError reports whether the raw turn carries a tool_result content block
// with is_error: true. Claude Code stores tool error flags inside the user
// message's content array, so we walk that structure rather than substring-match.
func turnHasError(rawJSON json.RawMessage) bool {
	if len(rawJSON) == 0 {
		return false
	}
	var raw models.RawTurn
	if err := json.Unmarshal(rawJSON, &raw); err != nil || raw.Message == nil {
		return false
	}
	var msg models.RawUserMessage
	if err := json.Unmarshal(raw.Message, &msg); err != nil {
		return false
	}
	var blocks []models.UserContentBlock
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		return false
	}
	for _, block := range blocks {
		if block.Type == "tool_result" && block.IsError {
			return true
		}
	}
	return false
}
