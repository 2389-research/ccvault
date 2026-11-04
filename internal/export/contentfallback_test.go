// ABOUTME: Tests that a turn of a type the exporter has no case for exports its content, and is skipped when it has none.
// ABOUTME: Fixtures are the real shapes behind the archive's 51,794 nanoclaw system turns and its ~169,000 machinery turns.

package export

import (
	"strings"
	"testing"
	"time"

	"github.com/2389-research/ccvault/pkg/models"
)

// scheduledTaskPrompt is the start of a real nanoclaw scheduled-task prompt,
// which the nanoclaw adapter retags from `user` to `system` because its own
// scheduler injected it. There are 51,834 such turns in the author's archive
// and 51,794 of them carry content like this — each a 1,000+ character agent
// prompt (max 6,433, 424 distinct) explaining why the session did anything.
const scheduledTaskPrompt = "[SCHEDULED TASK - The following message was sent automatically " +
	"and is not coming directly from the user or group.]\n\n" +
	"You are Hal, a homelab manager. Your job is to clean apt cache and check disk usage."

// nanoclawSystemTurn is that turn as it is stored: type `system`, the prompt in
// the `content` column, and raw_json still saying `"type":"user"` because the
// retag happens above the parser.
func nanoclawSystemTurn() models.Turn {
	return models.Turn{
		ID:        "turn-sched",
		Type:      "system",
		Timestamp: time.Date(2026, 8, 14, 19, 25, 27, 0, time.UTC),
		Content:   scheduledTaskPrompt,
		RawJSON: []byte(`{"parentUuid":null,"isSidechain":false,` +
			`"promptId":"72363906-aace-41ef-9c83-91d977c8fc7c","type":"user",` +
			`"message":{"role":"user","content":` + jsonString(scheduledTaskPrompt) + `}}`),
	}
}

// jsonString quotes s as a JSON string literal, so the fixture above can embed
// a multi-line prompt without hand-escaping it.
func jsonString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return `"` + s + `"`
}

// machineryTurns are the types the exporter has no case for that carry nothing
// to export. Real raw_json, real counts from the author's archive:
// 131,581 claude-code progress turns, 26,971 attachment turns and 6,396
// claude-code system turns, every one of them with an empty content column.
func machineryTurns() []models.Turn {
	ts := time.Date(2026, 8, 14, 19, 25, 27, 0, time.UTC)
	return []models.Turn{
		{
			ID: "turn-prog", Type: "progress", Timestamp: ts,
			RawJSON: []byte(`{"parentUuid":null,"isSidechain":false,"type":"progress",` +
				`"data":{"type":"hook_progress","hookEvent":"PreToolUse"}}`),
		},
		{
			ID: "turn-attach", Type: "attachment", Timestamp: ts,
			RawJSON: []byte(`{"parentUuid":null,"isSidechain":false,` +
				`"attachment":{"type":"deferred_tools_delta","addedNames":["CronCreate"]}}`),
		},
		{
			ID: "turn-cc-system", Type: "system", Timestamp: ts,
			RawJSON: []byte(`{"parentUuid":null,"isSidechain":false,"type":"system",` +
				`"subtype":"turn_duration","durationMs":15943,"messageCount":20}`),
		},
	}
}

// TestExportTurn_UnhandledTypeWithContentExports is what recovers the 51,794
// nanoclaw system turns. Each is the agent prompt that explains why the
// session did anything, and export's switch had a case for three types and no
// default — so a nanoclaw transcript read as though the agent started acting
// for no reason.
//
// The rule is "has content", not "is a system turn". Nothing here special-cases
// the type name; #179's lesson was that a type-name switch is the bug.
func TestExportTurn_UnhandledTypeWithContentExports(t *testing.T) {
	out := renderTurn(t, NewMarkdownExporter(), nanoclawSystemTurn())

	if !strings.Contains(out, "You are Hal, a homelab manager") {
		t.Errorf("a system turn's prompt did not export:\n%s", out)
	}
	// Labelled with the type it is, so a reader can tell the prompt from the
	// conversation around it.
	if !strings.Contains(out, "system") {
		t.Errorf("the turn exported unlabelled:\n%s", out)
	}
}

// TestExportTurn_UnhandledTypeWithNoContentIsSkipped is the other half of the
// same rule, and the reason the rule is worth having: ~169,000 turns in the
// author's archive are machinery with an empty content column, and a default
// branch that printed a header for every one of them would bury the 51,794
// that say something.
func TestExportTurn_UnhandledTypeWithNoContentIsSkipped(t *testing.T) {
	for _, turn := range machineryTurns() {
		t.Run(turn.Type+"/"+turn.ID, func(t *testing.T) {
			if out := renderTurn(t, NewMarkdownExporter(), turn); out != "" {
				t.Errorf("a %s turn with no content exported:\n%s", turn.Type, out)
			}
		})
	}
}

// TestExportTurn_TheRuleIsContentNotTypeName pins the generality. A type no
// adapter in the tree writes exports when it carries text and stays quiet when
// it does not — which is the whole point of keying on content.
//
// Note "human", the value a vacuous fixture in this repo once used where real
// transcripts say "user" (#107). It is not a user turn, so it gets no 👤 User
// header: it falls through to the default like any other unknown type.
func TestExportTurn_TheRuleIsContentNotTypeName(t *testing.T) {
	for _, typ := range []string{"human", "handoff", "a-type-not-yet-written"} {
		t.Run(typ, func(t *testing.T) {
			withContent := renderTurn(t, NewMarkdownExporter(), models.Turn{
				ID: "turn-x", Type: typ, Content: "something a reader needs",
			})
			if !strings.Contains(withContent, "something a reader needs") {
				t.Errorf("type %q dropped its content:\n%s", typ, withContent)
			}
			if strings.Contains(withContent, "👤 User") {
				t.Errorf("type %q exported as a user turn:\n%s", typ, withContent)
			}

			empty := renderTurn(t, NewMarkdownExporter(), models.Turn{ID: "turn-y", Type: typ})
			if empty != "" {
				t.Errorf("type %q with no content exported:\n%s", typ, empty)
			}
		})
	}
}

// TestExportTurn_EmptyTypeWithContentStillExports covers the type ccvault
// itself can produce: a row whose type column is empty. It holds text or it
// does not, and the rule does not need to know how it got that way.
func TestExportTurn_EmptyTypeWithContentStillExports(t *testing.T) {
	out := renderTurn(t, NewMarkdownExporter(), models.Turn{
		ID: "turn-z", Type: "", Content: "untyped but not empty",
	})
	if !strings.Contains(out, "untyped but not empty") {
		t.Errorf("an untyped turn with content did not export:\n%s", out)
	}
}
