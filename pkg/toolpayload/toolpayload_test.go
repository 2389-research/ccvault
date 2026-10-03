// ABOUTME: Tests for the tool-payload storage policy — which results get stored whole and which are reduced to a length.
// ABOUTME: Cases are drawn from the shapes measured in the real archive: string contents, text arrays, image blocks.

package toolpayload

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestResultFromJSON_StringContentStoredWhole covers the common case: a
// tool_result whose content is a plain JSON string. It is stored verbatim and
// its length is the string's bytes, so Length == len(Content) exactly.
func TestResultFromJSON_StringContentStoredWhole(t *testing.T) {
	got := ResultFromJSON("Bash", json.RawMessage(`"total 4\ndrwxr-xr-x  2 dylanr staff"`))

	want := "total 4\ndrwxr-xr-x  2 dylanr staff"
	if got.Content != want {
		t.Errorf("Content = %q, want %q", got.Content, want)
	}
	if got.Length != len(want) {
		t.Errorf("Length = %d, want %d", got.Length, len(want))
	}
	if got.OmitReason != "" {
		t.Errorf("OmitReason = %q, want empty", got.OmitReason)
	}
}

// TestResultFromJSON_TextArrayConcatenated covers the other half of the
// archive's results: content is an array of blocks. The text blocks are
// concatenated; Length is the content JSON's bytes, which is what the
// transcript actually carries, so it runs ahead of len(Content) by the JSON
// framing.
func TestResultFromJSON_TextArrayConcatenated(t *testing.T) {
	raw := json.RawMessage(`[{"type":"text","text":"first"},{"type":"text","text":"second"}]`)
	got := ResultFromJSON("Bash", raw)

	if got.Content != "first\nsecond" {
		t.Errorf("Content = %q, want %q", got.Content, "first\nsecond")
	}
	if got.Length != len(raw) {
		t.Errorf("Length = %d, want %d (the content JSON's bytes)", got.Length, len(raw))
	}
	if got.OmitReason != "" {
		t.Errorf("OmitReason = %q, want empty", got.OmitReason)
	}
}

