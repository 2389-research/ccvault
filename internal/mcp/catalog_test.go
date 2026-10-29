// ABOUTME: Tests that every tool and prompt the server advertises can actually be invoked.
// ABOUTME: Drives tools/list, prompts/list, tools/call and prompts/get over the injected reader.

package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/2389-research/ccvault/internal/db"
)

// requestOver sends one request through the full read->dispatch->write loop
// and returns the single reply. Uses the injected reader (#128) rather than
// calling the handler directly, so the method-name routing is exercised too.
func requestOver(t *testing.T, s *Server, line string) jsonRPCResponse {
	t.Helper()

	responses := runServerOver(t, s, line+"\n")
	if len(responses) != 1 {
		t.Fatalf("got %d replies, want exactly 1, for request %s", len(responses), line)
	}
	return responses[0]
}

// resultInto re-decodes a reply's Result into dst. The Result field arrives
// as a generic value because jsonRPCResponse is shared with the error path.
func resultInto(t *testing.T, resp jsonRPCResponse, dst any) {
	t.Helper()

	if resp.Error != nil {
		t.Fatalf("request failed: code=%d message=%q data=%v", resp.Error.Code, resp.Error.Message, resp.Error.Data)
	}
	raw, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatalf("re-marshal result: %v", err)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		t.Fatalf("decode result into %T: %v (raw %s)", dst, err, raw)
	}
}

