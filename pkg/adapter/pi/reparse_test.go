// ABOUTME: Tests that a pi session's token counters survive a round trip through the archive's stored lines.
// ABOUTME: The cache-token backfill re-reads turns.raw_json, which never includes the session header.

package pi

import (
	"bytes"
	"testing"
)

// storedTranscript rebuilds the stream db.StoredTranscript hands the reparser:
// one line per turn in ordinal order, and nothing else. Any line that was
// never a turn — the session header, a model_change, a tool result — is simply
// not there.
func storedTranscript(t *testing.T, lines ...string) *bytes.Buffer {
	t.Helper()
	ps := parseLines(t, lines...)

	var buf bytes.Buffer
	for _, turn := range ps.Turns {
		buf.Write(turn.RawJSON)
		buf.WriteByte('\n')
	}
	return &buf
}

// TestParseTranscript_TokenTotalsSurviveTheArchiveRoundTrip is the property the
// #155 backfill rests on, and the reason the usage-bearing entries are stored
// as turns of their own.
//
// The stored stream has no session header — that line is not a turn — so the
// reparse cannot name the session or its turns. It can still add up what was
// spent, because every counter pi records lives on a line that became a turn.
// That is the difference from codex, whose token_count lines are folded into
// the preceding turn and therefore never reach raw_json at all, leaving every
// legacy codex session permanently unrepairable.
func TestParseTranscript_TokenTotalsSurviveTheArchiveRoundTrip(t *testing.T) {
	lines := []string{
		alpha,
		`{"type":"message","id":"e1","parentId":null,"timestamp":"2025-12-09T00:54:00.000Z","message":{"role":"user","content":[{"type":"text","text":"hi"}],"timestamp":1765233665292}}`,
		`{"type":"message","id":"e2","parentId":"e1","timestamp":"2025-12-09T00:54:05.000Z","message":{"role":"assistant","content":[{"type":"text","text":"hello"}],"stopReason":"stop","timestamp":1765233665294,"usage":{"input":10,"output":20,"cacheRead":30,"cacheWrite":40,"totalTokens":100,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}}}}`,
		`{"type":"usage","id":"e3","parentId":"e2","timestamp":"2025-12-09T00:54:06.000Z","kind":"cache_warm","provider":"anthropic","model":"claude-opus-4-5","usage":{"input":1,"output":2,"cacheRead":3,"cacheWrite":4,"totalTokens":10,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}}}`,
		`{"type":"model_change","id":"e4","parentId":"e3","timestamp":"2025-12-09T00:54:07.000Z","provider":"openai","modelId":"gpt-5"}`,
	}

	fromFile := parseLines(t, lines...)
	wantTotals := fromFile.TokenTotals()

	rederived, err := New().ParseTranscript("/Users/dyl/.pi/agent/sessions/--x--/s.jsonl", storedTranscript(t, lines...))
	if err != nil {
		t.Fatalf("ParseTranscript() error: %v", err)
	}

	if got := rederived.TokenTotals(); got != wantTotals {
		t.Errorf("re-derived TokenTotals() = %+v, want %+v", got, wantTotals)
	}
	if len(rederived.Turns) != len(fromFile.Turns) {
		t.Errorf("re-derived %d turns, want %d", len(rederived.Turns), len(fromFile.Turns))
	}
}

// TestParseTranscript_HasNoSessionIdentityWithoutItsHeader states the limit of
// the round trip plainly, so nobody later reads a re-derived session's ID and
// believes it.
//
// The backfill reads only TokenTotals() from the result and already knows which
// session it asked about, so this costs it nothing. A future caller that wants
// the identity back has to pass the header, not this stream.
func TestParseTranscript_HasNoSessionIdentityWithoutItsHeader(t *testing.T) {
	lines := []string{
		alpha,
		`{"type":"message","id":"e1","parentId":null,"timestamp":"2025-12-09T00:54:00.000Z","message":{"role":"user","content":[{"type":"text","text":"hi"}],"timestamp":1765233665292}}`,
	}

	rederived, err := New().ParseTranscript("/Users/dyl/.pi/agent/sessions/--x--/s.jsonl", storedTranscript(t, lines...))
	if err != nil {
		t.Fatalf("ParseTranscript() error: %v", err)
	}

	if rederived.ID != "" {
		t.Errorf("ID = %q, want empty — the header that carries it is not a turn", rederived.ID)
	}
	if rederived.ProjectPath != "" {
		t.Errorf("ProjectPath = %q, want empty for the same reason", rederived.ProjectPath)
	}
}
