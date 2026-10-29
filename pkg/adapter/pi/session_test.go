// ABOUTME: Tests for the session-level fields the pi adapter reads out of a transcript.
// ABOUTME: Covers id namespacing, project path, model resolution and the session time range.

package pi

import (
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/2389-research/ccvault/pkg/adapter"
)

func parseLines(t *testing.T, lines ...string) *adapter.ParsedSession {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.jsonl")
	writeSession(t, path, lines...)
	ps, err := New().Parse(path)
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	return ps
}

// userMessage returns a pi message entry carrying one text block, with the
// Unix-millisecond timestamp pi writes inside the message and the RFC3339
// timestamp it writes on the entry.
func userMessage(text string, unixMillis int64) string {
	return `{"type":"message","id":"u` + strconv.FormatInt(unixMillis, 10) +
		`","parentId":null,"timestamp":"` + time.UnixMilli(unixMillis).UTC().Format(time.RFC3339Nano) +
		`","message":{"role":"user","content":[{"type":"text","text":"` + text +
		`"}],"timestamp":` + strconv.FormatInt(unixMillis, 10) + `}}`
}

// TestParse_SessionIDIsNamespaced keeps pi session ids off other sources'
// rows. pi names a session with a UUID, which is the same shape Claude Code
// uses, and sessions share one primary key across every source.
func TestParse_SessionIDIsNamespaced(t *testing.T) {
	got := parseLines(t, header("ffae836b-9420-4060-ac13-7745215f90ff", "/Users/dyl/work/alpha"))
	if got.ID != "pi:ffae836b-9420-4060-ac13-7745215f90ff" {
		t.Errorf("ID = %q, want %q", got.ID, "pi:ffae836b-9420-4060-ac13-7745215f90ff")
	}
	if got.SourceName != "pi" {
		t.Errorf("SourceName = %q, want %q", got.SourceName, "pi")
	}
}

// TestParse_AHeaderlessFileHasNoSessionID matches what sync does with the
// result: a parse with no ID is recorded as skipped rather than written.
func TestParse_AHeaderlessFileHasNoSessionID(t *testing.T) {
	got := parseLines(t, userMessage("hello", 1765233665292))
	if got.ID != "" {
		t.Errorf("ID = %q, want empty for a file with no session header", got.ID)
	}
}

func TestParse_ProjectPathAndDisplayNameComeFromTheHeaderCWD(t *testing.T) {
	got := parseLines(t, header("a1", "/Users/dyl/work/alpha"))
	if got.ProjectPath != "/Users/dyl/work/alpha" {
		t.Errorf("ProjectPath = %q, want %q", got.ProjectPath, "/Users/dyl/work/alpha")
	}
	if got.DisplayName != "alpha" {
		t.Errorf("DisplayName = %q, want %q", got.DisplayName, "alpha")
	}
}

// TestParse_ModelComesFromTheV1HeaderWhenThatIsAllThereIs covers the sessions
// pi wrote before it moved the model into its own entry. Both of the real
// sessions pi ships as fixtures are this shape.
func TestParse_ModelComesFromTheV1HeaderWhenThatIsAllThereIs(t *testing.T) {
	got := parseLines(t, header("a1", "/Users/dyl/work/alpha"))
	if got.Model != "anthropic/claude-opus-4-5" {
		t.Errorf("Model = %q, want %q", got.Model, "anthropic/claude-opus-4-5")
	}
}

// TestParse_TheLastModelChangeWins is what a session that switched models
// mid-conversation should report, and matches how the codex adapter resolves
// the same question.
func TestParse_TheLastModelChangeWins(t *testing.T) {
	got := parseLines(t,
		header("a1", "/Users/dyl/work/alpha"),
		`{"type":"model_change","id":"e1","parentId":null,"timestamp":"2025-12-09T00:54:00.000Z","provider":"anthropic","modelId":"claude-sonnet-4-5"}`,
		`{"type":"model_change","id":"e2","parentId":"e1","timestamp":"2025-12-09T00:55:00.000Z","provider":"openai","modelId":"gpt-5"}`,
	)
	if got.Model != "openai/gpt-5" {
		t.Errorf("Model = %q, want %q", got.Model, "openai/gpt-5")
	}
}

// TestParse_SessionTimeRangeSpansEveryEntry uses the minimum and maximum
// rather than the first and last line, because pi's timestamps are not
// monotonic in file order: a forked session replays its ancestor's entries,
// which are older than its own header.
func TestParse_SessionTimeRangeSpansEveryEntry(t *testing.T) {
	got := parseLines(t,
		header("a1", "/Users/dyl/work/alpha"), // 2025-12-09T00:53:29.825Z
		`{"type":"thinking_level_change","id":"e1","parentId":null,"timestamp":"2025-12-08T22:45:42.397Z","thinkingLevel":"minimal"}`,
		`{"type":"model_change","id":"e2","parentId":"e1","timestamp":"2025-12-09T01:10:00.000Z","provider":"anthropic","modelId":"claude-opus-4-5"}`,
	)

	wantStart := time.Date(2025, 12, 8, 22, 45, 42, 397000000, time.UTC)
	wantEnd := time.Date(2025, 12, 9, 1, 10, 0, 0, time.UTC)
	if !got.StartedAt.Equal(wantStart) {
		t.Errorf("StartedAt = %v, want %v", got.StartedAt, wantStart)
	}
	if !got.EndedAt.Equal(wantEnd) {
		t.Errorf("EndedAt = %v, want %v", got.EndedAt, wantEnd)
	}
}

// TestParse_AForkIsNotRecordedAsAParentSession pins a mapping deliberately
// not made. pi's branchedFrom names the session this one was forked from;
// sessions.parent_session_id means the session that dispatched this one and is
// rendered as "subagent of X". Writing a fork there would label every resumed
// pi session a subagent.
func TestParse_AForkIsNotRecordedAsAParentSession(t *testing.T) {
	got := parseLines(t,
		`{"type":"session","id":"a1","timestamp":"2025-12-09T00:53:29.825Z","cwd":"/Users/dyl/work/alpha","provider":"anthropic","modelId":"claude-opus-4-5","branchedFrom":"/Users/dyl/.pi/agent/sessions/--Users-dyl-work-alpha--/2025-12-09T00-52-54-397Z_d97339c6-6c10-4827-846b-9ff1d9c3dc37.jsonl"}`,
	)

	if _, ok := got.Metadata["parent_session_id"]; ok {
		t.Error("Metadata carries parent_session_id for a fork; that column means the session that dispatched this one")
	}
	if got.Metadata["pi_forked_from"] != "d97339c6-6c10-4827-846b-9ff1d9c3dc37" {
		t.Errorf("Metadata[pi_forked_from] = %v, want %q",
			got.Metadata["pi_forked_from"], "d97339c6-6c10-4827-846b-9ff1d9c3dc37")
	}
}
