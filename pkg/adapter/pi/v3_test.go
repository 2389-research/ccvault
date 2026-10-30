// ABOUTME: Tests for the v3 session shape, taken from sanitized sessions a pi user supplied on issue #178.
// ABOUTME: v3 puts an id and parentId on every entry and keeps no model in the header.

package pi

import (
	"testing"
)

// alphaV3 is a v3 header. Where the v1 header in alphaV1 keeps the model
// inline, this one carries no provider or modelId at all — v2 moved the model
// out into its own model_change entry — so a v3 session's model is only ever
// knowable from those entries.
const alphaV3 = `{"type":"session","version":3,"id":"m1","timestamp":"2026-01-05T09:00:00.000Z","cwd":"/Users/example/project"}`

// TestParse_V3ModelComesOnlyFromModelChange is the shape of every session a pi
// user sent for issue #178: a v3 header with no model in it at all, followed
// by a model_change that names one.
func TestParse_V3ModelComesOnlyFromModelChange(t *testing.T) {
	got := parseLines(t, alphaV3,
		`{"type":"model_change","id":"m2","parentId":null,"timestamp":"2026-01-05T09:00:01.000Z","provider":"example-provider","modelId":"example-model"}`,
		`{"type":"thinking_level_change","id":"m3","parentId":"m2","timestamp":"2026-01-05T09:00:02.000Z","thinkingLevel":"medium"}`,
		`{"type":"message","id":"m4","parentId":"m3","timestamp":"2026-01-05T09:00:03.000Z","message":{"role":"system","content":"a system prompt","timestamp":1767603603000}}`,
	)

	if got.ID != "pi:m1" {
		t.Errorf("ID = %q, want %q", got.ID, "pi:m1")
	}
	if got.Model != "example-provider/example-model" {
		t.Errorf("Model = %q, want %q", got.Model, "example-provider/example-model")
	}
	if len(got.Turns) != 1 {
		t.Fatalf("got %d turns, want 1 — only the message is a turn", len(got.Turns))
	}
	if got.Turns[0].ID != "pi:m1-m4" {
		t.Errorf("turn ID = %q, want %q", got.Turns[0].ID, "pi:m1-m4")
	}
	// m3 is a thinking_level_change: a parent that is not itself a turn. The
	// link is still recorded, because the entry pi named as the parent is the
	// entry pi named, and inventing a different one would be a guess.
	if got.Turns[0].ParentID != "pi:m1-m3" {
		t.Errorf("turn ParentID = %q, want %q", got.Turns[0].ParentID, "pi:m1-m3")
	}
}

// TestParse_RealUsageReadingAddsUpToPisOwnTotal is the empirical half of the
// subset rule. These five figures are one assistant message from a real
// session, and they settle the question the pi type declarations only assert:
//
//	input 10 + output 177 + cacheRead 1920 + cacheWrite 25216 = 27323
//
// which is exactly pi's own totalTokens, while reasoning sits at 58. So
// reasoning is inside output and adding it would overstate this one message by
// 58 tokens — and the session row sums all four counters, so it would overstate
// the session too (#155).
func TestParse_RealUsageReadingAddsUpToPisOwnTotal(t *testing.T) {
	const piTotalTokens = 27323

	got := parseLines(t, alphaV3,
		`{"type":"message","id":"m6","parentId":null,"timestamp":"2026-01-05T09:00:05.000Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"a thought","thinkingSignature":"opaque"},{"type":"toolCall","id":"m7","name":"read","arguments":{"path":"/Users/example/project/a.ts"}}],"timestamp":1767603605000,"usage":{"input":10,"output":177,"cacheRead":1920,"cacheWrite":25216,"reasoning":58,"totalTokens":27323,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}}}}`,
	)

	totals := got.TokenTotals()
	if totals.Total() != piTotalTokens {
		t.Errorf("Total() = %d, want %d to match pi's own totalTokens for this message",
			totals.Total(), piTotalTokens)
	}
	if totals.Output != 177 {
		t.Errorf("Output = %d, want 177 — the 58 reasoning tokens are already in it", totals.Output)
	}
}

