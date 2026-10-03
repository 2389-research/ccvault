// ABOUTME: Tests that search_conversations surfaces tool-payload hits and labels them.
// ABOUTME: An agent has to be able to tell a conversational hit from a command-output hit.

package mcp

import (
	"strings"
	"testing"
	"time"

	"github.com/2389-research/ccvault/pkg/models"
)

// TestSearchConversations_FindsAndLabelsAToolPayloadHit covers the new
// searchable material end to end through the MCP surface. The command exists
// only in a stored tool input; the turn's own content names the tool and
// nothing else.
func TestSearchConversations_FindsAndLabelsAToolPayloadHit(t *testing.T) {
	s, database := newTestServer(t)
	p := seedProject(t, database, "/test/proj")
	seedSession(t, database, "session-1", p.ID)

	uses := []models.ToolUse{{
		TurnID:      "session-1-turn-1",
		SessionID:   "session-1",
		ToolName:    "Bash",
		Timestamp:   time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC),
		ToolUseID:   "toolu_MCP",
		InputJSON:   `{"command":"terraform apply -auto-approve"}`,
		InputLength: 43,
		HasResult:   true,
		// Deliberately not a word anyone would type into a conversation.
		ResultContent: "Apply complete! Resources: 3 added, 0 changed, 0 destroyed.",
		ResultLength:  58,
	}}
	if err := database.InsertToolUses(uses); err != nil {
		t.Fatalf("insert tool uses: %v", err)
	}

	result, err := s.searchConversations(map[string]interface{}{"query": `"terraform apply"`})
	if err != nil {
		t.Fatalf("searchConversations: %v", err)
	}

	m := resultMap(t, result)
	if count := mustInt(t, m, "count"); count != 1 {
		t.Fatalf("count = %v, want 1", count)
	}

	results := mustField[[]map[string]interface{}](t, m, "results")
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	hit := results[0]

	matched, ok := hit["matched_tool_name"].(string)
	if !ok || matched != "Bash" {
		t.Errorf("matched_tool_name = %v, want Bash", hit["matched_tool_name"])
	}
	snippet, _ := hit["snippet"].(string)
	if !strings.Contains(snippet, "terraform apply") {
		t.Errorf("snippet = %q, want it to show the matching command", snippet)
	}
}

// TestSearchConversations_ConversationalHitHasNoMatchedTool is the other side
// of the label: present for payload hits, null for everything else, so an
// agent can rely on it rather than inferring.
func TestSearchConversations_ConversationalHitHasNoMatchedTool(t *testing.T) {
	s, database := newTestServer(t)
	p := seedProject(t, database, "/test/proj")
	seedSession(t, database, "session-1", p.ID)

	result, err := s.searchConversations(map[string]interface{}{"query": "hello"})
	if err != nil {
		t.Fatalf("searchConversations: %v", err)
	}

	m := resultMap(t, result)
	results := mustField[[]map[string]interface{}](t, m, "results")
	if len(results) == 0 {
		t.Fatal("no results for the seeded turn content")
	}
	raw, present := results[0]["matched_tool_name"]
	if !present {
		t.Fatalf("matched_tool_name is missing; the response has %v", sortedKeys(results[0]))
	}
	if raw != nil {
		t.Errorf("matched_tool_name = %v, want nil for a content hit", raw)
	}
}