func advertisedTools(t *testing.T, s *Server) []tool {
	t.Helper()

	var got toolsListResult
	resultInto(t, requestOver(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`), &got)
	if len(got.Tools) == 0 {
		t.Fatal("tools/list advertised nothing; a client would see a server with no capabilities")
	}
	return got.Tools
}

func advertisedPrompts(t *testing.T, s *Server) []prompt {
	t.Helper()

	var got promptsListResult
	resultInto(t, requestOver(t, s, `{"jsonrpc":"2.0","id":1,"method":"prompts/list"}`), &got)
	if len(got.Prompts) == 0 {
		t.Fatal("prompts/list advertised nothing; a client would see a server with no prompts")
	}
	return got.Prompts
}

// --- the catalog/dispatch cross-check -----------------------------------

// The contract that matters about tools/list: everything it advertises must
// be something tools/call will dispatch. The catalog and the switch in
// handleToolsCall are two separate lists that have to agree, and nothing but
// a test like this makes them. Add a tool to one and not the other and an
// agent discovers it, calls it, and gets "Unknown tool" -- a failure that
// looks like a client bug.
//
// Invoked with no arguments, so most tools answer with a tool-level error
// ("query is required"). That is fine and is not what this asserts: a
// tool-level error proves the call *reached* the implementation. The
// assertion is specifically that no reply is the JSON-RPC "Unknown tool"
// rejection, which is the one answer that means the catalog lied.
func TestToolsCall_EveryAdvertisedToolIsDispatchable(t *testing.T) {
	s, _ := newTestServer(t)

	for _, tl := range advertisedTools(t, s) {
		t.Run(tl.Name, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{
				"jsonrpc": "2.0", "id": 1, "method": "tools/call",
				"params": map[string]any{"name": tl.Name, "arguments": map[string]any{}},
			})
			if err != nil {
				t.Fatalf("marshal request: %v", err)
			}
			resp := requestOver(t, s, string(body))
			if resp.Error != nil && resp.Error.Message == "Unknown tool" {
				t.Errorf("tools/list advertises %q but tools/call does not dispatch it; "+
					"add it to the switch in handleToolsCall", tl.Name)
			}
		})
	}
}

// The same cross-check for prompts. prompts/get with no arguments returns a
// "Prompt error" for the four prompts that require one, which again proves
// the call reached the implementation. "Unknown prompt" is the answer that
// means prompts/list advertised something handlePromptsGet cannot serve.
func TestPromptsGet_EveryAdvertisedPromptIsGettable(t *testing.T) {
	s, _ := newTestServer(t)

	for _, p := range advertisedPrompts(t, s) {
		t.Run(p.Name, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{
				"jsonrpc": "2.0", "id": 1, "method": "prompts/get",
				"params": map[string]any{"name": p.Name, "arguments": map[string]any{}},
			})
			if err != nil {
				t.Fatalf("marshal request: %v", err)
			}
			resp := requestOver(t, s, string(body))
			if resp.Error != nil && resp.Error.Message == "Unknown prompt" {
				t.Errorf("prompts/list advertises %q but prompts/get does not serve it; "+
					"add it to the switch in handlePromptsGet", p.Name)
			}
		})
	}
}

// Every advertised tool needs a name, a description an agent can choose by,
// and an object-typed input schema -- the protocol requires the schema shape,
// and an agent cannot pick a tool it has no description for.
func TestToolsList_EveryToolCarriesAUsableDeclaration(t *testing.T) {
	s, _ := newTestServer(t)

	seen := map[string]bool{}
	for _, tl := range advertisedTools(t, s) {
		if tl.Name == "" {
			t.Error("a tool is advertised with an empty name")
			continue
		}
		if seen[tl.Name] {
			t.Errorf("tool %q is advertised twice; a client keying by name sees only one", tl.Name)
		}
		seen[tl.Name] = true
		if tl.Description == "" {
			t.Errorf("tool %q has no description, so an agent has nothing to choose it by", tl.Name)
		}
		if tl.InputSchema.Type != "object" {
			t.Errorf("tool %q has inputSchema.type %q, want %q", tl.Name, tl.InputSchema.Type, "object")
		}
	}
}

func TestPromptsList_EveryPromptCarriesAUsableDeclaration(t *testing.T) {
	s, _ := newTestServer(t)

	seen := map[string]bool{}
	for _, p := range advertisedPrompts(t, s) {
		if p.Name == "" {
			t.Error("a prompt is advertised with an empty name")
			continue
		}
		if seen[p.Name] {
			t.Errorf("prompt %q is advertised twice; a client keying by name sees only one", p.Name)
		}
		seen[p.Name] = true
		if p.Description == "" {
			t.Errorf("prompt %q has no description", p.Name)
		}
	}
}

// --- rejection paths ----------------------------------------------------

func TestToolsCall_UnknownToolIsRejectedAsInvalidParams(t *testing.T) {
	s, _ := newTestServer(t)

	resp := requestOver(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"no_such_tool"}}`)
	if resp.Error == nil {
		t.Fatalf("an unknown tool returned a result, not an error: %#v", resp.Result)
	}
	if resp.Error.Code != -32602 {
		t.Errorf("code = %d, want -32602 (invalid params)", resp.Error.Code)
	}
	if resp.Error.Data != "no_such_tool" {
		t.Errorf("data = %#v, want the offending name so the client can see which tool it asked for", resp.Error.Data)
	}
}

func TestPromptsGet_UnknownPromptIsRejectedAsInvalidParams(t *testing.T) {
	s, _ := newTestServer(t)

	resp := requestOver(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"prompts/get","params":{"name":"no_such_prompt"}}`)
	if resp.Error == nil {
		t.Fatalf("an unknown prompt returned a result, not an error: %#v", resp.Result)
	}
	if resp.Error.Code != -32602 {
		t.Errorf("code = %d, want -32602 (invalid params)", resp.Error.Code)
	}
	if resp.Error.Data != "no_such_prompt" {
		t.Errorf("data = %#v, want the offending name", resp.Error.Data)
	}
}

// Params of the wrong JSON shape must be rejected as invalid params rather
// than crashing the loop or being read as an empty request.
func TestPromptsGet_MalformedParamsAreRejected(t *testing.T) {
	s, _ := newTestServer(t)

	resp := requestOver(t, s, `{"jsonrpc":"2.0","id":1,"method":"prompts/get","params":"not-an-object"}`)
	if resp.Error == nil {
		t.Fatalf("malformed params returned a result, not an error: %#v", resp.Result)
	}
	if resp.Error.Code != -32602 {
		t.Errorf("code = %d, want -32602 (invalid params)", resp.Error.Code)
	}
}

func TestToolsCall_MalformedParamsAreRejected(t *testing.T) {
	s, _ := newTestServer(t)

	resp := requestOver(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":"not-an-object"}`)
	if resp.Error == nil {
		t.Fatalf("malformed params returned a result, not an error: %#v", resp.Result)
	}
	if resp.Error.Code != -32602 {
		t.Errorf("code = %d, want -32602 (invalid params)", resp.Error.Code)
	}
}

