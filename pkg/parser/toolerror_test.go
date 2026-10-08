// ABOUTME: Tests that ExtractToolUses carries the tool_result is_error flag onto the call it answers (#83).
// ABOUTME: Fixtures are real Claude Code shapes — is_error beside content in the user turn's tool_result block.

package parser

import (
	"strings"
	"testing"
)

// extractIsErrorFrom returns the IsError tri-state of every call in a
// transcript, rendered as a string so a test failure says which of the three
// states came back rather than printing a pointer address.
func extractIsErrorFrom(t *testing.T, transcript string) []string {
	t.Helper()
	turns, _, _, err := ParseSessionReader(strings.NewReader(transcript), "/test/toolerror.jsonl")
	if err != nil {
		t.Fatalf("ParseSessionReader: %v", err)
	}
	var out []string
	for _, tu := range ExtractToolUses(turns) {
		switch {
		case tu.IsError == nil:
			out = append(out, "unknown")
		case *tu.IsError:
			out = append(out, "error")
		default:
			out = append(out, "ok")
		}
	}
	return out
}

// TestExtractToolUses_IsErrorTrue is issue #83's whole point: the flag the
// transcript already carries lands on the row, so "which calls failed" stops
// being a question you answer by guessing at error wording.
func TestExtractToolUses_IsErrorTrue(t *testing.T) {
	transcript := `{"uuid":"a-1","sessionId":"s-1","type":"assistant","timestamp":"2026-09-01T10:00:00.000Z","message":{"model":"claude","role":"assistant","content":[{"type":"tool_use","id":"toolu_FAIL","name":"Bash","input":{"command":"false"}}]}}
{"uuid":"u-1","sessionId":"s-1","type":"user","timestamp":"2026-09-01T10:00:01.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_FAIL","is_error":true,"content":"Error: command failed with exit code 1"}]}}`

	got := extractIsErrorFrom(t, transcript)
	want := []string{"error"}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("IsError states = %v, want %v", got, want)
	}
}

// TestExtractToolUses_IsErrorFalseIsNotUnknown pins the 0 end of the
// convention. 44,533 of the author's result blocks write is_error: false
// explicitly, and that is a positive statement of success — not the absence of
// an answer, which is what NULL is reserved for.
func TestExtractToolUses_IsErrorFalseIsNotUnknown(t *testing.T) {
	transcript := `{"uuid":"a-1","sessionId":"s-2","type":"assistant","timestamp":"2026-09-01T10:00:00.000Z","message":{"model":"claude","role":"assistant","content":[{"type":"tool_use","id":"toolu_OK","name":"Bash","input":{"command":"true"}}]}}
{"uuid":"u-1","sessionId":"s-2","type":"user","timestamp":"2026-09-01T10:00:01.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_OK","is_error":false,"content":"done"}]}}`

	got := extractIsErrorFrom(t, transcript)
	if len(got) != 1 || got[0] != "ok" {
		t.Errorf("IsError states = %v, want [ok]", got)
	}
}

// TestExtractToolUses_AbsentFlagOnAPresentResultIsSuccess covers the common
// shape: 217,581 of the author's 273,023 result blocks omit is_error entirely.
// Claude Code writes the key only when there is something to report, so a
// result that arrived and said nothing about failing succeeded. Reading those
// as unknown would make 80% of the archive unqueryable and the column close to
// useless.
func TestExtractToolUses_AbsentFlagOnAPresentResultIsSuccess(t *testing.T) {
	got := extractIsErrorFrom(t, twoTurnTranscript)
	if len(got) != 1 || got[0] != "ok" {
		t.Errorf("IsError states = %v, want [ok]", got)
	}
}

// TestExtractToolUses_NoResultIsUnknown is the NULL end. A call nothing
// answered has no error state to report, and 0 would claim it succeeded —
// which is the same distinction result_length already draws between "answered
// with nothing" and "never answered".
func TestExtractToolUses_NoResultIsUnknown(t *testing.T) {
	transcript := `{"uuid":"a-1","sessionId":"s-3","type":"assistant","timestamp":"2026-09-01T10:00:00.000Z","message":{"model":"claude","role":"assistant","content":[{"type":"tool_use","id":"toolu_ORPHAN","name":"Bash","input":{"command":"sleep 600"}}]}}`

	got := extractIsErrorFrom(t, transcript)
	if len(got) != 1 || got[0] != "unknown" {
		t.Errorf("IsError states = %v, want [unknown] — nothing answered this call", got)
	}
}

