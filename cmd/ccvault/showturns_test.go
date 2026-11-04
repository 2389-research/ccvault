// ABOUTME: Tests that `ccvault show` prints every turn that carries text, not only the two types it was written for (#185).
// ABOUTME: Red first: the switch had no default, so a tool_result turn vanished from the session entirely.

package main

import (
	"strings"
	"testing"
	"time"

	"github.com/2389-research/ccvault/pkg/models"
)

// showTurn builds one stored turn for the printer.
func showTurn(turnType, content string) models.Turn {
	return models.Turn{
		ID:        turnType + "-1",
		SessionID: "sess-show",
		Type:      turnType,
		Timestamp: time.Date(2026, 10, 1, 17, 47, 24, 0, time.UTC),
		Content:   content,
	}
}

// TestPrintSessionTurns_ShowsEveryTypeThatCarriesText is #184's rule, which
// `ccvault show` never got: the switch here listed `user` and `assistant` and
// had no default, so every other type was dropped without a word.
//
// #185 is what made that cost something new. Retagging the archive's 315,985
// mislabeled tool results means `show` would stop printing them altogether —
// they used to appear, badly, as a `[USER]` header with a preview or nothing
// under it. Measured on the author's archive the same silence already swallows
// 51,794 nanoclaw `system` turns, each the agent prompt that explains why the
// session did anything.
func TestPrintSessionTurns_ShowsEveryTypeThatCarriesText(t *testing.T) {
	turns := []models.Turn{
		showTurn("user", "build the pct_deploy tool"),
		showTurn("assistant", "[Tool: Bash] $ grep -n pct src/ipc.ts"),
		showTurn("tool_result", "[Tool Result: (Bash completed with no output)]"),
		showTurn("system", "You are jo, a NanoClaw agent."),
	}

	out := captureStdout(t, func() { printSessionTurns(turns) })

	for _, want := range []string{
		"[USER]", "build the pct_deploy tool",
		"[ASSISTANT]", "grep -n pct src/ipc.ts",
		"[TOOL_RESULT]", "(Bash completed with no output)",
		"[SYSTEM]", "You are jo, a NanoClaw agent.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("show does not print %q:\n%s", want, out)
		}
	}
}

// TestPrintSessionTurns_SilentOnATypeWithNoText keeps the rule from turning
// into noise. ~169,000 of the archive's progress, attachment and claude-code
// system turns carry no content, and a header with nothing under it is the
// defect #185 reported, not the fix for it.
//
// `user` and `assistant` are exempt: those two have a renderer of their own and
// a reader counting the conversation's shape needs the header even when the
// turn's text is a pasted screenshot with no words in it.
func TestPrintSessionTurns_SilentOnATypeWithNoText(t *testing.T) {
	out := captureStdout(t, func() {
		printSessionTurns([]models.Turn{
			showTurn("progress", ""),
			showTurn("attachment", ""),
		})
	})

	if strings.TrimSpace(out) != "" {
		t.Errorf("contentless turns printed a header:\n%s", out)
	}
}

// TestPrintSessionTurns_TruncatesLongText pins the cut this printer already
// applied to an assistant turn, now that other types reach it too. A tool
// result is routinely tens of thousands of characters.
//
// Characters and not bytes: a byte cut can land inside a multi-byte rune and
// leave the terminal a fragment to draw.
func TestPrintSessionTurns_TruncatesLongText(t *testing.T) {
	out := captureStdout(t, func() {
		printSessionTurns([]models.Turn{showTurn("tool_result", strings.Repeat("あ", 900))})
	})

	if !strings.Contains(out, "truncated") {
		t.Errorf("a 900-character turn was not truncated:\n%s", out[:minInt(len(out), 200)])
	}
	if strings.ContainsRune(out, '\uFFFD') {
		t.Errorf("truncation split a rune and left a replacement character:\n%s", out)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
