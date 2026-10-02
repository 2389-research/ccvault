// ABOUTME: Nanoclaw source adapter that discovers and parses nanoclaw agent conversation logs.
// ABOUTME: Handles both parent sessions and subagent sidechains under sessions/<group>/.claude/.

package nanoclaw

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/2389-research/ccvault/pkg/adapter"
	"github.com/2389-research/ccvault/pkg/models"
	"github.com/2389-research/ccvault/pkg/parser"
)

const (
	sessionIDPrefix     = "nanoclaw:"
	scheduledTaskPrefix = "[SCHEDULED TASK - "
)

func init() {
	adapter.Register("nanoclaw", func() adapter.SourceAdapter {
		return New()
	})
}

// Adapter implements adapter.SourceAdapter for nanoclaw agent session files.
type Adapter struct{}

// New creates a new nanoclaw adapter.
func New() *Adapter {
	return &Adapter{}
}

// Name returns the adapter identifier.
func (a *Adapter) Name() string {
	return "nanoclaw"
}

// Discover scans the nanoclaw sessions directory for JSONL session files.
// root should point to the sessions root, e.g. ~/work/tools/nanoclaw/data/sessions/
// Each subdirectory is a group (reed, mo, jo, main, etc.) containing a .claude/ directory
// with parent sessions and optional subagents/ sidechain files.
func (a *Adapter) Discover(root string) ([]adapter.SessionFile, error) {
	groupEntries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading nanoclaw sessions dir: %w", err)
	}

	var files []adapter.SessionFile

	for _, groupEntry := range groupEntries {
		if !groupEntry.IsDir() {
			continue
		}
		group := groupEntry.Name()
		claudeDir := filepath.Join(root, group, ".claude")
		projectPath, _ := nanoclawProjectPath(group)

		// Parent sessions — ScanClaudeHome walks .claude/projects/ and picks up
		// UUID-named JSONLs, skipping any subagents/ subtrees.
		parserFiles, err := parser.ScanClaudeHome(claudeDir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// Group has no .claude/projects/ yet — expected, skip.
				continue
			}
			// Real error (permission denied, corrupt tree, etc.) — surface it
			// so partial data loss isn't silent. See issue #13.
			return nil, fmt.Errorf("scanning nanoclaw group %s: %w", group, err)
		}
		for _, pf := range parserFiles {
			files = append(files, adapter.SessionFile{
				Path:        pf.Path,
				ProjectPath: projectPath,
				ModTime:     pf.ModTime,
			})
		}

		// Subagent sidechains — walked separately because the shared scanner
		// intentionally skips subagents/ directories and rejects the
		// agent-<hex>.jsonl naming (non-UUID base names).
		subFiles, err := discoverSubagents(filepath.Join(claudeDir, "projects"), projectPath)
		if err != nil {
			return nil, err
		}
		files = append(files, subFiles...)
	}

	return files, nil
}

// discoverSubagents collects every */subagents/agent-*.jsonl file under a
// .claude/projects/ tree, re-labelled with nanoclaw's group project path
// (the shared walker derives a project path from the encoded directory name,
// which is not how nanoclaw names its projects). Missing trees are empty.
func discoverSubagents(projectsDir, projectPath string) ([]adapter.SessionFile, error) {
	parserFiles, err := parser.ScanSubagentFiles(projectsDir)
	if err != nil {
		return nil, fmt.Errorf("nanoclaw subagents: %w", err)
	}

	files := make([]adapter.SessionFile, 0, len(parserFiles))
	for _, pf := range parserFiles {
		files = append(files, adapter.SessionFile{
			Path:        pf.Path,
			ProjectPath: projectPath,
			ModTime:     pf.ModTime,
		})
	}
	return files, nil
}

// nanoclawProjectPath computes the ccvault projectPath and human-readable
// display name for a given nanoclaw group. Extracted to a single source of
// truth so Discover/parseParent/parseSubagent can't silently drift on the
// naming scheme (see issue #13's related nit).
func nanoclawProjectPath(group string) (projectPath, display string) {
	if group == "" {
		return "nanoclaw", "Nanoclaw"
	}
	return "nanoclaw/" + group, "nanoclaw/" + group
}

// Parse reads a nanoclaw JSONL session file and converts it to adapter.ParsedSession.
// Dispatches to the subagent branch when the path lives under a subagents/ directory,
// otherwise treats it as a parent session.
func (a *Adapter) Parse(path string) (*adapter.ParsedSession, error) {
	if adapter.IsSubagentPath(path) {
		return parseSubagent(path)
	}
	return parseParent(path)
}

// parseParent handles the top-level session JSONL — the same format Claude Code
// writes, with nanoclaw's [SCHEDULED TASK - ...] user injections reclassified
// as system turns.
func parseParent(path string) (*adapter.ParsedSession, error) {
	turns, session, stats, err := parser.ParseSession(path)
	if err != nil {
		return nil, err
	}

	group := extractGroup(path)
	parsedTurns, meta := buildTurnsAndMetadata(turns, true)
	if group != "" {
		meta["nanoclaw_group"] = group
	}
	if stats.SkippedLines > 0 {
		meta["skipped_lines"] = stats.SkippedLines
	}
	if stats.TurnsWithTruncatedRawJSON > 0 {
		meta["turns_with_truncated_raw_json"] = stats.TurnsWithTruncatedRawJSON
	}

	projectPath, display := nanoclawProjectPath(group)

	return &adapter.ParsedSession{
		ID:          sessionIDPrefix + session.ID,
		ProjectPath: projectPath,
		DisplayName: display,
		Turns:       parsedTurns,
		Model:       session.Model,
		GitBranch:   session.GitBranch,
		StartedAt:   session.StartedAt,
		EndedAt:     session.EndedAt,
		SourceName:  "nanoclaw",
		Metadata:    meta,
	}, nil
}