// TestExtractToolUses_IsErrorFollowsItsOwnCall covers the interleaving that
// makes a positional pairing wrong. Two calls in one turn, one failing, their
// results arriving out of order: the flag has to travel with the tool_use_id,
// or the archive reports the wrong call as the broken one.
func TestExtractToolUses_IsErrorFollowsItsOwnCall(t *testing.T) {
	transcript := `{"uuid":"a-1","sessionId":"s-4","type":"assistant","timestamp":"2026-09-01T10:00:00.000Z","message":{"model":"claude","role":"assistant","content":[{"type":"tool_use","id":"toolu_FIRST","name":"Bash","input":{"command":"one"}},{"type":"tool_use","id":"toolu_SECOND","name":"Grep","input":{"pattern":"two"}}]}}
{"uuid":"u-1","sessionId":"s-4","type":"user","timestamp":"2026-09-01T10:00:01.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_SECOND","content":"grep output"}]}}
{"uuid":"u-2","sessionId":"s-4","type":"user","timestamp":"2026-09-01T10:00:02.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_FIRST","is_error":true,"content":"bash exploded"}]}}`

	got := extractIsErrorFrom(t, transcript)
	want := []string{"error", "ok"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("IsError states = %v, want %v — the flag belongs to the call its tool_use_id names", got, want)
	}
}

// TestExtractToolUses_IsErrorSurvivesAnOmittedResult pins that the flag is
// independent of the storage policy. A failing Read has its content left out
// as bulk (and an image result as base64), and the one fact worth keeping about
// a call whose body is not stored is whether it failed.
func TestExtractToolUses_IsErrorSurvivesAnOmittedResult(t *testing.T) {
	transcript := `{"uuid":"a-1","sessionId":"s-5","type":"assistant","timestamp":"2026-09-01T10:00:00.000Z","message":{"model":"claude","role":"assistant","content":[{"type":"tool_use","id":"toolu_READ","name":"Read","input":{"file_path":"/nope"}}]}}
{"uuid":"u-1","sessionId":"s-5","type":"user","timestamp":"2026-09-01T10:00:01.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_READ","is_error":true,"content":"File does not exist."}]}}`

	turns, _, _, err := ParseSessionReader(strings.NewReader(transcript), "/test/toolerror.jsonl")
	if err != nil {
		t.Fatalf("ParseSessionReader: %v", err)
	}
	tus := ExtractToolUses(turns)
	if len(tus) != 1 {
		t.Fatalf("got %d tool uses, want 1", len(tus))
	}
	if tus[0].ResultOmittedReason == "" {
		t.Fatalf("expected the Read's content to be omitted, got reason %q", tus[0].ResultOmittedReason)
	}
	if tus[0].IsError == nil || !*tus[0].IsError {
		t.Error("IsError did not survive the omission — the flag is the most useful thing left on a row whose body is not stored")
	}
}

// TestExtractToolUses_IsErrorFromStructuredContent covers the other half of the
// archive's result shapes. 130,516 results carry content as an array of blocks
// rather than a string; is_error sits beside that array either way, so the
// extraction must not depend on the content's shape.
func TestExtractToolUses_IsErrorFromStructuredContent(t *testing.T) {
	transcript := `{"uuid":"a-1","sessionId":"s-6","type":"assistant","timestamp":"2026-09-01T10:00:00.000Z","message":{"model":"claude","role":"assistant","content":[{"type":"tool_use","id":"toolu_ARR","name":"Bash","input":{"command":"boom"}}]}}
{"uuid":"u-1","sessionId":"s-6","type":"user","timestamp":"2026-09-01T10:00:01.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_ARR","is_error":true,"content":[{"type":"text","text":"stderr said no"}]}]}}`

	got := extractIsErrorFrom(t, transcript)
	if len(got) != 1 || got[0] != "error" {
		t.Errorf("IsError states = %v, want [error]", got)
	}
}
