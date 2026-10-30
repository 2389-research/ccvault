// ABOUTME: Tests for how the pi adapter turns session entries into ccvault turns.
// ABOUTME: Covers role mapping, turn order, turn ids on both v1 and v3 sessions, and content.

package pi

import (
	"testing"
)

const alpha = `{"type":"session","version":3,"id":"a1","timestamp":"2025-12-09T00:53:29.825Z","cwd":"/Users/dyl/work/alpha"}`

// alphaV1 is the header pi wrote before it put ids on entries. Both of the
// real sessions pi ships as fixtures are this shape.
const alphaV1 = `{"type":"session","id":"a1","timestamp":"2025-12-09T00:53:29.825Z","cwd":"/Users/dyl/work/alpha","provider":"anthropic","modelId":"claude-opus-4-5","thinkingLevel":"off"}`

func TestParse_UserAndAssistantMessagesBecomeTurnsInFileOrder(t *testing.T) {
	got := parseLines(t, alpha,
		`{"type":"message","id":"e1","parentId":null,"timestamp":"2025-12-09T00:54:00.000Z","message":{"role":"user","content":[{"type":"text","text":"read main.ts"}],"timestamp":1765233665292}}`,
		`{"type":"message","id":"e2","parentId":"e1","timestamp":"2025-12-09T00:54:05.000Z","message":{"role":"assistant","content":[{"type":"text","text":"on it"}],"api":"anthropic-messages","provider":"anthropic","model":"claude-opus-4-5","stopReason":"stop","timestamp":1765233665294,"usage":{"input":10,"output":2,"cacheRead":0,"cacheWrite":0,"totalTokens":12,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}}}}`,
		`{"type":"message","id":"e3","parentId":"e2","timestamp":"2025-12-09T00:54:09.000Z","message":{"role":"user","content":[{"type":"text","text":"thanks"}],"timestamp":1765233665300}}`,
	)

	if len(got.Turns) != 3 {
		t.Fatalf("got %d turns, want 3", len(got.Turns))
	}
	wantTypes := []string{"user", "assistant", "user"}
	wantContent := []string{"read main.ts", "on it", "thanks"}
	for i, turn := range got.Turns {
		if turn.Type != wantTypes[i] {
			t.Errorf("turn %d Type = %q, want %q", i, turn.Type, wantTypes[i])
		}
		if turn.Content != wantContent[i] {
			t.Errorf("turn %d Content = %q, want %q", i, turn.Content, wantContent[i])
		}
		if len(turn.RawJSON) == 0 {
			t.Errorf("turn %d has no RawJSON", i)
		}
	}
}

// TestParse_TurnIDsComeFromTheEntryAndCarryTheSession keeps pi's 8-hex entry
// ids off other sessions' rows: they are unique within one session's tree, not
// across the archive.
func TestParse_TurnIDsComeFromTheEntryAndCarryTheSession(t *testing.T) {
	got := parseLines(t, alpha,
		`{"type":"message","id":"3f2a91bc","parentId":null,"timestamp":"2025-12-09T00:54:00.000Z","message":{"role":"user","content":[{"type":"text","text":"hi"}],"timestamp":1765233665292}}`,
		`{"type":"message","id":"c0ffee11","parentId":"3f2a91bc","timestamp":"2025-12-09T00:54:05.000Z","message":{"role":"assistant","content":[{"type":"text","text":"hello"}],"timestamp":1765233665294}}`,
	)

	if len(got.Turns) != 2 {
		t.Fatalf("got %d turns, want 2", len(got.Turns))
	}
	if got.Turns[0].ID != "pi:a1-3f2a91bc" {
		t.Errorf("turn 0 ID = %q, want %q", got.Turns[0].ID, "pi:a1-3f2a91bc")
	}
	if got.Turns[0].ParentID != "" {
		t.Errorf("turn 0 ParentID = %q, want empty for a null parentId", got.Turns[0].ParentID)
	}
	if got.Turns[1].ID != "pi:a1-c0ffee11" {
		t.Errorf("turn 1 ID = %q, want %q", got.Turns[1].ID, "pi:a1-c0ffee11")
	}
	if got.Turns[1].ParentID != "pi:a1-3f2a91bc" {
		t.Errorf("turn 1 ParentID = %q, want %q", got.Turns[1].ParentID, "pi:a1-3f2a91bc")
	}
}