// The four prompts declaring a required argument must refuse to run without
// it, and the refusal must arrive as a prompt error (-32603) rather than as
// a successful prompt built on an empty topic -- a prompt asking the model to
// analyse "" is worse than an error, because the model will answer it.
func TestPromptsGet_RequiredArgumentIsEnforced(t *testing.T) {
	s, _ := newTestServer(t)

	for _, p := range advertisedPrompts(t, s) {
		var required []string
		for _, a := range p.Arguments {
			if a.Required {
				required = append(required, a.Name)
			}
		}
		if len(required) == 0 {
			continue
		}
		t.Run(p.Name, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{
				"jsonrpc": "2.0", "id": 1, "method": "prompts/get",
				"params": map[string]any{"name": p.Name, "arguments": map[string]any{}},
			})
			if err != nil {
				t.Fatalf("marshal request: %v", err)
			}
			resp := requestOver(t, s, string(body))
			if resp.Error == nil {
				t.Fatalf("prompt %q declares %v required but produced a prompt without it",
					p.Name, required)
			}
			if resp.Error.Code != -32603 {
				t.Errorf("code = %d, want -32603 (prompt error)", resp.Error.Code)
			}
		})
	}
}

// --- prompt bodies ------------------------------------------------------

// A prompt's whole purpose is the text it hands the model, so the assertion
// has to be that the conversation's content reached it. Checking only that
// Messages is non-empty would pass against a prompt built from an empty
// context block -- which is the failure mode the "the markdown IS the
// prompt" comments in server.go are about.
func TestPromptReviewSession_CarriesTheSessionsTurnsIntoTheMessage(t *testing.T) {
	s, database := newTestServer(t)
	seedSession(t, database, "review-me-0001", 1)

	var got promptGetResult
	resultInto(t, requestOver(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"prompts/get","params":`+
			`{"name":"review_session","arguments":{"session_id":"review-me-0001"}}}`), &got)

	if len(got.Messages) != 1 {
		t.Fatalf("got %d messages, want 1", len(got.Messages))
	}
	if got.Messages[0].Role != "user" {
		t.Errorf("role = %q, want user", got.Messages[0].Role)
	}
	// seedSession writes exactly this turn content.
	if !strings.Contains(got.Messages[0].Content.Text, "hello from review-me-0001") {
		t.Errorf("the session's turn text is missing from the prompt, so the model would be "+
			"asked to review an empty transcript; got:\n%s", got.Messages[0].Content.Text)
	}
}

func TestPromptReviewSession_UnknownSessionIsAnError(t *testing.T) {
	s, _ := newTestServer(t)

	resp := requestOver(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"prompts/get","params":`+
			`{"name":"review_session","arguments":{"session_id":"does-not-exist"}}}`)
	if resp.Error == nil {
		t.Fatalf("a missing session produced a prompt instead of an error: %#v", resp.Result)
	}
	if resp.Error.Code != -32603 {
		t.Errorf("code = %d, want -32603", resp.Error.Code)
	}
}

// Class D doctrine, at the prompt surface: an ambiguous filter must refuse
// rather than analyse whichever project sorted first, and the refusal has to
// name the candidates so the caller can narrow. Two projects sharing a
// basename is the real case this exists for.
func TestPromptAnalyzeProject_AmbiguousFilterRefusesAndNamesBothPaths(t *testing.T) {
	s, database := newTestServer(t)
	seedProject(t, database, "/Users/dyl/work/ccvault")
	seedProject(t, database, "/Users/dyl/personal/ccvault")

	resp := requestOver(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"prompts/get","params":`+
			`{"name":"analyze_project","arguments":{"project":"ccvault"}}}`)
	if resp.Error == nil {
		t.Fatalf("an ambiguous filter silently picked a project: %#v", resp.Result)
	}
	data, _ := resp.Error.Data.(string)
	for _, want := range []string{"/Users/dyl/work/ccvault", "/Users/dyl/personal/ccvault"} {
		if !strings.Contains(data, want) {
			t.Errorf("the refusal does not name %q, so the caller cannot narrow; got: %s", want, data)
		}
	}
}

func TestPromptAnalyzeProject_UnmatchedFilterIsAnError(t *testing.T) {
	s, database := newTestServer(t)
	seedProject(t, database, "/Users/dyl/work/ccvault")

	resp := requestOver(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"prompts/get","params":`+
			`{"name":"analyze_project","arguments":{"project":"nothing-matches-this"}}}`)
	if resp.Error == nil {
		t.Fatalf("an unmatched filter produced a prompt instead of an error: %#v", resp.Result)
	}
}