// TestParse_ImageAndEmptyTextBlocksContributeNoText covers the blocks a
// supplied edge-case session holds: an assistant message carrying thinking,
// text and an image, and a second one whose only text block is empty.
//
// An image has no text to index, and an empty text block has none either, so
// the second turn has no content — and still exists, because the model did
// produce a message.
func TestParse_ImageAndEmptyTextBlocksContributeNoText(t *testing.T) {
	got := parseLines(t, alphaV3,
		`{"type":"message","id":"m6","parentId":null,"timestamp":"2026-01-05T10:00:03.000Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"a thought","thinkingSignature":"opaque"},{"type":"text","text":"an answer"},{"type":"image","mimeType":"image/png","data":"iVBORw0KGgo="}],"timestamp":1767607203000}}`,
		`{"type":"message","id":"m7","parentId":"m6","timestamp":"2026-01-05T10:00:04.000Z","message":{"role":"assistant","content":[{"type":"text","text":""}],"timestamp":1767607204000}}`,
	)

	if len(got.Turns) != 2 {
		t.Fatalf("got %d turns, want 2", len(got.Turns))
	}
	if got.Turns[0].Content != "a thought\n\nan answer" {
		t.Errorf("Content = %q, want the thinking and text joined, with nothing for the image",
			got.Turns[0].Content)
	}
	if got.Turns[1].Content != "" {
		t.Errorf("Content = %q, want empty for a lone empty text block", got.Turns[1].Content)
	}
}

// TestParse_UnicodeContentSurvives because the supplied sessions are German
// and a turn's text is what gets indexed for search.
func TestParse_UnicodeContentSurvives(t *testing.T) {
	got := parseLines(t, alphaV3,
		`{"type":"message","id":"m5","parentId":null,"timestamp":"2026-01-05T10:00:02.000Z","message":{"role":"user","content":"Beispieltext mit Umlauten: ä ö ü ß — und einem Emoji 🧪","timestamp":1767607202000}}`,
	)

	if len(got.Turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(got.Turns))
	}
	want := "Beispieltext mit Umlauten: ä ö ü ß — und einem Emoji 🧪"
	if got.Turns[0].Content != want {
		t.Errorf("Content = %q, want %q", got.Turns[0].Content, want)
	}
}

// TestParse_ResultsWhoseIDMatchesNothingAttachNothing is the rule holding up
// under the worst input it has had.
//
// One of the sessions supplied for #178 lost every toolCallId to its
// sanitiser: 21 calls kept their distinct ids while all 20 results were
// rewritten to one identical placeholder. The adapter attached none of them,
// which is the point — matching on the id alone means a result that identifies
// no call is dropped. An order-based fallback, which is what the jeff adapter
// needs for its own reasons, would instead have attached twenty results to the
// wrong twenty calls and made each one searchable under the wrong tool.
func TestParse_ResultsWhoseIDMatchesNothingAttachNothing(t *testing.T) {
	got := parseLines(t, alphaV3,
		`{"type":"message","id":"m6","parentId":null,"timestamp":"2026-01-05T09:00:05.000Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"m7","name":"read","arguments":{"path":"a.ts"}},{"type":"toolCall","id":"m8","name":"read","arguments":{"path":"b.ts"}}],"timestamp":1767603605000}}`,
		`{"type":"message","id":"m9","parentId":"m6","timestamp":"2026-01-05T09:00:06.000Z","message":{"role":"toolResult","toolCallId":"placeholder","toolName":"read","content":[{"type":"text","text":"first"}],"isError":false,"timestamp":1767603606000}}`,
		`{"type":"message","id":"m10","parentId":"m9","timestamp":"2026-01-05T09:00:07.000Z","message":{"role":"toolResult","toolCallId":"placeholder","toolName":"read","content":[{"type":"text","text":"second"}],"isError":false,"timestamp":1767603607000}}`,
	)

	if len(got.Turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(got.Turns))
	}
	uses := got.Turns[0].ToolUses
	if len(uses) != 2 {
		t.Fatalf("got %d tool uses, want 2 — both calls happened", len(uses))
	}
	for i, use := range uses {
		if use.HasResult {
			t.Errorf("tool use %d has result %q; no result identified this call", i, use.ResultContent)
		}
		if use.IsError != nil {
			t.Errorf("tool use %d IsError = %v, want nil", i, use.IsError)
		}
	}
}