// TestParse_V1TurnIDsAreSynthesizedAndChained covers the sessions pi wrote
// before v2 added the tree. Those files carry no id and no parentId on any
// entry — pi mints them in memory on open — so the adapter has to name the
// turns itself, and it chains each to the one before it the way pi's own v1
// migration does.
func TestParse_V1TurnIDsAreSynthesizedAndChained(t *testing.T) {
	got := parseLines(t, alphaV1,
		`{"type":"message","timestamp":"2025-12-09T00:54:00.000Z","message":{"role":"user","content":[{"type":"text","text":"hi"}],"timestamp":1765233665292}}`,
		`{"type":"message","timestamp":"2025-12-09T00:54:05.000Z","message":{"role":"assistant","content":[{"type":"text","text":"hello"}],"timestamp":1765233665294}}`,
	)

	if len(got.Turns) != 2 {
		t.Fatalf("got %d turns, want 2", len(got.Turns))
	}
	if got.Turns[0].ID != "pi:a1-turn-1" {
		t.Errorf("turn 0 ID = %q, want %q", got.Turns[0].ID, "pi:a1-turn-1")
	}
	if got.Turns[0].ParentID != "" {
		t.Errorf("turn 0 ParentID = %q, want empty", got.Turns[0].ParentID)
	}
	if got.Turns[1].ID != "pi:a1-turn-2" {
		t.Errorf("turn 1 ID = %q, want %q", got.Turns[1].ID, "pi:a1-turn-2")
	}
	if got.Turns[1].ParentID != "pi:a1-turn-1" {
		t.Errorf("turn 1 ParentID = %q, want %q", got.Turns[1].ParentID, "pi:a1-turn-1")
	}
}

// TestParse_TurnTimestampComesFromTheEntry uses the entry's RFC3339 timestamp
// rather than the Unix-millisecond one inside the message. They are the same
// instant; only one of them is the shape every other entry type uses.
func TestParse_TurnTimestampComesFromTheEntry(t *testing.T) {
	got := parseLines(t, alpha,
		`{"type":"message","id":"e1","parentId":null,"timestamp":"2025-12-09T00:54:00.000Z","message":{"role":"user","content":[{"type":"text","text":"hi"}],"timestamp":1765233665292}}`,
	)
	if len(got.Turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(got.Turns))
	}
	if ts := got.Turns[0].Timestamp.UTC().Format("2006-01-02T15:04:05Z"); ts != "2025-12-09T00:54:00Z" {
		t.Errorf("turn Timestamp = %q, want %q", ts, "2025-12-09T00:54:00Z")
	}
}

// TestParse_AssistantContentKeepsThinkingText matches what the Claude Code
// adapter stores, so a search for something the model only said while thinking
// finds the turn it said it in.
func TestParse_AssistantContentKeepsThinkingText(t *testing.T) {
	got := parseLines(t, alpha,
		`{"type":"message","id":"e1","parentId":null,"timestamp":"2025-12-09T00:54:05.000Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"the loop is quadratic","thinkingSignature":"opaque"},{"type":"text","text":"I will rewrite the loop"}],"timestamp":1765233665294}}`,
	)
	if len(got.Turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(got.Turns))
	}
	want := "the loop is quadratic\n\nI will rewrite the loop"
	if got.Turns[0].Content != want {
		t.Errorf("Content = %q, want %q", got.Turns[0].Content, want)
	}
}

// TestParse_StringContentIsRead because pi's content field is either a string
// or a list of blocks, and both appear in the real sessions.
func TestParse_StringContentIsRead(t *testing.T) {
	got := parseLines(t, alpha,
		`{"type":"message","id":"e1","parentId":null,"timestamp":"2025-12-09T00:54:00.000Z","message":{"role":"user","content":"just a string","timestamp":1765233665292}}`,
	)
	if len(got.Turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(got.Turns))
	}
	if got.Turns[0].Content != "just a string" {
		t.Errorf("Content = %q, want %q", got.Turns[0].Content, "just a string")
	}
}