// A single match must go through and carry the project's identity, so the
// resolve-exactly-one path is covered as well as the two refusals.
func TestPromptAnalyzeProject_SingleMatchCarriesTheProjectPath(t *testing.T) {
	s, database := newTestServer(t)
	seedProject(t, database, "/Users/dyl/work/only-one")

	var got promptGetResult
	resultInto(t, requestOver(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"prompts/get","params":`+
			`{"name":"analyze_project","arguments":{"project":"only-one"}}}`), &got)

	if len(got.Messages) != 1 {
		t.Fatalf("got %d messages, want 1", len(got.Messages))
	}
	if !strings.Contains(got.Messages[0].Content.Text, "/Users/dyl/work/only-one") {
		t.Errorf("the project path is missing from the prompt; got:\n%s", got.Messages[0].Content.Text)
	}
}

// The days argument arrives as a JSON number (float64) and has to reach both
// the description and the body. Asserting on a non-default value is what
// distinguishes "the argument was read" from "the default happened to match".
func TestPromptSummarizeRecent_HonoursTheDaysArgument(t *testing.T) {
	s, database := newTestServer(t)
	seedSession(t, database, "summarize-0001", 1)

	var got promptGetResult
	resultInto(t, requestOver(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"prompts/get","params":`+
			`{"name":"summarize_recent","arguments":{"days":30}}}`), &got)

	if !strings.Contains(got.Description, "30") {
		t.Errorf("description = %q, want it to mention the 30 days asked for", got.Description)
	}
	if len(got.Messages) != 1 {
		t.Fatalf("got %d messages, want 1", len(got.Messages))
	}
	if !strings.Contains(got.Messages[0].Content.Text, "Last 30 Days") {
		t.Errorf("the body does not reflect the 30-day window; got:\n%s", got.Messages[0].Content.Text)
	}
}

func TestPromptSummarizeRecent_DefaultsToSevenDays(t *testing.T) {
	s, _ := newTestServer(t)

	var got promptGetResult
	resultInto(t, requestOver(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"prompts/get","params":`+
			`{"name":"summarize_recent","arguments":{}}}`), &got)

	if !strings.Contains(got.Description, "7") {
		t.Errorf("description = %q, want the documented 7-day default", got.Description)
	}
}

// find_solutions and compare_approaches both run a real FTS search. Seeding
// a turn whose content is the needle proves the search reached the index and
// the hit reached the prompt -- not merely that the handler returned.
func TestPromptFindSolutions_CarriesMatchingTurnsIntoTheMessage(t *testing.T) {
	s, database := newTestServer(t)
	seedSession(t, database, "solutions-0001", 1)

	var got promptGetResult
	resultInto(t, requestOver(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"prompts/get","params":`+
			`{"name":"find_solutions","arguments":{"topic":"hello"}}}`), &got)

	if len(got.Messages) != 1 {
		t.Fatalf("got %d messages, want 1", len(got.Messages))
	}
	text := got.Messages[0].Content.Text
	if !strings.Contains(text, "Found 1 relevant conversations") {
		t.Errorf("the seeded turn was not found by the search; got:\n%s", text)
	}
	if !strings.Contains(text, "solutions") {
		t.Errorf("the matching session is not identified in the prompt; got:\n%s", text)
	}
}

