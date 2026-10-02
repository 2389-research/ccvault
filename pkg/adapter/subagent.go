// ABOUTME: Shared helpers for subagent (sidechain) transcripts: composite ids, path parts, meta.json.
// ABOUTME: One pattern for every adapter, because two spellings of the same id would be two bugs.

package adapter

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

const (
	subagentDirName    = "subagents"
	subagentFilePrefix = "agent-"
	subagentMetaSuffix = ".meta.json"
)

// SubagentSessionID mints the session id for a subagent transcript:
//
//	<source>:<parent-uuid>:<agent-id>
//	nanoclaw:845f7a4e-2827-4f87-8c31-2e4d0b429405:agent-a07c3516373ab4719
//	claude-code:04fb5717-c508-4503-ac85-dc11787cafaa:agent-a01b71e80ea28b3ad
//
// The transcript cannot supply its own identity: its in-band sessionId field
// holds the PARENT session's uuid, so storing it under that id would collide
// with the parent on the sessions.id primary key and the subagent would
// overwrite the session that dispatched it. The nanoclaw adapter has minted
// this exact shape since it started ingesting sidechains (133 rows, zero
// collisions); this is that rule, lifted out so every adapter shares it.
//
// The fallbacks keep a real file from ever producing an empty id while still
// returning "" when there is genuinely nothing to mint from, which is the
// signal sync uses to skip a file.
func SubagentSessionID(source, parentID, agentID string) string {
	switch {
	case parentID != "" && agentID != "":
		return source + ":" + parentID + ":" + agentID
	case agentID != "":
		return source + ":" + agentID
	case parentID != "":
		return source + ":" + parentID
	default:
		return ""
	}
}

// IsSubagentPath reports whether a transcript lives under a subagents/
// directory. Matches on a full path segment so a project literally named
// "subagents-experiments" isn't mistaken for one.
func IsSubagentPath(path string) bool {
	dir := filepath.ToSlash(filepath.Dir(path))
	for _, p := range strings.Split(dir, "/") {
		if p == subagentDirName {
			return true
		}
	}
	return false
}

// SubagentParentUUID returns the parent session's uuid for a subagent path,
// which is the directory holding the subagents/ directory:
//
//	.../projects/<encoded>/<parent-uuid>/subagents/agent-<hex>.jsonl
func SubagentParentUUID(path string) string {
	parts := strings.Split(filepath.ToSlash(path), "/")
	for i, p := range parts {
		if p == subagentDirName && i >= 1 {
			return parts[i-1]
		}
	}
	return ""
}

// SubagentAgentID returns the agent id for a subagent path — the filename stem
// including the "agent-" prefix, e.g. "agent-a7ae9e5a676a18b62". Keeping the
// prefix means the id lines up with the sibling .meta.json filename and with
// the agentId field inside the transcript.
func SubagentAgentID(path string) string {
	base := filepath.Base(path)
	if !strings.HasSuffix(base, ".jsonl") {
		return ""
	}
	return strings.TrimSuffix(base, ".jsonl")
}

// SubagentMeta is the sidecar Claude Code writes next to every subagent
// transcript. Optional context, not a requirement for ingestion.
type SubagentMeta struct {
	AgentType   string `json:"agentType"`
	Description string `json:"description"`
	// ToolUseID is the id of the tool_use block in the DISPATCHING turn.
	// Measured across 95 real transcripts, 87 resolve to a tool_use in the
	// parent; the other 8 are nested agents (spawnDepth >= 2) whose
	// dispatch lives in a sibling subagent's transcript instead.
	ToolUseID  string `json:"toolUseId"`
	SpawnDepth int    `json:"spawnDepth"`
}

// ReadSubagentMeta reads the .meta.json sibling of a subagent transcript.
// A missing or malformed file reads back as the zero value: the transcript
// parses fine without it, and a misleading partial value would be worse than
// nothing.
func ReadSubagentMeta(subagentPath string) SubagentMeta {
	stem := SubagentAgentID(subagentPath)
	if stem == "" {
		return SubagentMeta{}
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(subagentPath), stem+subagentMetaSuffix))
	if err != nil {
		// Both the missing case and a read failure (permissions, dead
		// mount) surface as "no metadata" — the distinction has no
		// consequence for the caller.
		return SubagentMeta{}
	}
	var meta SubagentMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return SubagentMeta{}
	}
	return meta
}
