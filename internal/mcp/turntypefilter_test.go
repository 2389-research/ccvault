// ABOUTME: Tests that get_turns' advertised type filter matches what the filter actually accepts, which is any stored type.
// ABOUTME: The enum used to name three types, two of which have no rows at all, while the types with rows went unadvertised.

package mcp

import (
	"strings"
	"testing"
	"time"

	"github.com/2389-research/ccvault/internal/db"
	"github.com/2389-research/ccvault/pkg/models"
)

// getTurnsTypeProperty is the `type` parameter of get_turns exactly as a client
// reads it off tools/list.
func getTurnsTypeProperty(t *testing.T, s *Server) property {
	t.Helper()

	for _, tl := range advertisedTools(t, s) {
		if tl.Name != "get_turns" {
			continue
		}
		prop, ok := tl.InputSchema.Properties["type"]
		if !ok {
			t.Fatal("get_turns advertises no `type` parameter, but getTurns filters on one")
		}
		return prop
	}
	t.Fatal("tools/list does not advertise get_turns")
	return property{}
}

// TestGetTurns_TypeFilterAdvertisesNoEnum pins the schema against the filter.
//
// The enum used to read ["user", "assistant", "tool_result"]. Measured on the
// author's 50,000-session archive, `tool_result` has no rows at all — it was
// declared in the design doc and no source emits it — while `system` (58,230
// rows) and `attachment` (26,971) do and went unadvertised. The filter itself
// is `AND type = ?` and accepts any string, so the enum named a capability
// that did not exist and withheld two that did, and it contradicted
// get_session_summary beside it, which reports every type a session holds.
//
// Dropping the constraint rather than relisting the types is deliberate. A
// list of type names is exactly what #179 and the content fallback removed
// from the renderers, and reintroducing one here puts it in the hardest place
// to keep current: a schema an agent reads and believes. An adapter can add a
// type at any time, and an enum naming types a *given* archive may not even
// contain is its own kind of wrong. get_session_summary already answers the
// question properly, per session and from the rows.
func TestGetTurns_TypeFilterAdvertisesNoEnum(t *testing.T) {
	s, _ := newTestServer(t)

	prop := getTurnsTypeProperty(t, s)
	if len(prop.Enum) != 0 {
		t.Errorf("get_turns still constrains `type` to %v, but the filter accepts any stored type", prop.Enum)
	}
	// Dropping the constraint only helps if the description says where the
	// real answer lives, or an agent is left guessing what to pass.
	if !strings.Contains(prop.Description, "get_session_summary") {
		t.Errorf("the `type` description does not point at get_session_summary:\n%s", prop.Description)
	}
}

// TestGetTurns_FiltersATypeTheEnumNeverNamed is the behaviour the enum was
// wrong about. `system` works and always did — the author verified
// type:"system" returning 532 turns with full content on the live archive —
// which is what made the enum a false statement rather than an unimplemented
// feature.
//
// The second case is a type no adapter in the tree writes. It filters too,
// because the filter never knew any type names, and asserting it is what
// stops a future reader from "fixing" the schema by relisting today's types.
func TestGetTurns_FiltersATypeTheEnumNeverNamed(t *testing.T) {
	for _, tc := range []struct {
		turnType string
		content  string
	}{
		{"system", "[SCHEDULED TASK] You are Hal, a homelab manager."},
		{"handoff", "handing the task to the review agent"},
	} {
		t.Run(tc.turnType, func(t *testing.T) {
			s, database := newTestServer(t)
			seedTypedTurns(t, database, "sess-types", tc.turnType, tc.content)

			result, err := s.getTurns(map[string]interface{}{
				"session_id": "sess-types",
				"type":       tc.turnType,
			})
			if err != nil {
				t.Fatalf("getTurns with type=%q: %v", tc.turnType, err)
			}
			m := resultMap(t, result)

			turns := mustField[[]map[string]interface{}](t, m, "turns")
			if len(turns) != 1 {
				t.Fatalf("type=%q returned %d turns, want the 1 of that type", tc.turnType, len(turns))
			}
			if got := turns[0]["content"]; got != tc.content {
				t.Errorf("type=%q returned content %q, want %q", tc.turnType, got, tc.content)
			}
		})
	}
}

// seedTypedTurns writes a session holding one turn of turnType and one user
// turn, so a filter that ignored its argument would return two and fail.
func seedTypedTurns(t *testing.T, database *db.DB, sessionID, turnType, content string) {
	t.Helper()

	ts := time.Date(2026, 8, 14, 19, 25, 27, 0, time.UTC)
	if err := database.UpsertSession(&models.Session{
		ID: sessionID, StartedAt: ts, SourceFile: "/tmp/" + sessionID + ".jsonl", Source: "nanoclaw",
	}); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	if err := database.InsertTurns([]models.Turn{
		{ID: sessionID + "-t0", SessionID: sessionID, Type: turnType, Timestamp: ts, Ordinal: 0, Content: content},
		{ID: sessionID + "-t1", SessionID: sessionID, Type: "user", Timestamp: ts, Ordinal: 1, Content: "and a user turn"},
	}); err != nil {
		t.Fatalf("insert turns: %v", err)
	}
}