// TestResultFromJSON_ImageDetectedByShape pins the rule from issue #28: an
// image payload is recognised by a {"type":"image"} block in the content, not
// by the tool's name. The tool here is an MCP tool whose name says nothing
// about images, and the base64 must not be stored or indexed.
func TestResultFromJSON_ImageDetectedByShape(t *testing.T) {
	needle := "BASE64PAYLOADCANARYAAAAAAAAAAAAAAAAAA"
	raw := json.RawMessage(`[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + needle + `"}}]`)

	got := ResultFromJSON("mcp__some__arbitrary_name", raw)

	if strings.Contains(got.Content, needle) {
		t.Fatalf("image payload was stored: %q", got.Content)
	}
	if got.Content != "" {
		t.Errorf("Content = %q, want empty", got.Content)
	}
	if got.OmitReason != OmitImage {
		t.Errorf("OmitReason = %q, want %q", got.OmitReason, OmitImage)
	}
	if got.Length != len(raw) {
		t.Errorf("Length = %d, want %d — the length must survive even though the content does not", got.Length, len(raw))
	}
}

// TestResultFromJSON_ImageBesideTextStillOmitted covers a mixed content array.
// One image block is enough: storing the sibling text while silently dropping
// the image would make the stored result a misleading partial.
func TestResultFromJSON_ImageBesideTextStillOmitted(t *testing.T) {
	raw := json.RawMessage(`[{"type":"text","text":"here is the screenshot"},{"type":"image","source":{"data":"AAAA"}}]`)

	got := ResultFromJSON("Bash", raw)

	if got.Content != "" {
		t.Errorf("Content = %q, want empty", got.Content)
	}
	if got.OmitReason != OmitImage {
		t.Errorf("OmitReason = %q, want %q", got.OmitReason, OmitImage)
	}
}

// TestResultFromJSON_BulkReadKeepsLengthOnly covers the bulk file readers. The
// file path is already extracted onto the row, so a length is enough to keep
// the row self-describing, and the full text stays in raw_json.
func TestResultFromJSON_BulkReadKeepsLengthOnly(t *testing.T) {
	for _, tool := range []string{"Read", "NotebookRead"} {
		body := strings.Repeat("x", 5000)
		raw := json.RawMessage(`"` + body + `"`)

		got := ResultFromJSON(tool, raw)

		if got.Content != "" {
			t.Errorf("%s: Content length = %d, want empty", tool, len(got.Content))
		}
		if got.OmitReason != OmitBulkRead {
			t.Errorf("%s: OmitReason = %q, want %q", tool, got.OmitReason, OmitBulkRead)
		}
		if got.Length != len(body) {
			t.Errorf("%s: Length = %d, want %d", tool, got.Length, len(body))
		}
	}
}

// TestResultFromJSON_BackstopOmitsOversize covers the 128 KB backstop. No
// non-bulk result in the measured archive comes close to it, so this is the
// unknown-future-tool case: a tool nobody has classified that returns
// something enormous must not be stored or indexed.
func TestResultFromJSON_BackstopOmitsOversize(t *testing.T) {
	body := strings.Repeat("y", MaxStoredResultBytes+1)
	got := ResultFromJSON("SomeFutureTool", json.RawMessage(`"`+body+`"`))

	if got.Content != "" {
		t.Errorf("Content length = %d, want empty", len(got.Content))
	}
	if got.OmitReason != OmitOversize {
		t.Errorf("OmitReason = %q, want %q", got.OmitReason, OmitOversize)
	}
	if got.Length != len(body) {
		t.Errorf("Length = %d, want %d", got.Length, len(body))
	}
}

// TestResultFromJSON_AtBackstopStoredWhole pins the boundary: the backstop is
// a ceiling the policy allows, not one it rejects.
func TestResultFromJSON_AtBackstopStoredWhole(t *testing.T) {
	body := strings.Repeat("z", MaxStoredResultBytes)
	got := ResultFromJSON("SomeFutureTool", json.RawMessage(`"`+body+`"`))

	if got.OmitReason != "" {
		t.Errorf("OmitReason = %q, want empty at exactly the backstop", got.OmitReason)
	}
	if len(got.Content) != MaxStoredResultBytes {
		t.Errorf("stored %d bytes, want %d", len(got.Content), MaxStoredResultBytes)
	}
}

// TestResultFromJSON_EmptyResultIsNotAnOmission separates "the tool returned
// nothing" from "the content was left out", which is the distinction issue #28
// asks a consumer to be able to make without guessing.
func TestResultFromJSON_EmptyResultIsNotAnOmission(t *testing.T) {
	got := ResultFromJSON("Bash", json.RawMessage(`""`))

	if got.OmitReason != "" {
		t.Errorf("OmitReason = %q, want empty", got.OmitReason)
	}
	if got.Length != 0 {
		t.Errorf("Length = %d, want 0", got.Length)
	}
}

// TestResultFromJSON_MalformedContentKeepsItsLength covers content the policy
// cannot decode. It must not be stored (nothing sane to store) but the length
// is still a fact worth keeping.
func TestResultFromJSON_MalformedContentKeepsItsLength(t *testing.T) {
	raw := json.RawMessage(`{"unexpected":"object"}`)
	got := ResultFromJSON("Bash", raw)

	if got.Content != "" {
		t.Errorf("Content = %q, want empty", got.Content)
	}
	if got.Length != len(raw) {
		t.Errorf("Length = %d, want %d", got.Length, len(raw))
	}
	if got.OmitReason != OmitUndecodable {
		t.Errorf("OmitReason = %q, want %q", got.OmitReason, OmitUndecodable)
	}
}

// TestResultFromText covers the adapters whose transcripts carry a tool result
// as a bare string (codex, jeff) rather than a Claude Code content block. The
// same policy applies, minus the shapes that cannot occur.
func TestResultFromText(t *testing.T) {
	got := ResultFromText("exec_command", "Process exited with code 0")
	if got.Content != "Process exited with code 0" || got.Length != 26 || got.OmitReason != "" {
		t.Errorf("ResultFromText = %+v", got)
	}

	big := ResultFromText("exec_command", strings.Repeat("q", MaxStoredResultBytes+1))
	if big.OmitReason != OmitOversize || big.Content != "" {
		t.Errorf("oversize text result = %+v", big)
	}

	bulk := ResultFromText("Read", "file contents")
	if bulk.OmitReason != OmitBulkRead || bulk.Content != "" {
		t.Errorf("bulk text result = %+v", bulk)
	}
}

// TestMaxStoredResultBytes guards the backstop against a well-meaning edit.
// It is set from measurement: the largest non-bulk, non-image result in the
// author's 965,061-turn archive is 51,557 bytes, so this leaves 2.5x headroom
// and fires on nothing that exists today.
func TestMaxStoredResultBytes(t *testing.T) {
	if MaxStoredResultBytes != 128*1024 {
		t.Errorf("MaxStoredResultBytes = %d, want %d", MaxStoredResultBytes, 128*1024)
	}
}
