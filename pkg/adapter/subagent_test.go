// ABOUTME: Tests for the shared subagent id/path helpers used by every adapter.
// ABOUTME: One pattern for composite ids, not one per adapter.

package adapter

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSubagentSessionID(t *testing.T) {
	cases := []struct {
		name     string
		source   string
		parentID string
		agentID  string
		want     string
	}{
		{
			// The exact shape nanoclaw has been writing since it started
			// ingesting sidechains.
			name:     "nanoclaw",
			source:   "nanoclaw",
			parentID: "845f7a4e-2827-4f87-8c31-2e4d0b429405",
			agentID:  "agent-a07c3516373ab4719",
			want:     "nanoclaw:845f7a4e-2827-4f87-8c31-2e4d0b429405:agent-a07c3516373ab4719",
		},
		{
			name:     "claude code",
			source:   "claude-code",
			parentID: "04fb5717-c508-4503-ac85-dc11787cafaa",
			agentID:  "agent-a01b71e80ea28b3ad",
			want:     "claude-code:04fb5717-c508-4503-ac85-dc11787cafaa:agent-a01b71e80ea28b3ad",
		},
		{
			name:    "no parent still yields a usable id",
			source:  "nanoclaw",
			agentID: "agent-a07c3516373ab4719",
			want:    "nanoclaw:agent-a07c3516373ab4719",
		},
		{
			name:     "no agent id falls back to the parent",
			source:   "nanoclaw",
			parentID: "845f7a4e-2827-4f87-8c31-2e4d0b429405",
			want:     "nanoclaw:845f7a4e-2827-4f87-8c31-2e4d0b429405",
		},
		{
			name:   "nothing to mint from",
			source: "nanoclaw",
			want:   "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SubagentSessionID(tc.source, tc.parentID, tc.agentID); got != tc.want {
				t.Errorf("SubagentSessionID(%q, %q, %q) = %q, want %q",
					tc.source, tc.parentID, tc.agentID, got, tc.want)
			}
		})
	}
}

func TestIsSubagentPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/p/projects/-x/04fb5717/subagents/agent-a01b71e80ea28b3ad.jsonl", true},
		{"/p/projects/-x/04fb5717.jsonl", false},
		{"/p/projects/-subagents-project/04fb5717.jsonl", false},
	}
	for _, tc := range cases {
		if got := IsSubagentPath(tc.path); got != tc.want {
			t.Errorf("IsSubagentPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestSubagentPathParts(t *testing.T) {
	const path = "/p/projects/-x/04fb5717-c508-4503-ac85-dc11787cafaa/subagents/agent-a01b71e80ea28b3ad.jsonl"

	if got := SubagentParentUUID(path); got != "04fb5717-c508-4503-ac85-dc11787cafaa" {
		t.Errorf("SubagentParentUUID = %q", got)
	}
	if got := SubagentAgentID(path); got != "agent-a01b71e80ea28b3ad" {
		t.Errorf("SubagentAgentID = %q", got)
	}
	if got := SubagentAgentID("/p/subagents/agent-x.meta.json"); got != "" {
		t.Errorf("SubagentAgentID on a non-jsonl = %q, want empty", got)
	}
}

func TestReadSubagentMeta(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-a01b71e80ea28b3ad.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	meta := `{"agentType":"general-purpose","description":"Implement Task 7","toolUseId":"toolu_018APMWCXLmJBpvk6iytRVM1","spawnDepth":2}`
	if err := os.WriteFile(filepath.Join(dir, "agent-a01b71e80ea28b3ad.meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}

	got := ReadSubagentMeta(path)
	if got.AgentType != "general-purpose" {
		t.Errorf("AgentType = %q", got.AgentType)
	}
	if got.ToolUseID != "toolu_018APMWCXLmJBpvk6iytRVM1" {
		t.Errorf("ToolUseID = %q", got.ToolUseID)
	}
	if got.SpawnDepth != 2 {
		t.Errorf("SpawnDepth = %d, want 2", got.SpawnDepth)
	}

	// A missing meta.json is normal, not an error: it is optional context.
	missing := ReadSubagentMeta(filepath.Join(dir, "agent-nope.jsonl"))
	if missing.AgentType != "" || missing.ToolUseID != "" || missing.SpawnDepth != 0 {
		t.Errorf("missing meta should read back zero, got %+v", missing)
	}
}
