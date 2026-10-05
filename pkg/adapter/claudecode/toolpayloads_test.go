// ABOUTME: Tests that the claude-code adapter carries the parser's tool payloads through to ParsedToolUse.
// ABOUTME: The payload extraction itself is tested in pkg/parser; this pins that the adapter does not drop it.

package claudecode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParse_CarriesToolPayloadsThrough covers the hand-off the adapter makes.
// The parser extracts the id, input, and result; this layer converts
// models.ToolUse to adapter.ParsedToolUse field by field, and a field left out
// of that conversion is silently empty the whole way to the database.
func TestParse_CarriesToolPayloadsThrough(t *testing.T) {
	transcript := `{"uuid":"a-1","sessionId":"s-cc","type":"assistant","cwd":"/tmp/proj","timestamp":"2026-09-01T10:00:00.000Z","message":{"model":"claude","role":"assistant","content":[{"type":"tool_use","id":"toolu_CC1","name":"Bash","input":{"command":"go test ./..."}}]}}
{"uuid":"u-1","sessionId":"s-cc","type":"user","timestamp":"2026-09-01T10:00:01.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_CC1","content":"ok github.com/2389-research/ccvault"}]}}
`
	path := filepath.Join(t.TempDir(), "s-cc.jsonl")
	if err := os.WriteFile(path, []byte(transcript), 0o644); err != nil {
		t.Fatal(err)
	}

	parsed, err := New().Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	var found bool
	for _, turn := range parsed.Turns {
		for _, tu := range turn.ToolUses {
			found = true
			if tu.ToolUseID != "toolu_CC1" {
				t.Errorf("ToolUseID = %q, want %q", tu.ToolUseID, "toolu_CC1")
			}
			if !strings.Contains(tu.InputJSON, "go test ./...") {
				t.Errorf("InputJSON = %q, want the command", tu.InputJSON)
			}
			if tu.InputLength != len(tu.InputJSON) {
				t.Errorf("InputLength = %d, len = %d", tu.InputLength, len(tu.InputJSON))
			}
			if !tu.HasResult {
				t.Error("HasResult = false, want true")
			}
			if want := "ok github.com/2389-research/ccvault"; tu.ResultContent != want {
				t.Errorf("ResultContent = %q, want %q", tu.ResultContent, want)
			}
			if tu.ResultLength != len("ok github.com/2389-research/ccvault") {
				t.Errorf("ResultLength = %d, want %d", tu.ResultLength, len("ok github.com/2389-research/ccvault"))
			}
			if tu.ResultOmittedReason != "" {
				t.Errorf("ResultOmittedReason = %q, want empty", tu.ResultOmittedReason)
			}
		}
	}
	if !found {
		t.Fatal("no tool uses on any turn")
	}
}