// parseSubagent handles sidechain JSONLs — same wire format as parent sessions
// but sessionId inside the file points at the *parent* session, so we
// disambiguate the ccvault-side ID with the file-derived agent ID.
func parseSubagent(path string) (*adapter.ParsedSession, error) {
	turns, session, stats, err := parser.ParseSession(path)
	if err != nil {
		return nil, err
	}

	group := extractGroup(path)
	parentUUID := adapter.SubagentParentUUID(path)
	agentID := adapter.SubagentAgentID(path)
	agentType := adapter.ReadSubagentMeta(path).AgentType

	parsedTurns, meta := buildTurnsAndMetadata(turns, false)
	meta["is_sidechain"] = true
	meta["agent_id"] = agentID
	if agentType != "" {
		meta["agent_type"] = agentType
	}
	if group != "" {
		meta["nanoclaw_group"] = group
	}
	if stats.SkippedLines > 0 {
		meta["skipped_lines"] = stats.SkippedLines
	}
	if stats.TurnsWithTruncatedRawJSON > 0 {
		meta["turns_with_truncated_raw_json"] = stats.TurnsWithTruncatedRawJSON
	}
	// parent_session_id points at whatever ccvault stored the parent under —
	// keep it namespaced so cross-source joins can't collide.
	if parentUUID != "" {
		meta["parent_session_id"] = sessionIDPrefix + parentUUID
	} else if session.ID != "" {
		// Fall back to the sessionId embedded in the message body if the path
		// doesn't yield a parent UUID.
		meta["parent_session_id"] = sessionIDPrefix + session.ID
	}

	// Unique ID: <source>:<parent-uuid>:<agent-id>, minted by the shared
	// helper so claude-code and nanoclaw cannot drift onto two spellings of
	// the same composite id. Falls back to the in-band sessionId when the
	// path yields no parent, so a real file never gets an empty id.
	parentForID := parentUUID
	if parentForID == "" {
		parentForID = session.ID
	}
	id := adapter.SubagentSessionID("nanoclaw", parentForID, agentID)

	projectPath, display := nanoclawProjectPath(group)

	return &adapter.ParsedSession{
		ID:          id,
		ProjectPath: projectPath,
		DisplayName: display,
		Turns:       parsedTurns,
		Model:       session.Model,
		GitBranch:   session.GitBranch,
		StartedAt:   session.StartedAt,
		EndedAt:     session.EndedAt,
		SourceName:  "nanoclaw",
		Metadata:    meta,
	}, nil
}

// buildTurnsAndMetadata converts parser turns to adapter turns and derives the
// shared metadata flags (has_error, has_subagent, has_scheduled_tasks). When
// reclassifyScheduled is true, [SCHEDULED TASK - ...] user turns are retagged
// as system turns — nanoclaw's own scheduler injects those, they aren't human
// input, and downstream analytics shouldn't treat them as user activity.
func buildTurnsAndMetadata(turns []models.Turn, reclassifyScheduled bool) ([]adapter.ParsedTurn, map[string]any) {
	toolUses := parser.ExtractToolUses(turns)
	toolUsesByTurn := make(map[string][]models.ToolUse)
	for _, tu := range toolUses {
		toolUsesByTurn[tu.TurnID] = append(toolUsesByTurn[tu.TurnID], tu)
	}

	metadata := make(map[string]any)
	hasError := false
	hasSubagent := false
	hasScheduledTasks := false

	parsedTurns := make([]adapter.ParsedTurn, len(turns))
	for i, t := range turns {
		turnType := t.Type

		if reclassifyScheduled && t.Type == "user" && strings.HasPrefix(t.Content, scheduledTaskPrefix) {
			turnType = "system"
			hasScheduledTasks = true
		}

		pt := adapter.ParsedTurn{
			ID:           t.ID,
			ParentID:     t.ParentID,
			Type:         turnType,
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

		if tus, ok := toolUsesByTurn[t.ID]; ok {
			for _, tu := range tus {
				pt.ToolUses = append(pt.ToolUses, adapter.ParsedToolUse{
					ToolName: tu.ToolName,
					FilePath: tu.FilePath,
				})
				if tu.ToolName == "Task" || tu.ToolName == "Agent" {
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
	if hasScheduledTasks {
		metadata["has_scheduled_tasks"] = true
	}

	return parsedTurns, metadata
}

// extractGroup parses the group name from a nanoclaw session path.
// Path pattern: .../sessions/{group}/.claude/...
func extractGroup(path string) string {
	parts := strings.Split(filepath.ToSlash(path), "/")
	for i, part := range parts {
		if part == ".claude" && i > 0 {
			return parts[i-1]
		}
	}
	return ""
}

// turnHasError reports whether the raw turn carries a tool_result with is_error: true.
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
