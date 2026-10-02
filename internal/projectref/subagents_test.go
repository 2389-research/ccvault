// ABOUTME: Tests that Class C session output always carries the subagent fields.
// ABOUTME: A filtered listing that doesn't say what it filtered misleads scripts.

package projectref

import (
	"testing"
	"time"

	"github.com/2389-research/ccvault/pkg/models"
)

// TestSessionRefAlwaysCarriesSubagentFields covers the rule that
// list-sessions --json and every MCP session response emit
// parent_session_id and subagent_count whether or not the caller filtered.
// A script reading the default listing has to be able to tell that rows were
// held back, and from which parent.
func TestSessionRefAlwaysCarriesSubagentFields(t *testing.T) {
	started := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	parent := SessionRef(&models.Session{
		ID:            "04fb5717-c508-4503-ac85-dc11787cafaa",
		StartedAt:     started,
		SubagentCount: 3,
	}, nil)
	count, ok := parent["subagent_count"]
	if !ok {
		t.Fatal("subagent_count missing from a top-level session ref")
	}
	if count != 3 {
		t.Errorf("subagent_count = %v, want 3", count)
	}
	// Present, but null: a top-level session has no parent, and "" would be a
	// third state consumers would have to learn.
	val, ok := parent["parent_session_id"]
	if !ok {
		t.Fatal("parent_session_id missing from a top-level session ref")
	}
	if val != nil {
		t.Errorf("parent_session_id = %v, want nil for a top-level session", val)
	}

	child := SessionRef(&models.Session{
		ID:              "claude-code:04fb5717-c508-4503-ac85-dc11787cafaa:agent-a01b71e80ea28b3ad",
		StartedAt:       started,
		ParentSessionID: "04fb5717-c508-4503-ac85-dc11787cafaa",
	}, nil)
	if child["parent_session_id"] != "04fb5717-c508-4503-ac85-dc11787cafaa" {
		t.Errorf("parent_session_id = %v, want the parent id", child["parent_session_id"])
	}
	if child["subagent_count"] != 0 {
		t.Errorf("subagent_count = %v, want 0", child["subagent_count"])
	}
}