// TestParse_SystemMessageBecomesASystemTurn covers the prompt and the
// mid-conversation system messages that change it.
func TestParse_SystemMessageBecomesASystemTurn(t *testing.T) {
	got := parseLines(t, alpha,
		`{"type":"message","id":"e1","parentId":null,"timestamp":"2025-12-09T00:53:30.000Z","message":{"role":"system","content":"You are pi.","timestamp":1765233665000}}`,
	)
	if len(got.Turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(got.Turns))
	}
	if got.Turns[0].Type != "system" {
		t.Errorf("Type = %q, want %q", got.Turns[0].Type, "system")
	}
}

// TestParse_BashExecutionIsAUserTurn because pi's "!" command is the human
// running a shell command and putting its output into the conversation, and pi
// itself feeds it to the model as a user message.
func TestParse_BashExecutionIsAUserTurn(t *testing.T) {
	got := parseLines(t, alphaV1,
		`{"type":"message","timestamp":"2025-12-09T00:58:22.057Z","message":{"role":"bashExecution","command":"ls","output":"README.md\npackage.json","exitCode":0,"cancelled":false,"truncated":false,"timestamp":1765238302057}}`,
	)
	if len(got.Turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(got.Turns))
	}
	if got.Turns[0].Type != "user" {
		t.Errorf("Type = %q, want %q", got.Turns[0].Type, "user")
	}
	want := "$ ls\nREADME.md\npackage.json"
	if got.Turns[0].Content != want {
		t.Errorf("Content = %q, want %q", got.Turns[0].Content, want)
	}
}

func TestParse_AFailedBashExecutionRecordsItsExitCode(t *testing.T) {
	got := parseLines(t, alphaV1,
		`{"type":"message","timestamp":"2025-12-09T00:58:22.057Z","message":{"role":"bashExecution","command":"go test ./...","output":"FAIL","exitCode":1,"cancelled":false,"truncated":false,"timestamp":1765238302057}}`,
	)
	if len(got.Turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(got.Turns))
	}
	want := "$ go test ./...\nFAIL\n\n(exit 1)"
	if got.Turns[0].Content != want {
		t.Errorf("Content = %q, want %q", got.Turns[0].Content, want)
	}
}

// TestParse_InjectedMessagesBecomeSystemTurns covers the three roles that are
// in the conversation without anybody having typed them: an extension's
// sendMessage, and the two summaries pi writes when it compacts or returns
// from a branch. hookMessage is what "custom" was called before pi's v3
// rename, and sessions written then are still on disk.
func TestParse_InjectedMessagesBecomeSystemTurns(t *testing.T) {
	got := parseLines(t, alpha,
		`{"type":"message","id":"e1","parentId":null,"timestamp":"2025-12-09T00:54:00.000Z","message":{"role":"custom","customType":"reminder","content":"run the tests","display":true,"timestamp":1765233665292}}`,
		`{"type":"message","id":"e2","parentId":"e1","timestamp":"2025-12-09T00:54:01.000Z","message":{"role":"hookMessage","customType":"legacy","content":"older spelling","display":true,"timestamp":1765233665293}}`,
		`{"type":"message","id":"e3","parentId":"e2","timestamp":"2025-12-09T00:54:02.000Z","message":{"role":"compactionSummary","summary":"we refactored the loop","tokensBefore":120000,"timestamp":1765233665294}}`,
		`{"type":"message","id":"e4","parentId":"e3","timestamp":"2025-12-09T00:54:03.000Z","message":{"role":"branchSummary","summary":"the branch tried generics","fromId":"e2","timestamp":1765233665295}}`,
	)

	if len(got.Turns) != 4 {
		t.Fatalf("got %d turns, want 4", len(got.Turns))
	}
	wantContent := []string{"run the tests", "older spelling", "we refactored the loop", "the branch tried generics"}
	for i, turn := range got.Turns {
		if turn.Type != "system" {
			t.Errorf("turn %d Type = %q, want %q", i, turn.Type, "system")
		}
		if turn.Content != wantContent[i] {
			t.Errorf("turn %d Content = %q, want %q", i, turn.Content, wantContent[i])
		}
	}
}

