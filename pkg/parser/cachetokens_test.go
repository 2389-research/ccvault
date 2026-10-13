// ABOUTME: Tests that the Claude Code parser reads the two cache counters out of an assistant message's usage object.
// ABOUTME: Guards #155 — RawUsage parsed them, nothing assigned them, so the archive's cache columns were 0 everywhere.

package parser

import (
	"strings"
	"testing"
)

// assistantWithUsage is one assistant line carrying the full usage object a
// real Claude Code transcript writes. All four counters are distinct and
// non-zero on purpose: a fixture that leaves the cache fields out reads back
// as zero, which is also what the bug produced, so the test would have passed
// against it (#107's vacuity, which is how #155 was found).
const assistantWithUsage = `{"uuid":"a-1","sessionId":"s-1","type":"assistant","timestamp":"2026-01-01T00:00:00.000Z",` +
	`"message":{"role":"assistant","model":"claude-opus-4","content":[{"type":"text","text":"hi"}],` +
	`"usage":{"input_tokens":11,"output_tokens":7,"cache_read_input_tokens":4096,"cache_creation_input_tokens":512}}}`

func TestParseTurn_ReadsCacheCounters(t *testing.T) {
	turn, err := ParseTurn([]byte(assistantWithUsage))
	if err != nil {
		t.Fatalf("ParseTurn: %v", err)
	}
	if turn == nil {
		t.Fatal("ParseTurn returned no turn")
	}

	for _, tc := range []struct {
		field string
		got   int
		want  int
	}{
		{"InputTokens", turn.InputTokens, 11},
		{"OutputTokens", turn.OutputTokens, 7},
		{"CacheReadTokens", turn.CacheReadTokens, 4096},
		{"CacheWriteTokens", turn.CacheWriteTokens, 512},
	} {
		if tc.got != tc.want {
			t.Errorf("turn.%s = %d, want %d", tc.field, tc.got, tc.want)
		}
	}
}

// TestParseSessionReader_AccumulatesCacheCounters is the session-level half.
// The parser maintains running token totals in updateSessionMetadata, and the
// cache pair has to accumulate there alongside input/output or the session row
// stays zero however well the turns parsed.
func TestParseSessionReader_AccumulatesCacheCounters(t *testing.T) {
	// Two assistant turns so the test distinguishes "accumulated" from
	// "assigned from the last turn seen".
	jsonl := assistantWithUsage + "\n" +
		strings.Replace(assistantWithUsage, `"uuid":"a-1"`, `"uuid":"a-2"`, 1) + "\n"

	_, session, _, err := ParseSessionReader(strings.NewReader(jsonl), "s-1.jsonl")
	if err != nil {
		t.Fatalf("ParseSessionReader: %v", err)
	}

	if session.CacheReadTokens != 2*4096 {
		t.Errorf("session.CacheReadTokens = %d, want %d", session.CacheReadTokens, 2*4096)
	}
	if session.CacheWriteTokens != 2*512 {
		t.Errorf("session.CacheWriteTokens = %d, want %d", session.CacheWriteTokens, 2*512)
	}
	if got, want := session.TotalTokens(), int64(2*(11+7+4096+512)); got != want {
		t.Errorf("session.TotalTokens() = %d, want %d", got, want)
	}
}