func TestPromptCompareApproaches_GroupsHitsBySession(t *testing.T) {
	s, database := newTestServer(t)
	seedSession(t, database, "compare-aaaa-01", 1)
	seedSession(t, database, "compare-bbbb-02", 1)

	var got promptGetResult
	resultInto(t, requestOver(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"prompts/get","params":`+
			`{"name":"compare_approaches","arguments":{"topic":"hello"}}}`), &got)

	if len(got.Messages) != 1 {
		t.Fatalf("got %d messages, want 1", len(got.Messages))
	}
	text := got.Messages[0].Content.Text
	// Two seeded sessions each contain "hello", so the grouping must report
	// two sessions rather than two undifferentiated hits.
	if !strings.Contains(text, "Found 2 sessions") {
		t.Errorf("hits were not grouped into 2 sessions; got:\n%s", text)
	}
}

// tool_usage_report takes an optional argument, so both branches are real
// behaviour: without it the report is the frequency table, with it the table
// gains a focus section naming the tool.
func TestPromptToolUsageReport_AddsAFocusSectionForTheNamedTool(t *testing.T) {
	s, _ := newTestServer(t)

	var without promptGetResult
	resultInto(t, requestOver(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"prompts/get","params":`+
			`{"name":"tool_usage_report","arguments":{}}}`), &without)
	if strings.Contains(without.Messages[0].Content.Text, "### Focus:") {
		t.Error("a report with no tool argument still rendered a focus section")
	}

	var with promptGetResult
	resultInto(t, requestOver(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"prompts/get","params":`+
			`{"name":"tool_usage_report","arguments":{"tool":"Bash"}}}`), &with)
	if !strings.Contains(with.Messages[0].Content.Text, "### Focus: Bash") {
		t.Errorf("the named tool produced no focus section; got:\n%s", with.Messages[0].Content.Text)
	}
}

// --- shortenModel -------------------------------------------------------

// The documented discipline: shortening must never drop a trailing
// datestamp, because opus-4-5-20251101 and opus-4-5 are different releases.
// A width-based truncation that ignored that would still pass a test that
// only checked the result was short, so the datestamp is asserted directly.
func TestShortenModel_DropsTheVendorPrefixButKeepsTheDatestamp(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		// 24 chars, over the 20-char budget: the vendor prefix goes and the
		// datestamp -- the part that identifies the release -- stays.
		{"claude-opus-4-5-20251101", "opus-4-5-20251101"},
		// 17 chars, inside the budget, so nothing is touched at all. The
		// prefix is only dropped when shortening is actually needed.
		{"claude-sonnet-4-5", "claude-sonnet-4-5"},
		{"opus-4-5", "opus-4-5"},
		{"", ""},
	} {
		if got := shortenModel(tc.in); got != tc.want {
			t.Errorf("shortenModel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Sessions carrying no model must not render a stray placeholder into the
// prompt text.
func TestShortenModel_EmptyModelStaysEmpty(t *testing.T) {
	if got := shortenModel(""); got != "" {
		t.Errorf("shortenModel(\"\") = %q, want empty", got)
	}
}

// --- tools/call result envelope -----------------------------------------

// A tool-level failure is reported inside the result with isError set, NOT
// as a JSON-RPC error -- that is what lets an agent read the message and
// retry rather than treating the server as broken. Both halves are asserted
// because reporting it as a protocol error is the plausible mistake.
func TestToolsCall_ToolFailureIsAnIsErrorResultNotAProtocolError(t *testing.T) {
	s, _ := newTestServer(t)

	resp := requestOver(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":`+
			`{"name":"search_conversations","arguments":{}}}`)
	if resp.Error != nil {
		t.Fatalf("a missing required argument became a protocol error (%d %q); an agent cannot "+
			"distinguish that from a broken server", resp.Error.Code, resp.Error.Message)
	}

	var got toolResult
	resultInto(t, resp, &got)
	if !got.IsError {
		t.Error("isError is false for a call with no query; the agent would read the message as data")
	}
	if len(got.Content) == 0 || !strings.Contains(got.Content[0].Text, "query is required") {
		t.Errorf("the result does not say what was wrong; got %#v", got.Content)
	}
}

// The success envelope: a tool's payload arrives as JSON text in content[0]
// with isError unset, so an agent can parse it.
func TestToolsCall_SuccessEnvelopeCarriesParseableJSON(t *testing.T) {
	s, database := newTestServer(t)
	seedSession(t, database, "envelope-0001", 1)

	resp := requestOver(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":`+
			`{"name":"list_sessions","arguments":{}}}`)

	var got toolResult
	resultInto(t, resp, &got)
	if got.IsError {
		t.Fatalf("list_sessions reported an error: %#v", got.Content)
	}
	if len(got.Content) != 1 || got.Content[0].Type != "text" {
		t.Fatalf("content = %#v, want exactly one text item", got.Content)
	}
	var payload any
	if err := json.Unmarshal([]byte(got.Content[0].Text), &payload); err != nil {
		t.Fatalf("content text is not JSON an agent could parse: %v (got %q)", err, got.Content[0].Text)
	}
	if !strings.Contains(got.Content[0].Text, "envelope-0001") {
		t.Errorf("the seeded session is absent from the payload; got %s", got.Content[0].Text)
	}
}

// Guards the assumption the two cross-checks above rest on: that
// newTestServer's database really is empty-but-working, so a "not found"
// answer means the lookup ran rather than that the fixture was broken.
func TestNewTestServerDatabaseIsQueryable(t *testing.T) {
	_, database := newTestServer(t)

	sessions, err := database.QuerySessions(db.SessionQuery{Limit: 1})
	if err != nil {
		t.Fatalf("the fixture database is not queryable: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("a fresh fixture already has %d sessions", len(sessions))
	}
}