// TestParse_StateOnlyEntriesAreNotTurns lists what pi records about itself
// rather than about the conversation. context_edit is here on purpose: it
// changes what a later model call sees, but the turn it edits still happened,
// and an archive that applied the edit would be claiming the original was
// never said.
func TestParse_StateOnlyEntriesAreNotTurns(t *testing.T) {
	got := parseLines(t, alpha,
		`{"type":"model_change","id":"e1","parentId":null,"timestamp":"2025-12-09T00:54:00.000Z","provider":"anthropic","modelId":"claude-opus-4-5"}`,
		`{"type":"thinking_level_change","id":"e2","parentId":"e1","timestamp":"2025-12-09T00:54:01.000Z","thinkingLevel":"minimal"}`,
		`{"type":"label","id":"e3","parentId":"e2","timestamp":"2025-12-09T00:54:02.000Z","targetId":"e1","label":"before the fix"}`,
		`{"type":"session_info","id":"e4","parentId":"e3","timestamp":"2025-12-09T00:54:03.000Z","name":"loop refactor"}`,
		`{"type":"custom","id":"e5","parentId":"e4","timestamp":"2025-12-09T00:54:04.000Z","customType":"artifact-index","data":{"files":2}}`,
		`{"type":"context_edit","id":"e6","parentId":"e5","timestamp":"2025-12-09T00:54:05.000Z","targetId":"e1","replacement":null}`,
	)

	if len(got.Turns) != 0 {
		t.Fatalf("got %d turns, want 0: %+v", len(got.Turns), got.Turns)
	}
}

// TestParse_CustomMessageEntriesBecomeSystemTurns separates the two things pi
// spells similarly: a "custom" entry is extension state and is not in the
// conversation, while a "custom_message" entry is content the model was shown.
func TestParse_CustomMessageEntriesBecomeSystemTurns(t *testing.T) {
	got := parseLines(t, alpha,
		`{"type":"custom_message","id":"e1","parentId":null,"timestamp":"2025-12-09T00:54:00.000Z","customType":"lint","content":"3 lint errors remain","display":true}`,
	)
	if len(got.Turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(got.Turns))
	}
	if got.Turns[0].Type != "system" {
		t.Errorf("Type = %q, want %q", got.Turns[0].Type, "system")
	}
	if got.Turns[0].Content != "3 lint errors remain" {
		t.Errorf("Content = %q, want %q", got.Turns[0].Content, "3 lint errors remain")
	}
}

// TestParse_MalformedLinesAreCountedNotDropped reports what sync aggregates
// into its skipped-lines warning, so a file pi half-wrote is visible rather
// than quietly short.
func TestParse_MalformedLinesAreCountedNotDropped(t *testing.T) {
	got := parseLines(t, alpha,
		`{"type":"message","id":"e1","parentId":null,"timestamp":"2025-12-09T00:54:00.000Z","message":{"role":"user","content":[{"type":"text","text":"hi"}],"timestamp":1765233665292}}`,
		`{"type":"message","id":"e2",`,
		`not json at all`,
	)

	if len(got.Turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(got.Turns))
	}
	if got.Metadata["skipped_lines"] != 2 {
		t.Errorf("Metadata[skipped_lines] = %v, want 2", got.Metadata["skipped_lines"])
	}
}

// TestParse_AnAssistantErrorMarksTheTurn so a session that failed mid-response
// is findable as one.
func TestParse_AnAssistantErrorMarksTheTurn(t *testing.T) {
	got := parseLines(t, alpha,
		`{"type":"message","id":"e1","parentId":null,"timestamp":"2025-12-09T00:54:05.000Z","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"overloaded_error","timestamp":1765233665294}}`,
	)
	if len(got.Turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(got.Turns))
	}
	if !got.Turns[0].HasError {
		t.Error("HasError = false, want true for an assistant message that stopped on an error")
	}
	if got.Metadata["has_error"] != true {
		t.Errorf("Metadata[has_error] = %v, want true", got.Metadata["has_error"])
	}
}
