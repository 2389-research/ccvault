// ABOUTME: MCP server for ccvault AI integration
// ABOUTME: Implements JSON-RPC 2.0 over stdio for Model Context Protocol

package mcp

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/2389-research/ccvault/internal/analytics"
	"github.com/2389-research/ccvault/internal/buildinfo"
	"github.com/2389-research/ccvault/internal/compact"
	"github.com/2389-research/ccvault/internal/config"
	"github.com/2389-research/ccvault/internal/db"
	"github.com/2389-research/ccvault/internal/export"
	"github.com/2389-research/ccvault/internal/projectref"
	"github.com/2389-research/ccvault/internal/search"
	"github.com/2389-research/ccvault/pkg/models"
)

// Server handles MCP protocol communication
type Server struct {
	db          *db.DB
	cfg         *config.Config
	analyzer    *analytics.Analyzer
	analyzerErr error
	build       buildinfo.Report
	debug       bool
	in          io.Reader // stdin by default; overridable for tests
	out         io.Writer // stdout by default; overridable for tests
}

// NewServer creates a new MCP server. build is what the binary resolved about
// itself; it is passed in rather than resolved here because the ldflags stamps
// a release carries only reach package main.
func NewServer(database *db.DB, cfg *config.Config, build buildinfo.Report) (*Server, error) {
	cacheDir := filepath.Join(cfg.DataDir, "analytics")
	analyzer, analyzerErr := analytics.NewAnalyzer(cacheDir)
	if analyzerErr != nil {
		// Analytics stays optional, but the reason is kept and surfaced by get_analytics
		analyzer = nil
	}

	return &Server{
		db:          database,
		cfg:         cfg,
		analyzer:    analyzer,
		analyzerErr: analyzerErr,
		build:       build,
		debug:       os.Getenv("CCVAULT_MCP_DEBUG") == "1",
		in:          os.Stdin,
		out:         os.Stdout,
	}, nil
}

// Close cleans up server resources
func (s *Server) Close() error {
	if s.analyzer != nil {
		return s.analyzer.Close()
	}
	return nil
}

func (s *Server) log(format string, args ...interface{}) {
	if s.debug {
		fmt.Fprintf(os.Stderr, "[ccvault-mcp] "+format+"\n", args...)
	}
}

// JSON-RPC 2.0 types
type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      interface{} `json:"id"`
	Result  interface{} `json:"result,omitempty"`
	Error   *rpcError   `json:"error,omitempty"`
}

type rpcError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

// internalErrorCode is JSON-RPC 2.0's reserved code for a server-side
// failure the caller cannot correct.
const internalErrorCode = -32603

// invalidParamsCode is JSON-RPC 2.0's reserved code for params the server
// cannot act on. MCP reuses it for an initialize whose protocolVersion
// names no revision the server can speak, distinguished from any other
// bad-params case by the message rather than the code.
const invalidParamsCode = -32602

// MCP types
type serverInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type capabilities struct {
	Tools   *toolsCapability   `json:"tools,omitempty"`
	Prompts *promptsCapability `json:"prompts,omitempty"`
}

type toolsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

type promptsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

type initializeResult struct {
	ProtocolVersion string       `json:"protocolVersion"`
	Capabilities    capabilities `json:"capabilities"`
	ServerInfo      serverInfo   `json:"serverInfo"`
}

// initializeParams is the half of the client's initialize params we act on.
// Capabilities and clientInfo are read past: this server offers the same
// tools to every client and adapts to none of them, so recording what they
// said it could do would be collecting a fact nothing consults.
type initializeParams struct {
	ProtocolVersion string `json:"protocolVersion"`
}

type tool struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema inputSchema `json:"inputSchema"`
}

type inputSchema struct {
	Type       string              `json:"type"`
	Properties map[string]property `json:"properties,omitempty"`
	Required   []string            `json:"required,omitempty"`
}

type property struct {
	Type        string   `json:"type"`
	Description string   `json:"description"`
	Enum        []string `json:"enum,omitempty"`
}

type toolsListResult struct {
	Tools []tool `json:"tools"`
}

type toolCallParams struct {
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments,omitempty"`
}

type toolResult struct {
	Content []contentItem `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

type contentItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Prompt types
type prompt struct {
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Arguments   []promptArgument `json:"arguments,omitempty"`
}

type promptArgument struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

type promptsListResult struct {
	Prompts []prompt `json:"prompts"`
}

type promptGetParams struct {
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments,omitempty"`
}

type promptMessage struct {
	Role    string      `json:"role"`
	Content contentItem `json:"content"`
}

type promptGetResult struct {
	Description string          `json:"description,omitempty"`
	Messages    []promptMessage `json:"messages"`
}

// Run starts the MCP server, reading newline-delimited JSON-RPC from its
// input stream and writing replies to its output stream. It returns when
// the input reaches EOF, which is how a client shuts the server down.
func (s *Server) Run() error {
	s.log("Starting MCP server")
	reader := bufio.NewReader(s.input())

	for {
		line, err := reader.ReadBytes('\n')

		// Dispatch before acting on the error. ReadBytes returns the bytes
		// it managed to read alongside io.EOF, so a client that writes one
		// request and closes the pipe without a trailing newline delivers
		// its frame this way — and returning on the error first read that
		// frame and threw it away, leaving the client waiting on a reply
		// that would never come.
		s.handleLine(line)

		if err != nil {
			if err == io.EOF {
				s.log("EOF received, shutting down")
				return nil
			}
			return fmt.Errorf("read input: %w", err)
		}
	}
}

// input is the stream Run reads requests from.
//
// os.Stdin unless a caller supplied one, which is what makes the whole
// read → dispatch → write path reachable from a test: the write side has
// been injectable as s.out for some time, and the asymmetry (#128) was why
// every protocol-level test had to build and spawn a binary to say anything
// about it. Nil rather than a required constructor argument so the Servers
// assembled field-by-field in tests keep working unchanged.
func (s *Server) input() io.Reader {
	if s.in == nil {
		return os.Stdin
	}
	return s.in
}

// handleLine parses one newline-delimited frame and dispatches it. A frame
// that is not JSON is answered with a parse error and the loop goes on to
// the next one: a client that garbles a frame has to be able to recover by
// sending another.
//
// Whitespace-only input is not a frame and is ignored. A blank line carries
// no message, so there is no request to answer and no id to answer it with;
// a -32700 with a null id would report a failure where nothing failed. It
// is also what the reader hands back at a clean EOF.
func (s *Server) handleLine(line []byte) {
	frame := strings.TrimSpace(string(line))
	if frame == "" {
		return
	}

	s.log("Received: %s", frame)

	var req jsonRPCRequest
	if err := json.Unmarshal(line, &req); err != nil {
		s.log("Parse error: %v", err)
		s.sendError(nil, -32700, "Parse error", err.Error())
		return
	}

	s.handleRequest(&req)
}

func (s *Server) handleRequest(req *jsonRPCRequest) {
	s.log("Handling method: %s", req.Method)

	switch req.Method {
	case "initialize":
		s.handleInitialize(req)
	case "notifications/initialized":
		// Client → server notification per MCP spec. No response.
		s.log("Client initialized")
	case "notifications/cancelled":
		// Client cancelling an in-flight request. We don't have long-running
		// work today, so just log. No response (notification).
		s.log("Client cancelled request: %s", string(req.Params))
	case "notifications/roots/list_changed":
		// Client roots changed. We don't consume roots, so log-only.
		s.log("Client roots changed")
	case "ping":
		s.sendResult(req.ID, map[string]interface{}{})
	case "tools/list":
		s.handleToolsList(req)
	case "tools/call":
		s.handleToolsCall(req)
	case "prompts/list":
		s.handlePromptsList(req)
	case "prompts/get":
		s.handlePromptsGet(req)
	default:
		// JSON-RPC 2.0: notifications (messages without an id) MUST NOT
		// receive a response, even for unknown methods. Only respond when
		// the caller supplied an id, indicating a request. See issue #7.
		s.log("Unknown method: %s", req.Method)
		if req.ID == nil {
			return
		}
		s.sendError(req.ID, -32601, "Method not found", req.Method)
	}
}

// supportedProtocolVersions are the MCP revisions this server speaks,
// newest first, which makes [0] the latest and the whole slice the
// `supported` list a rejected client is handed.
//
// All three are claimed because this server's entire surface is tools and
// prompts over stdio, and those are defined identically across them. What
// each later revision adds -- completions, audio content and tool
// annotations in 2025-03-26; elicitation, structured tool output and
// resource links in 2025-06-18 -- is optional, and none of it appears in
// the capabilities we advertise. The one change that cuts the other way is
// JSON-RPC batching, which 2024-11-05 permits a client to send and
// 2025-06-18 removes: this server reads one JSON object per line and has
// never handled a batch, so 2025-06-18 describes it more accurately than
// 2024-11-05 does.
//
// Adding a revision here is a claim about wire behaviour. Check what it
// changed before extending the list, rather than assuming newest is safe.
var supportedProtocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// negotiateProtocolVersion applies the MCP lifecycle spec's two rules to
// the version a client proposed, and returns ok=false when neither applies.
//
//   - A version we support is answered with itself. The spec makes this a
//     MUST, and it is the half that matters most: a client that strictly
//     checks the negotiated version against what it offered sees agreement
//     rather than an unexplained substitution.
//   - Any other MCP revision is answered with our latest, which the spec
//     makes a MUST (respond with another version you support) and a SHOULD
//     (make it the latest). This covers revisions newer than ours and the
//     dated revisions that predate the oldest we speak; the client decides
//     whether it can live with the answer and disconnects if it cannot.
//   - A proposal that is not a dated MCP revision at all -- "1.0.0", the
//     spec's own example -- names nothing to downgrade to, so there is no
//     version both sides support and the caller sends the spec's error.
//     Answering a date here would assert an agreement that does not exist.
//
// An absent proposal is treated as no proposal rather than as a failure.
// The spec requires clients to send one, so there is nothing to negotiate
// against, and our latest is at least a version we really do support.
func negotiateProtocolVersion(requested string) (agreed string, ok bool) {
	latest := supportedProtocolVersions[0]
	if requested == "" {
		return latest, true
	}
	for _, v := range supportedProtocolVersions {
		if v == requested {
			return requested, true
		}
	}
	if _, err := time.Parse(protocolVersionLayout, requested); err != nil {
		return "", false
	}
	return latest, true
}

// protocolVersionLayout is the YYYY-MM-DD shape MCP revisions are named
// with. Parsing rather than pattern-matching so "2025-13-40" is rejected
// as the non-revision it is.
const protocolVersionLayout = "2006-01-02"

func (s *Server) handleInitialize(req *jsonRPCRequest) {
	var params initializeParams
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			s.log("Initialize params unreadable: %v", err)
			s.sendError(req.ID, invalidParamsCode, "Invalid params", err.Error())
			return
		}
	}

	// Negotiate rather than assert. The handler used to answer the literal
	// "2024-11-05" without ever reading what the client offered (#127), so
	// a client speaking another revision was told it had agreement it had
	// not been given.
	agreed, ok := negotiateProtocolVersion(params.ProtocolVersion)
	if !ok {
		s.log("Unsupported protocol version requested: %s", params.ProtocolVersion)
		s.sendError(req.ID, invalidParamsCode, "Unsupported protocol version", map[string]interface{}{
			"supported": supportedProtocolVersions,
			"requested": params.ProtocolVersion,
		})
		return
	}

	result := initializeResult{
		ProtocolVersion: agreed,
		Capabilities: capabilities{
			Tools:   &toolsCapability{},
			Prompts: &promptsCapability{},
		},
		ServerInfo: serverInfo{
			Name:    "ccvault",
			Version: s.advertisedVersion(),
		},
	}
	s.sendResult(req.ID, result)
}

// advertisedVersion is what initialize reports as serverInfo.version.
//
// A client reads this to decide what the server can do -- which tools exist,
// which response fields to expect, whether to work around a known bug -- so
// it has to be the version this binary actually is. It is whatever
// `ccvault version` reports, which for a build carrying no release stamps is
// "dev" rather than a semver-shaped guess. MCP's Implementation.version is a
// free-form string with no format requirement, so an honest "dev" is a better
// answer to "which build is this" than an invented number.
//
// The Unknown fallback covers a Server assembled without build facts: the
// field is required by the schema, and an empty string would read to a client
// as a version it failed to parse rather than one the server does not have.
func (s *Server) advertisedVersion() string {
	if s.build.Version == "" {
		return buildinfo.Unknown
	}
	return s.build.Version
}

func (s *Server) handleToolsList(req *jsonRPCRequest) {
	tools := []tool{
		{
			Name:        "search_conversations",
			Description: "Search through Claude Code conversation history. Returns compact results with snippets. Use offset for pagination.",
			InputSchema: inputSchema{
				Type: "object",
				Properties: map[string]property{
					"query": {
						Type: "string",
						Description: "Search query. Supports: project:name, model:opus, tool:Bash, " +
							"before:2024-01-01, after:2024-01-01, \"exact phrase\", has:subagent, " +
							"has:error (sessions that held a tool failure), has:toolerror (the turns " +
							"whose own tool call failed — the per-call granularity, and what to use " +
							"to find what broke)",
					},
					"limit": {
						Type:        "number",
						Description: "Results per page (default: 10, max: 50)",
					},
					"offset": {
						Type:        "number",
						Description: "Skip first N results for pagination. Use next_offset from response.",
					},
					"source": {
						Type:        "string",
						Description: "Filter by source (e.g. \"claude-code\", \"codex\", \"jeff\", \"hex\")",
					},
				},
				Required: []string{"query"},
			},
		},
		{
			Name: "get_session_summary",
			Description: "Get a quick overview of a session: metadata, stats, tools used, and first/last " +
				"messages. Use this first before get_turns. tools_used is counted from the tool calls " +
				"stored for the session, which every source records — so it is right for codex, hex and " +
				"jeff, whose calls never appear in a turn's raw_json. An assistant turn that recorded no " +
				"calls and whose raw_json cannot be read may still have called something nothing can " +
				"name; the response counts those in unreadable_turns with a 'tools_used incomplete' " +
				"warnings[] entry. " +
				"failed_tool_calls and failed_tools report the calls whose result said they failed, " +
				"read from a stored column rather than raw_json, so they stay correct on a session " +
				"whose tools_used is incomplete. Both are absent when nothing failed.",
			InputSchema: inputSchema{
				Type: "object",
				Properties: map[string]property{
					"session_id": {
						Type:        "string",
						Description: "Session UUID",
					},
				},
				Required: []string{"session_id"},
			},
		},
		{
			Name: "get_turns",
			Description: "Get paginated turns from a session, in conversation order. Every turn reports its " +
				"ordinal — its position in the session, counting from 0, stable across calls. To walk a " +
				"session, pass the response's next_after_ordinal back as after_ordinal; offset also works " +
				"but counts rows in the last response rather than naming a turn. An assistant turn's " +
				"tools come from the session's stored tool calls, so they are reported for every source " +
				"and survive a turn whose raw_json cannot be read. has_thinking is read from raw_json and " +
				"is present only when it could be determined: a turn whose raw_json could not be read " +
				"carries raw_unavailable: true and omits it, and so does a source that records no " +
				"thinking at all — absence is not a false. The page reports how many turns were " +
				"unreadable in unreadable_turns plus one warnings[] entry. A turn that issued a " +
				"call whose result said it failed carries failed_tools with that call's tool name; it " +
				"comes from a stored column, so it is present even on a raw_unavailable turn, and it is " +
				"absent rather than empty when nothing failed.",
			InputSchema: inputSchema{
				Type: "object",
				Properties: map[string]property{
					"session_id": {
						Type:        "string",
						Description: "Session UUID",
					},
					"after_ordinal": {
						Type: "number",
						Description: "Resume after this position in the session. Prefer this over offset: " +
							"it names a turn, so it is unaffected by a type filter and still means the " +
							"same thing on a later call. Pass the previous response's next_after_ordinal.",
					},
					"offset": {
						Type:        "number",
						Description: "Skip first N turns (default: 0). Ignored when after_ordinal is given.",
					},
					"limit": {
						Type:        "number",
						Description: "Turns per page (default: 20, max: 50)",
					},
					"type": {
						Type:        "string",
						Description: "Filter by turn type",
						Enum:        []string{"user", "assistant", "tool_result"},
					},
				},
				Required: []string{"session_id"},
			},
		},
		{
			Name:        "get_session",
			Description: "Get full session in markdown format. Sessions over 100 turns come back with no markdown and a 'markdown unavailable' entry in warnings[]; markdown is truncated at 50K chars. Use get_session_summary + get_turns for big sessions.",
			InputSchema: inputSchema{
				Type: "object",
				Properties: map[string]property{
					"session_id": {
						Type:        "string",
						Description: "Session UUID",
					},
				},
				Required: []string{"session_id"},
			},
		},
		{
			Name:        "list_sessions",
			Description: "List recent top-level sessions, optionally filtered by project. Use offset for pagination. Subagent sessions are not listed by default; every row reports subagent_count, and get_session / get_turns work on a subagent id directly.",
			InputSchema: inputSchema{
				Type: "object",
				Properties: map[string]property{
					"project": {
						Type:        "string",
						Description: "Filter by project path or name (partial match)",
					},
					"limit": {
						Type:        "number",
						Description: "Sessions per page (default 20, max 100)",
					},
					"offset": {
						Type:        "number",
						Description: "Skip first N sessions for pagination. Use next_offset from response.",
					},
					"include_subagents": {
						Type:        "boolean",
						Description: "List subagent sessions alongside top-level ones instead of hiding them.",
					},
					"subagents_of": {
						Type:        "string",
						Description: "List only the subagent sessions dispatched by this session id.",
					},
				},
			},
		},
		{
			Name:        "list_projects",
			Description: "List all indexed projects with session counts and token usage. Use offset for pagination.",
			InputSchema: inputSchema{
				Type: "object",
				Properties: map[string]property{
					"sort": {
						Type:        "string",
						Description: "Sort order",
						Enum:        []string{"name", "activity", "tokens", "sessions"},
					},
					"limit": {
						Type:        "number",
						Description: "Projects per page (default 50, max 100)",
					},
					"offset": {
						Type:        "number",
						Description: "Skip first N projects for pagination. Use next_offset from response.",
					},
				},
			},
		},
		{
			Name:        "get_stats",
			Description: "Get archive statistics: projects, sessions, tokens, models used, date range.",
			InputSchema: inputSchema{
				Type:       "object",
				Properties: map[string]property{},
			},
		},
		{
			Name:        "get_analytics",
			Description: "Get detailed analytics: token usage by day, model breakdown, top projects.",
			InputSchema: inputSchema{
				Type: "object",
				Properties: map[string]property{
					"days": {
						Type:        "number",
						Description: "Days of history (default: 30)",
					},
				},
			},
		},
	}

	s.sendResult(req.ID, toolsListResult{Tools: tools})
}

func (s *Server) handlePromptsList(req *jsonRPCRequest) {
	prompts := []prompt{
		{
			Name:        "summarize_recent",
			Description: "Summarize recent Claude Code activity across all projects",
			Arguments: []promptArgument{
				{Name: "days", Description: "Number of days to summarize (default: 7)", Required: false},
			},
		},
		{
			Name:        "analyze_project",
			Description: "Analyze Claude Code usage patterns for a specific project",
			Arguments: []promptArgument{
				{Name: "project", Description: "Project path or name to analyze", Required: true},
			},
		},
		{
			Name:        "find_solutions",
			Description: "Find past solutions and approaches for a given problem or topic",
			Arguments: []promptArgument{
				{Name: "topic", Description: "Problem or topic to search for", Required: true},
			},
		},
		{
			Name:        "review_session",
			Description: "Review and summarize a specific conversation session",
			Arguments: []promptArgument{
				{Name: "session_id", Description: "Session UUID to review", Required: true},
			},
		},
		{
			Name:        "compare_approaches",
			Description: "Find different approaches used for similar problems across sessions",
			Arguments: []promptArgument{
				{Name: "topic", Description: "Topic or problem type to compare", Required: true},
			},
		},
		{
			Name:        "tool_usage_report",
			Description: "Generate a report on which tools are used most and how",
			Arguments: []promptArgument{
				{Name: "tool", Description: "Specific tool to analyze (optional)", Required: false},
			},
		},
	}

	s.sendResult(req.ID, promptsListResult{Prompts: prompts})
}

func (s *Server) handlePromptsGet(req *jsonRPCRequest) {
	var params promptGetParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		s.sendError(req.ID, -32602, "Invalid params", err.Error())
		return
	}

	var result promptGetResult
	var err error

	switch params.Name {
	case "summarize_recent":
		result, err = s.promptSummarizeRecent(params.Arguments)
	case "analyze_project":
		result, err = s.promptAnalyzeProject(params.Arguments)
	case "find_solutions":
		result, err = s.promptFindSolutions(params.Arguments)
	case "review_session":
		result, err = s.promptReviewSession(params.Arguments)
	case "compare_approaches":
		result, err = s.promptCompareApproaches(params.Arguments)
	case "tool_usage_report":
		result, err = s.promptToolUsageReport(params.Arguments)
	default:
		s.sendError(req.ID, -32602, "Unknown prompt", params.Name)
		return
	}

	if err != nil {
		s.sendError(req.ID, -32603, "Prompt error", err.Error())
		return
	}

	s.sendResult(req.ID, result)
}

func (s *Server) handleToolsCall(req *jsonRPCRequest) {
	var params toolCallParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		s.sendError(req.ID, -32602, "Invalid params", err.Error())
		return
	}

	s.log("Tool call: %s with args: %v", params.Name, params.Arguments)

	var result interface{}
	var err error

	switch params.Name {
	case "search_conversations":
		result, err = s.searchConversations(params.Arguments)
	case "get_session_summary":
		result, err = s.getSessionSummary(params.Arguments)
	case "get_turns":
		result, err = s.getTurns(params.Arguments)
	case "get_session":
		result, err = s.getSession(params.Arguments)
	case "list_sessions":
		result, err = s.listSessions(params.Arguments)
	case "list_projects":
		result, err = s.listProjects(params.Arguments)
	case "get_stats":
		result, err = s.getStats(params.Arguments)
	case "get_analytics":
		result, err = s.getAnalytics(params.Arguments)
	default:
		s.sendError(req.ID, -32602, "Unknown tool", params.Name)
		return
	}

	if err != nil {
		s.log("Tool error: %v", err)
		s.sendResult(req.ID, toolResult{
			Content: []contentItem{{Type: "text", Text: fmt.Sprintf("Error: %v", err)}},
			IsError: true,
		})
		return
	}

	// Marshal result to JSON text
	jsonBytes, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		s.sendResult(req.ID, toolResult{
			Content: []contentItem{{Type: "text", Text: fmt.Sprintf("Error marshaling result: %v", err)}},
			IsError: true,
		})
		return
	}

	s.sendResult(req.ID, toolResult{
		Content: []contentItem{{Type: "text", Text: string(jsonBytes)}},
	})
}

// Tool implementations

func (s *Server) searchConversations(args map[string]interface{}) (interface{}, error) {
	query, _ := args["query"].(string)
	if query == "" {
		return nil, fmt.Errorf("query is required")
	}

	limit := 10
	if l, ok := args["limit"].(float64); ok {
		limit = int(l)
		if limit > 50 {
			limit = 50
		}
	}

	offset := 0
	if o, ok := args["offset"].(float64); ok {
		offset = int(o)
	}

	// Fetch one extra to determine if there are more results
	parsed := search.Parse(query)

	// Apply source filter from explicit parameter (overrides any source: in query text)
	if source, ok := args["source"].(string); ok && source != "" {
		parsed.Source = source
	}

	searcher := search.New(s.db.DB)
	results, err := searcher.Search(parsed, limit+offset+1)
	if err != nil {
		return nil, fmt.Errorf("search failed: %w", err)
	}

	// Apply offset
	if offset > len(results) {
		results = nil
	} else if offset > 0 {
		results = results[offset:]
	}

	// Check if there are more results
	hasMore := len(results) > limit
	if hasMore {
		results = results[:limit]
	}

	// Degraded enrichment queries report here, in the one top-level
	// `warnings` array every MCP response uses, and omit their field.
	var warnings []string

	// Class C — enrich each result with a project_name so agents get
	// {name, path} doctrine shape uniformly across MCP surfaces. A failed
	// lookup leaves every project_name on the basename fallback, so say so
	// rather than passing the fallback off as the adapter-provided label.
	allProjects, enrichErr := s.db.GetProjects("activity", 0, 0)
	if enrichErr != nil {
		warnings = append(warnings, fmt.Sprintf("project enrichment unavailable: %v (result project_name values fell back to basename)", enrichErr))
	}
	byPath := projectref.ProjectsByPath(allProjects)

	compactResults := make([]map[string]interface{}, 0, len(results))
	for _, r := range results {
		// Truncate snippet to 200 chars
		snippet := r.Snippet
		if len(snippet) > 200 {
			snippet = snippet[:200] + "..."
		}

		result := map[string]interface{}{
			"session_id":   r.SessionID,
			"turn_id":      r.Turn.ID,
			"turn_type":    r.Turn.Type,
			"timestamp":    r.Turn.Timestamp.Format(time.RFC3339),
			"project_path": r.ProjectPath,
			"project_name": projectref.LabelFromPath(r.ProjectPath, byPath),
			"model":        r.Model,
			"source":       r.Source,
			"snippet":      snippet,
			// Search is never filtered by the subagent listing default, so a
			// hit can sit in a transcript list_sessions doesn't show. The
			// parent id is how an agent walks back to the conversation.
			"parent_session_id": nil,
			// Set when the query matched a stored tool input or result rather
			// than the turn's own text. Without it an agent cannot tell a
			// conversational hit from a tool-payload hit, and the snippet for
			// the latter is command output rather than anything anyone said.
			"matched_tool_name": nil,
		}
		if r.ParentSessionID != "" {
			result["parent_session_id"] = r.ParentSessionID
		}
		if r.MatchedToolName != "" {
			result["matched_tool_name"] = r.MatchedToolName
		}
		compactResults = append(compactResults, result)
	}

	response := map[string]interface{}{
		"count":   len(compactResults),
		"offset":  offset,
		"limit":   limit,
		"results": compactResults,
	}

	if hasMore {
		response["next_offset"] = offset + limit
		response["has_more"] = true
	}

	if len(compactResults) == 0 {
		hint := "No results. Broaden the search: drop one filter or try different terms; use list_projects to verify project names."
		if parsed.Tool != "" {
			names, warning := s.similarToolNames(parsed.Tool, 5)
			switch {
			case warning != "":
				warnings = append(warnings, warning)
			case len(names) > 0:
				response["similar_tool_names"] = names
				hint = fmt.Sprintf("No results for tool:%s — tool matching requires the full tool name. See similar_tool_names for close matches.", parsed.Tool)
			}
		}
		response["hint"] = hint
	}

	if len(warnings) > 0 {
		response["warnings"] = warnings
	}

	return response, nil
}

// similarToolNames finds tool names close to a tool: fragment that matched
// nothing. A failed lookup comes back as a single warning string — empty
// when the lookup succeeded — so the caller reports the gap instead of
// returning a response that reads as "no tool name resembles this".
func (s *Server) similarToolNames(fragment string, limit int) (names []string, warning string) {
	names, err := s.db.GetToolNamesLike(fragment, limit)
	if err != nil {
		return nil, fmt.Sprintf("similar_tool_names unavailable: %v", err)
	}
	return names, ""
}

// lookupProjectPath resolves a session's project path. A failure comes
// back as a single warning string — empty when the lookup succeeded — so
// callers surface it instead of dropping it.
func (s *Server) lookupProjectPath(projectID int64) (path, warning string) {
	path, _, warning = s.lookupProjectPathAndName(projectID)
	return path, warning
}

// lookupProjectPathAndName resolves a project ID to its path AND its
// adapter-provided label (via projectref.Label). Class C emitters use
// this so responses carry the {name, path} doctrine shape even when the
// caller only has a project ID (not a full session with ProjectPath).
func (s *Server) lookupProjectPathAndName(projectID int64) (path, name, warning string) {
	if projectID <= 0 {
		return "", "", ""
	}

	project, err := s.db.GetProject(projectID)
	switch {
	case err != nil:
		return "", "", fmt.Sprintf("project %d unavailable: %v", projectID, err)
	case project == nil:
		return "", "", fmt.Sprintf("project %d unavailable: not found, the session references a missing project", projectID)
	default:
		return project.Path, projectref.Label(project), ""
	}
}

// turnEnrichment is what an assistant turn's raw_json could be made to
// say. The two flags are separate because they fail separately, and a
// caller that collapsed them would reintroduce the defect they exist to
// prevent.
type turnEnrichment struct {
	// tools names the calls recorded in message.content[], in order.
	// Only ever a partial picture — see readTurnEnrichment.
	tools []string

	// rawReadable is false when raw_json could not be read at all:
	// absent, because db.GetTurns dropped bytes that were not valid JSON,
	// or valid JSON in a shape where message.content is not a list of
	// blocks. A turn like this is marked raw_unavailable.
	rawReadable bool

	// shapeCarriesThinking is true only when the raw entry holds the
	// top-level `message` object thinking blocks live in. claude-code and
	// nanoclaw write it; codex, hex and jeff write raw entries with no
	// such key, and Go's json.Unmarshal ignores unknown keys, so the
	// parse succeeds against them and finds nothing. "Found nothing" in
	// a shape that could not have held it is not a reading (#100), so
	// callers omit has_thinking rather than answering false.
	shapeCarriesThinking bool

	// hasThinking is meaningful only when shapeCarriesThinking.
	hasThinking bool
}

// readTurnEnrichment reads what it can out of an assistant turn's raw_json.
//
// Tools recovered here are a fallback, not the source of truth: calls are
// stored in tool_uses by every adapter, and toolNamesByTurn reads them from
// there. The fallback exists for a turn that holds tool_use blocks and has
// no stored rows — measured as zero turns across the author's 47k-session
// archive, so it is insurance against an archive written by an older
// ccvault rather than a path anything takes today.
//
// The parse stays tolerant of unknown keys because raw_json's shape
// differs across the source adapters.
func readTurnEnrichment(rawJSON []byte) turnEnrichment {
	if len(rawJSON) == 0 {
		return turnEnrichment{}
	}

	// A pointer, so an entry with no `message` key at all is distinguishable
	// from one whose message held nothing. That distinction is the whole
	// fix for has_thinking in #100.
	var raw struct {
		Message *struct {
			Content []struct {
				Type     string `json:"type"`
				Name     string `json:"name,omitempty"`
				Thinking string `json:"thinking,omitempty"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(rawJSON, &raw); err != nil {
		return turnEnrichment{}
	}

	e := turnEnrichment{rawReadable: true}
	if raw.Message == nil {
		return e
	}

	e.shapeCarriesThinking = true
	for _, c := range raw.Message.Content {
		switch c.Type {
		case "tool_use":
			if c.Name != "" {
				e.tools = append(e.tools, c.Name)
			}
		case "thinking":
			e.hasThinking = true
		}
	}
	return e
}

// toolNamesByTurn groups a session's recorded tool calls by the turn that
// issued them, in the order they were issued.
//
// This is where the MCP layer learns what a turn called. Reading raw_json
// for it only ever worked for claude-code and nanoclaw, whose entries carry
// a top-level `message` object; codex and jeff record a turn's calls on
// their own transcript lines, so a codex turn's calls are not in that
// turn's raw_json at all and no amount of re-parsing recovers them (#100).
// The adapters already extract every source's calls into tool_uses — the
// column existed, this layer just was not reading it.
//
// Measured over the author's 47k-session archive: of the 287,557 assistant
// turns whose raw_json the old parse could read, not one names a tool the
// stored rows do not, and 501 of them name fewer. The column dominates
// wherever both have an answer.
//
// Duplicates are kept, as failedToolsByTurn keeps them: a turn that made
// three Bash calls says Bash three times, because the count is the fact.
func toolNamesByTurn(database *db.DB, sessionID string) (map[string][]string, error) {
	rows, err := database.Query(`
		SELECT turn_id, tool_name
		FROM tool_uses
		WHERE session_id = ? AND turn_id IS NOT NULL
		ORDER BY turn_id, turn_ordinal`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("query tool calls for session %s: %w", sessionID, err)
	}
	return scanToolNamesByTurn(rows)
}

// toolNamesForTurns is toolNamesByTurn scoped to named turns, for a caller
// rendering a page rather than summarising a session (#141).
//
// Naming the turns changes which index answers the query, and that is where
// the cost goes. Session-wide, SQLite seeks idx_tool_uses_session and then
// sorts every row it found in a temp B-tree, because that index does not
// carry the ORDER BY. Per turn it seeks idx_tool_uses_turn_ordinal — which
// leads on turn_id and already holds the rows in the order asked for — once
// per turn, and sorts nothing.
//
// Measured by BenchmarkToolNameReadForOnePage on a session of 7,034 turns,
// for one 50-turn page, session-wide -> page-bounded:
//
//	1,871 calls     1,871 rows, 1.3ms  ->   50 rows, 0.07ms
//	133,039 calls  133,039 rows, 205ms ->  950 rows, 0.81ms
//
// The first is the largest real session in the author's archive; the second
// is the scale #141 quoted. Either way the point is the shape: a per-page
// cost proportional to the session is what undoes paging a long one (#79).
func toolNamesForTurns(database *db.DB, sessionID string, turnIDs []string) (map[string][]string, error) {
	if len(turnIDs) == 0 {
		// `turn_id IN ()` is not valid SQL, and there is nothing to ask about.
		return map[string][]string{}, nil
	}

	byTurn := make(map[string][]string, len(turnIDs))

	// Chunked so a caller asking about more turns than get_turns' 50-row page
	// can't exceed SQLite's bound-parameter ceiling.
	const chunk = 500
	for start := 0; start < len(turnIDs); start += chunk {
		batch := turnIDs[start:min(start+chunk, len(turnIDs))]

		placeholders := make([]string, len(batch))
		args := make([]interface{}, 0, len(batch)+1)
		args = append(args, sessionID)
		for i, id := range batch {
			placeholders[i] = "?"
			args = append(args, id)
		}

		rows, err := database.Query(`
			SELECT turn_id, tool_name
			FROM tool_uses
			WHERE session_id = ? AND turn_id IN (`+strings.Join(placeholders, ", ")+`)
			ORDER BY turn_id, turn_ordinal`, args...)
		if err != nil {
			return nil, fmt.Errorf("query tool calls for %d turns of session %s: %w", len(turnIDs), sessionID, err)
		}
		part, err := scanToolNamesByTurn(rows)
		if err != nil {
			return nil, err
		}
		for turnID, names := range part {
			byTurn[turnID] = append(byTurn[turnID], names...)
		}
	}

	return byTurn, nil
}

// scanToolNamesByTurn groups the rows of a (turn_id, tool_name) query, which
// both readers above produce in the order the calls were issued.
func scanToolNamesByTurn(rows *sql.Rows) (map[string][]string, error) {
	defer func() { _ = rows.Close() }()

	byTurn := make(map[string][]string)
	for rows.Next() {
		var turnID, toolName string
		if err := rows.Scan(&turnID, &toolName); err != nil {
			return nil, fmt.Errorf("scan tool call: %w", err)
		}
		if toolName == "" {
			continue
		}
		byTurn[turnID] = append(byTurn[turnID], toolName)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read tool calls: %w", err)
	}

	return byTurn, nil
}

// turnTools is what a turn called: the stored rows, falling back to
// whatever its raw_json names when it has none. The fallback can never
// double-count, because it only runs for a turn with no stored calls.
func turnTools(stored []string, e turnEnrichment) []string {
	if len(stored) > 0 {
		return stored
	}
	return e.tools
}

// topCountsByName renders a name-to-count map as the {tool, count} list the
// MCP responses use, highest first and capped at ten.
//
// Extracted so tools_used and failed_tools come out in the same shape, which
// is the point of putting them side by side: a caller comparing "what this
// session reached for" against "what came back broken" should not have to read
// two formats to do it. Ties break on the name so one session's summary does
// not reorder between calls.
func topCountsByName(counts map[string]int) []map[string]interface{} {
	names := make([]string, 0, len(counts))
	for name := range counts {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if counts[names[i]] != counts[names[j]] {
			return counts[names[i]] > counts[names[j]]
		}
		return names[i] < names[j]
	})
	if len(names) > 10 {
		names = names[:10]
	}

	out := make([]map[string]interface{}, len(names))
	for i, name := range names {
		out[i] = map[string]interface{}{"tool": name, "count": counts[name]}
	}
	return out
}

// failedToolsByTurn groups a session's failed calls by the turn that issued
// them, so a response can label each turn with what broke in it.
//
// Reads the stored is_error column through db.GetFailedToolCalls rather than
// re-deriving the flag from raw_json, and that is the whole point of the
// column. readTurnEnrichment above fails on 216,978 of the author's turns
// (#101) and reports nothing on any source whose raw shape has no top-level
// message key (#100); a failure flag recovered by the backfill or written by
// sync must not be lost to either.
//
// Names rather than ids: a caller reading "failed_tools: [Edit]" learns what
// it needs without a second lookup, and the same turn's `tools` field is names
// too. Duplicates are kept — a turn that made three failing Bash calls says
// Bash three times, because the count is the fact.
func failedToolsByTurn(failures []db.FailedToolCall) map[string][]string {
	if len(failures) == 0 {
		return nil
	}
	byTurn := make(map[string][]string, len(failures))
	for _, f := range failures {
		byTurn[f.TurnID] = append(byTurn[f.TurnID], f.ToolName)
	}
	return byTurn
}

func (s *Server) getSessionSummary(args map[string]interface{}) (interface{}, error) {
	sessionID, _ := args["session_id"].(string)
	if sessionID == "" {
		return nil, fmt.Errorf("session_id is required")
	}

	session, err := s.db.GetSession(sessionID)
	if err != nil {
		return nil, fmt.Errorf("database error: %w", err)
	}
	if session == nil {
		return nil, fmt.Errorf("session not found: %s", sessionID)
	}

	turns, err := s.db.GetTurns(sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to get turns: %w", err)
	}

	// Get project info — pull both path AND name so the response carries
	// the Class C {name, path} doctrine shape.
	projectPath, projectName, projectWarning := s.lookupProjectPathAndName(session.ProjectID)

	// What each turn called, read once for the session from the stored
	// rows. Every adapter writes them, which is what makes tools_used
	// correct for codex, hex and jeff rather than silently empty (#100).
	storedTools, err := toolNamesByTurn(s.db, sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to get tool calls: %w", err)
	}

	// Count turn types and extract tool usage
	turnTypeCounts := make(map[string]int)
	toolCounts := make(map[string]int)
	var firstUserMsg, lastUserMsg string

	// An assistant turn with no stored calls and raw_json we cannot read
	// is the one case tools_used is silently short by: it may have called
	// something, and nothing here can say what. Count those turns so the
	// response can say tools_used is incomplete — an empty tools_used
	// otherwise reads as a session that called nothing.
	unreadableTurns := 0

	for _, t := range turns {
		turnTypeCounts[t.Type]++

		if t.Type == "assistant" {
			enrichment := readTurnEnrichment(t.RawJSON)
			tools := turnTools(storedTools[t.ID], enrichment)
			if len(tools) == 0 && !enrichment.rawReadable {
				unreadableTurns++
			}
			for _, name := range tools {
				toolCounts[name]++
			}
		}

		// Capture first and last user messages
		if t.Type == "user" {
			content := t.Content
			if content == "" && len(t.RawJSON) > 0 {
				// Try to extract from raw JSON
				var raw struct {
					Message struct {
						Content interface{} `json:"content"`
					} `json:"message"`
				}
				if json.Unmarshal(t.RawJSON, &raw) == nil {
					if str, ok := raw.Message.Content.(string); ok {
						content = str
					}
				}
			}
			if content != "" {
				if firstUserMsg == "" {
					firstUserMsg = content
				}
				lastUserMsg = content
			}
		}
	}

	// Truncate messages
	if len(firstUserMsg) > 500 {
		firstUserMsg = firstUserMsg[:500] + "..."
	}
	if len(lastUserMsg) > 500 {
		lastUserMsg = lastUserMsg[:500] + "..."
	}

	topToolsMap := topCountsByName(toolCounts)

	result := map[string]interface{}{
		"session_id":     session.ID,
		"project_path":   projectPath,
		"project_name":   projectName,
		"source":         session.Source,
		"model":          session.Model,
		"started_at":     session.StartedAt.Format(time.RFC3339),
		"ended_at":       session.EndedAt.Format(time.RFC3339),
		"git_branch":     session.GitBranch,
		"turn_count":     len(turns),
		"turn_types":     turnTypeCounts,
		"input_tokens":   session.InputTokens,
		"output_tokens":  session.OutputTokens,
		"total_tokens":   session.TotalTokens(),
		"tools_used":     topToolsMap,
		"first_user_msg": firstUserMsg,
		"last_user_msg":  lastUserMsg,
		"hint": "Use get_turns to paginate through the conversation, resuming with " +
			"after_ordinal",
	}

	// What broke, counted per tool. tools_used says what the session reached
	// for; this says which of those reaches came back a failure, which is the
	// half of "what happened here" the summary could not answer before (#83).
	//
	// Read from the stored column, so it is unaffected by the raw_json that
	// makes tools_used incomplete on a corrupt session — the two can therefore
	// disagree, with failed_tool_calls naming a tool tools_used undercounts.
	// That is the honest outcome and the warning below already says why.
	//
	// Both fields are omitted entirely when nothing failed, rather than
	// emitted as a zero and an empty list. A field that is present and empty on
	// every clean session is a field callers learn to skip.
	failures, err := s.db.GetFailedToolCalls(sessionID)
	if err != nil {
		return nil, fmt.Errorf("read failed tool calls: %w", err)
	}
	if len(failures) > 0 {
		failedCounts := make(map[string]int, len(failures))
		for _, f := range failures {
			failedCounts[f.ToolName]++
		}
		result["failed_tool_calls"] = len(failures)
		result["failed_tools"] = topCountsByName(failedCounts)
	}

	// Where the session's sequence ends, so a caller can resume from the tail
	// without fetching the conversation to find it. last_entry_uuid names the
	// turn at that position: a caller holding both can tell "the session grew"
	// from "the transcript was rewritten under me", which a turn count cannot.
	cursor, err := s.db.SessionTurnCursor(sessionID)
	if err != nil {
		return nil, fmt.Errorf("read turn cursor: %w", err)
	}
	if cursor.Found {
		result["last_ordinal"] = cursor.LastOrdinal
		result["last_entry_uuid"] = cursor.LastEntryUUID
	}

	// Both notices share the one top-level warnings array every other
	// degraded response uses, so a caller has one field to check.
	var warnings []string
	if projectWarning != "" {
		warnings = append(warnings, projectWarning)
	}
	// This warning names a field that is present but incomplete, not
	// absent: dropping tools_used outright would cost a caller every tool
	// the other turns did report, which on a half-corrupt session is most
	// of them.
	if unreadableTurns > 0 {
		result["unreadable_turns"] = unreadableTurns
		warnings = append(warnings, fmt.Sprintf(
			"tools_used incomplete: %d of %d assistant turns recorded no tool calls and have "+
				"unreadable raw_json, so anything they called is not counted",
			unreadableTurns, turnTypeCounts["assistant"]))
	}
	if len(warnings) > 0 {
		result["warnings"] = warnings
	}
	return result, nil
}

func (s *Server) getTurns(args map[string]interface{}) (interface{}, error) {
	sessionID, _ := args["session_id"].(string)
	if sessionID == "" {
		return nil, fmt.Errorf("session_id is required")
	}

	// limit and offset arrive from an MCP caller, so both bounds are
	// clamped. Without the lower one a negative value reaches the slice
	// expressions below and panics the server, and limit 0 reports has_more
	// with no turns, which walks a caller into an endless loop.
	limit := 20
	if l, ok := args["limit"].(float64); ok && l > 0 {
		limit = int(l)
		if limit > 50 {
			limit = 50
		}
	}

	offset := 0
	if o, ok := args["offset"].(float64); ok && o > 0 {
		offset = int(o)
	}

	// after_ordinal resumes from a turn's position in the session rather than
	// from a count of rows. That distinction is the point of the ordinal:
	// offset indexes whatever this call returned, so it shifts the moment a
	// type filter is applied and means something different on the next call,
	// while a position names one turn in the session forever.
	afterOrdinal, resumeFromOrdinal := -1, false
	if a, ok := args["after_ordinal"].(float64); ok {
		afterOrdinal, resumeFromOrdinal = int(a), true
	}

	typeFilter, _ := args["type"].(string)

	turns, err := s.db.GetTurns(sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to get turns: %w", err)
	}

	// Filter by type if specified
	if typeFilter != "" {
		var filtered []models.Turn
		for _, t := range turns {
			if t.Type == typeFilter {
				filtered = append(filtered, t)
			}
		}
		turns = filtered
	}

	totalCount := len(turns)

	// after_ordinal and offset are two ways to say the same thing, so taking
	// both would mean applying one on top of the other. A position wins:
	// nothing else in the response is as specific.
	if resumeFromOrdinal {
		offset = 0
		kept := turns[:0]
		for _, t := range turns {
			if t.Ordinal > afterOrdinal {
				kept = append(kept, t)
			}
		}
		turns = kept
	} else if offset >= len(turns) {
		turns = nil
	} else {
		turns = turns[offset:]
	}

	// Check for more
	hasMore := len(turns) > limit
	if hasMore {
		turns = turns[:limit]
	}

	// Which calls failed, read once for the session rather than per turn.
	// One indexed query against idx_tool_uses_is_error costs less than fifty
	// lookups, and a page shows at most fifty turns.
	failures, err := s.db.GetFailedToolCalls(sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to get failed tool calls: %w", err)
	}
	failedTools := failedToolsByTurn(failures)

	// What each turn called, read for the turns this page is about to render
	// and no others. Only assistant turns are labelled, so only those are
	// asked about — and a page of nothing but user turns asks nothing.
	//
	// Scoped rather than session-wide because the session is the wrong unit
	// for a page: the read used to cost what the whole session's tool calls
	// cost, however few rows it was labelling (#141).
	pageTurnIDs := make([]string, 0, len(turns))
	for _, t := range turns {
		if t.Type == "assistant" {
			pageTurnIDs = append(pageTurnIDs, t.ID)
		}
	}
	storedTools, err := toolNamesForTurns(s.db, sessionID, pageTurnIDs)
	if err != nil {
		return nil, fmt.Errorf("failed to get tool calls: %w", err)
	}

	// Transform to compact representation
	compactTurns := make([]map[string]interface{}, 0, len(turns))
	unreadableTurns := 0
	for _, t := range turns {
		content := t.Content
		// Truncate long content
		if len(content) > 1000 {
			content = content[:1000] + "... [truncated, " + fmt.Sprintf("%d", len(t.Content)) + " chars total]"
		}

		turn := map[string]interface{}{
			"id":   t.ID,
			"type": t.Type,
			// The turn's position in the session. Reported because it is what
			// a caller resumes from: offset counts rows in whatever this call
			// returned (and shifts when a type filter is applied), while the
			// ordinal names the turn itself.
			"ordinal":   t.Ordinal,
			"timestamp": t.Timestamp.Format(time.RFC3339),
			"content":   content,
		}

		// tools comes from the stored tool_uses rows, which every adapter
		// writes — so it is right for codex, hex and jeff, whose calls are
		// recorded on transcript lines of their own and were never in the
		// turn's raw_json for the old reader to find (#100). It also
		// survives a turn whose raw_json cannot be read, which is where it
		// is worth the most.
		//
		// has_thinking can only come from raw_json, and only from the
		// `message` object claude-code and nanoclaw write. It is omitted
		// when raw_json cannot be read, and omitted again when the raw
		// shape has no such object: answering false there would be a claim
		// about a source that records no thinking at all, not a reading.
		//
		// raw_unavailable marks the first of those, and is still worth
		// saying. Measured over the 46k-session archive (493,908 assistant
		// turns): 52,851 reach here with no raw_json at all, because
		// db.GetTurns drops bytes that are not valid JSON — the
		// mid-document fragments left by claude-code syncs predating the
		// oversized-line fix.
		if t.Type == "assistant" {
			enrichment := readTurnEnrichment(t.RawJSON)

			if tools := turnTools(storedTools[t.ID], enrichment); len(tools) > 0 {
				turn["tools"] = tools
			}
			if enrichment.shapeCarriesThinking {
				turn["has_thinking"] = enrichment.hasThinking
			}
			if !enrichment.rawReadable {
				turn["raw_unavailable"] = true
				unreadableTurns++
			}
		}

		// failed_tools names the calls of this turn whose result reported a
		// failure (#83). Set only when there are some: an empty array on
		// every turn would make "this turn broke nothing" and "nothing is
		// known about this turn" the same shape, and the second is a real
		// state — a call nothing answered stores NULL, not 0.
		//
		// Outside the raw_unavailable branch deliberately. It comes from the
		// stored column, so it is just as good on a turn whose raw_json is
		// unreadable, which is where it is worth the most.
		if names := failedTools[t.ID]; len(names) > 0 {
			turn["failed_tools"] = names
		}

		compactTurns = append(compactTurns, turn)
	}

	response := map[string]interface{}{
		"session_id":  sessionID,
		"total_turns": totalCount,
		"offset":      offset,
		"limit":       limit,
		"count":       len(compactTurns),
		"turns":       compactTurns,
	}

	// One count and one warning for the page, rather than one warning per
	// affected turn: at the measured rate a 50-turn page of a claude-code
	// session can carry 25 of them, and 25 copies of the same sentence
	// would bury the warnings a caller actually needs to read. Which turns
	// are affected is on the turns themselves, as raw_unavailable.
	if unreadableTurns > 0 {
		response["unreadable_turns"] = unreadableTurns
		response["warnings"] = []string{fmt.Sprintf(
			"turn enrichment unavailable: %d of %d returned turns have unreadable raw_json, "+
				"so has_thinking is omitted for them; those turns carry raw_unavailable: true. "+
				"Their tools come from the stored tool calls and are unaffected",
			unreadableTurns, len(compactTurns))}
	}

	// next_after_ordinal is the resume point for the page just returned, and
	// it is reported whether or not there is more: a caller polling a live
	// session wants to know where it got to even when it has caught up.
	if n := len(compactTurns); n > 0 {
		if ordinal, ok := compactTurns[n-1]["ordinal"].(int); ok {
			response["next_after_ordinal"] = ordinal
		}
	}

	if hasMore {
		response["next_offset"] = offset + limit
		response["has_more"] = true
	}

	return response, nil
}

func (s *Server) getSession(args map[string]interface{}) (interface{}, error) {
	sessionID, _ := args["session_id"].(string)
	if sessionID == "" {
		return nil, fmt.Errorf("session_id is required")
	}

	session, err := s.db.GetSession(sessionID)
	if err != nil {
		return nil, fmt.Errorf("database error: %w", err)
	}
	if session == nil {
		return nil, fmt.Errorf("session not found: %s", sessionID)
	}

	turns, err := s.db.GetTurns(sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to get turns: %w", err)
	}

	// Get project info
	projectPath, projectWarning := s.lookupProjectPath(session.ProjectID)

	// For large sessions, recommend using get_session_summary + get_turns.
	// The notice lives in the same top-level `warnings` array as every
	// other degraded-response signal, so an agent has one field to check
	// rather than a singular `warning` string beside a plural `warnings`.
	if len(turns) > 100 {
		warnings := []string{
			fmt.Sprintf("markdown unavailable: large session with %d turns. Use get_session_summary and get_turns for better results.", len(turns)),
		}
		if projectWarning != "" {
			warnings = append(warnings, projectWarning)
		}
		return map[string]interface{}{
			"warnings":   warnings,
			"session_id": sessionID,
			"turn_count": len(turns),
			"hint":       "Call get_session_summary first, then use get_turns with offset/limit to paginate",
		}, nil
	}

	// Export to markdown for smaller sessions
	var buf strings.Builder
	exporter := export.NewMarkdownExporter(
		export.WithThinking(false), // Skip thinking to reduce size
	)
	if err := exporter.Export(&buf, session, turns, projectPath); err != nil {
		return nil, fmt.Errorf("export failed: %w", err)
	}

	content := buf.String()
	// Truncate if still too large
	if len(content) > 50000 {
		content = content[:50000] + "\n\n... [truncated - use get_turns for full content]"
	}

	result := map[string]interface{}{
		"session_id": sessionID,
		"turn_count": len(turns),
		"markdown":   content,
	}
	if projectWarning != "" {
		result["warnings"] = []string{projectWarning}
	}
	return result, nil
}

func (s *Server) listSessions(args map[string]interface{}) (interface{}, error) {
	limit := 20
	if l, ok := args["limit"].(float64); ok {
		limit = int(l)
		if limit > 100 {
			limit = 100
		}
	}
	if limit <= 0 {
		limit = 20
	}

	offset := 0
	if o, ok := args["offset"].(float64); ok && o > 0 {
		offset = int(o)
	}

	var projectID int64
	if projectFilter, ok := args["project"].(string); ok && projectFilter != "" {
		// Class D — return all matches, don't silently pick one.
		projects, err := s.db.GetProjects("activity", 0, 0)
		if err != nil {
			return nil, fmt.Errorf("failed to get projects: %w", err)
		}
		matches := projectref.ResolveAll(projects, projectFilter)
		switch len(matches) {
		case 0:
			return nil, fmt.Errorf("project not found: %s", projectFilter)
		case 1:
			projectID = matches[0].ID
		default:
			// Ambiguous — return the candidate set so the agent can
			// re-issue the call with a more specific (usually the full
			// path) filter. Keep `count` and `sessions` in the response
			// (both empty) so existing clients that iterate
			// `resp.sessions` don't null-deref on ambiguous input —
			// the response is a superset of the success shape, plus
			// the `ambiguous_project_filter` signal + candidates.
			candidates := make([]map[string]any, len(matches))
			for i := range matches {
				candidates[i] = projectref.Ref(&matches[i])
			}
			return map[string]interface{}{
				"count":                    0,
				"sessions":                 []map[string]any{},
				"ambiguous_project_filter": true,
				"filter":                   projectFilter,
				"matched_projects":         candidates,
				"hint":                     "Filter matched multiple projects. Re-call list_sessions with 'project' set to one of matched_projects[].path.",
			}, nil
		}
	}

	// Default to top-level sessions only, the same default the CLI and TUI
	// use. The per-row subagent_count (always emitted) is what keeps that
	// from hiding anything, and get_session / get_turns reach a subagent id
	// with no flag at all.
	query := db.SessionQuery{
		ProjectID: projectID,
		// One extra row detects whether more sessions exist.
		Limit:  limit + 1,
		Offset: offset,
		Scope:  db.SubagentsHidden,
	}
	if include, ok := args["include_subagents"].(bool); ok && include {
		query.Scope = db.SubagentsIncluded
	}
	if parent, ok := args["subagents_of"].(string); ok && parent != "" {
		query.Scope = db.SubagentsOf
		query.ParentSessionID = parent
	}

	sessions, err := s.db.QuerySessions(query)
	if err != nil {
		return nil, fmt.Errorf("failed to get sessions: %w", err)
	}

	hasMore := len(sessions) > limit
	if hasMore {
		sessions = sessions[:limit]
	}

	// Class C — enrich sessions with {name, path} for each session's
	// project so agents get the doctrine shape. Adapter-provided
	// DisplayName is preserved via the ProjectsByID lookup. If the
	// lookup query fails, surface a warning rather than silently falling
	// back to basename for every session — the agent needs to know why
	// adapter branding is missing.
	allProjects, enrichErr := s.db.GetProjects("activity", 0, 0)
	projectsByID := projectref.ProjectsByID(allProjects)

	response := map[string]interface{}{
		"count":    len(sessions),
		"offset":   offset,
		"limit":    limit,
		"sessions": projectref.SessionRefsFromValues(sessions, projectsByID),
	}
	if enrichErr != nil {
		response["warnings"] = []string{
			fmt.Sprintf("project enrichment unavailable: %v (session project_name values fell back to basename)", enrichErr),
		}
	}
	if hasMore {
		response["has_more"] = true
		response["next_offset"] = offset + limit
		response["hint"] = fmt.Sprintf("More sessions exist. Fetch next page with offset=%d, or narrow with the project filter.", offset+limit)
	}

	return response, nil
}

func (s *Server) listProjects(args map[string]interface{}) (interface{}, error) {
	sortBy := "activity"
	if sb, ok := args["sort"].(string); ok && sb != "" {
		sortBy = sb
	}

	limit := 50
	if l, ok := args["limit"].(float64); ok {
		limit = int(l)
		if limit > 100 {
			limit = 100
		}
	}
	if limit <= 0 {
		limit = 50
	}

	offset := 0
	if o, ok := args["offset"].(float64); ok && o > 0 {
		offset = int(o)
	}

	// Fetch one extra row to detect whether more projects exist
	projects, err := s.db.GetProjects(sortBy, limit+1, offset)
	if err != nil {
		return nil, fmt.Errorf("failed to get projects: %w", err)
	}

	hasMore := len(projects) > limit
	if hasMore {
		projects = projects[:limit]
	}

	// Class C — emit each project with a Ref-doctrine {name, path}
	// shape, plus the operational fields agents need.
	response := map[string]interface{}{
		"count":    len(projects),
		"offset":   offset,
		"limit":    limit,
		"projects": projectref.EnrichedRefsFromValues(projects),
	}
	if hasMore {
		response["has_more"] = true
		response["next_offset"] = offset + limit
		response["hint"] = fmt.Sprintf("More projects exist. Fetch next page with offset=%d, or use sort to surface the relevant ones.", offset+limit)
	}

	return response, nil
}

func (s *Server) getStats(args map[string]interface{}) (interface{}, error) {
	projectCount, _, err := s.db.GetProjectStats()
	if err != nil {
		return nil, fmt.Errorf("get project stats: %w", err)
	}

	sessionCount, turnCount, totalTokens, err := s.db.GetSessionStats()
	if err != nil {
		return nil, fmt.Errorf("get session stats: %w", err)
	}

	tokensByModel, err := s.db.GetTokensByModel()
	if err != nil {
		return nil, fmt.Errorf("get tokens by model: %w", err)
	}

	var warnings []string

	firstActivity, lastActivity, err := s.db.GetFirstAndLastActivity()
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("activity range unavailable: %v", err))
	}

	result := map[string]interface{}{
		"projects":     projectCount,
		"sessions":     sessionCount,
		"turns":        turnCount,
		"total_tokens": totalTokens,
		"models":       tokensByModel,
	}

	// Enrichment fields are omitted rather than emitted empty when their
	// query fails — an agent checking presence must not read a nil
	// top_tools as "this archive used no tools".
	toolStats, err := s.db.GetToolUsageStats(10)
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("tool stats unavailable: %v", err))
	} else {
		result["top_tools"] = toolStats
	}

	if !firstActivity.IsZero() {
		result["first_activity"] = firstActivity.Format(time.RFC3339)
	}
	if !lastActivity.IsZero() {
		result["last_activity"] = lastActivity.Format(time.RFC3339)
	}
	if !firstActivity.IsZero() && !lastActivity.IsZero() {
		result["days_span"] = int(lastActivity.Sub(firstActivity).Hours() / 24)
	}
	if len(warnings) > 0 {
		result["warnings"] = warnings
	}

	return result, nil
}

func (s *Server) getAnalytics(args map[string]interface{}) (interface{}, error) {
	days := 30
	if d, ok := args["days"].(float64); ok {
		days = int(d)
	}

	result := make(map[string]interface{})

	// Warnings live in exactly one place on this response: top-level
	// `warnings`. Degraded stats queries and degraded DuckDB queries both
	// land here, so an agent has a single list to check.
	var warnings []string

	// Get basic stats
	stats, err := s.getStats(nil)
	if err != nil {
		return nil, err
	}
	if summary, ok := stats.(map[string]interface{}); ok {
		if statsWarnings, ok := summary["warnings"].([]string); ok {
			warnings = append(warnings, statsWarnings...)
			delete(summary, "warnings")
		}
	}
	result["summary"] = stats

	// DuckDB analytics: report failures instead of silently omitting sections
	if s.analyzer != nil {
		dailyTokens, err := s.analyzer.GetTokensByDay(days)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("tokens_by_day unavailable: %v", err))
		} else {
			result["tokens_by_day"] = dailyTokens
		}

		topProjects, err := s.analyzer.GetTopProjects(10)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("top_projects unavailable: %v", err))
		} else {
			result["top_projects"] = topProjects
		}

		modelStats, err := s.analyzer.GetTokensByModel()
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("model_breakdown unavailable: %v", err))
		} else {
			result["model_breakdown"] = modelStats
		}
	} else {
		reason := "analytics cache not initialized"
		if s.analyzerErr != nil {
			reason = s.analyzerErr.Error()
		}
		result["analytics"] = map[string]interface{}{
			"available": false,
			"reason":    reason,
			"hint":      "Run 'ccvault build-cache' to enable DuckDB analytics",
		}
	}

	if len(warnings) > 0 {
		result["warnings"] = warnings
	}

	return result, nil
}

// Prompt implementations

func (s *Server) promptSummarizeRecent(args map[string]interface{}) (promptGetResult, error) {
	days := 7
	if d, ok := args["days"].(float64); ok {
		days = int(d)
	}

	// Get recent stats
	stats, err := s.getStats(nil)
	if err != nil {
		return promptGetResult{}, err
	}

	// Get recent sessions. Top-level only, the same default list_sessions
	// uses — a narrative summary of "recent activity" reads as sessions the
	// user started, not as the agent dispatches inside them.
	sessions, err := s.db.QuerySessions(db.SessionQuery{Limit: 20, Scope: db.SubagentsHidden})
	if err != nil {
		return promptGetResult{}, err
	}

	// Build context
	var context strings.Builder
	fmt.Fprintf(&context, "## Claude Code Activity Summary (Last %d Days)\n\n", days)
	context.WriteString("### Archive Statistics\n")
	// The stats block IS the prompt's data, so a failed marshal must error
	// rather than hand the model an empty code fence it would read as
	// "the archive is empty".
	statsJSON, err := json.MarshalIndent(stats, "", "  ")
	if err != nil {
		return promptGetResult{}, fmt.Errorf("marshal archive stats: %w", err)
	}
	context.WriteString("```json\n")
	context.WriteString(string(statsJSON))
	context.WriteString("\n```\n\n")

	context.WriteString("### Recent Sessions\n")
	for _, sess := range sessions {
		fmt.Fprintf(&context, "- **%s** (%s): %d turns, %s model\n",
			sess.ID[:8],
			sess.StartedAt.Format("Jan 2 15:04"),
			sess.TurnCount,
			shortenModel(sess.Model))
	}

	return promptGetResult{
		Description: fmt.Sprintf("Summary of Claude Code activity over the last %d days", days),
		Messages: []promptMessage{
			{
				Role: "user",
				Content: contentItem{
					Type: "text",
					Text: fmt.Sprintf("Please analyze and summarize my Claude Code usage over the last %d days. Here's the data:\n\n%s\n\nProvide insights on:\n1. Overall usage patterns\n2. Most active projects\n3. Model preferences\n4. Notable trends", days, context.String()),
				},
			},
		},
	}, nil
}

func (s *Server) promptAnalyzeProject(args map[string]interface{}) (promptGetResult, error) {
	projectName, _ := args["project"].(string)
	if projectName == "" {
		return promptGetResult{}, fmt.Errorf("project argument is required")
	}

	// Find project
	projects, err := s.db.GetProjects("activity", 0, 0)
	if err != nil {
		return promptGetResult{}, err
	}

	// Class D — never silently pick a match. A prompt is a single-analysis
	// contract; on ambiguous input the caller must narrow.
	matches := projectref.ResolveAll(projects, projectName)
	if len(matches) == 0 {
		return promptGetResult{}, fmt.Errorf("project not found: %s", projectName)
	}
	if len(matches) > 1 {
		var paths []string
		for _, m := range matches {
			paths = append(paths, m.Path)
		}
		return promptGetResult{}, fmt.Errorf(
			"filter %q matched multiple projects; re-call with an exact path:\n  %s",
			projectName, strings.Join(paths, "\n  "),
		)
	}
	project := &matches[0]

	// Get sessions for this project — top-level only, matching list_sessions.
	sessions, err := s.db.QuerySessions(db.SessionQuery{
		ProjectID: project.ID,
		Limit:     50,
		Scope:     db.SubagentsHidden,
	})
	if err != nil {
		return promptGetResult{}, err
	}

	var context strings.Builder
	// Class B — combined inline form so a same-basename project doesn't
	// produce an ambiguous prompt header.
	fmt.Fprintf(&context, "## Project Analysis: %s\n\n", projectref.Inline(project))
	fmt.Fprintf(&context, "- **Path**: %s\n", project.Path)
	fmt.Fprintf(&context, "- **Sessions**: %d\n", project.SessionCount)
	fmt.Fprintf(&context, "- **Total Tokens**: %d\n", project.TotalTokens)
	fmt.Fprintf(&context, "- **First Seen**: %s\n", project.FirstSeenAt.Format("Jan 2, 2006"))
	fmt.Fprintf(&context, "- **Last Activity**: %s\n\n", project.LastActivityAt.Format("Jan 2, 2006"))

	context.WriteString("### Session History\n")
	for _, sess := range sessions {
		fmt.Fprintf(&context, "- %s: %d turns, %s\n",
			sess.StartedAt.Format("Jan 2 15:04"),
			sess.TurnCount,
			shortenModel(sess.Model))
	}

	return promptGetResult{
		Description: fmt.Sprintf("Analysis of Claude Code usage for project: %s", projectref.Inline(project)),
		Messages: []promptMessage{
			{
				Role: "user",
				Content: contentItem{
					Type: "text",
					Text: fmt.Sprintf("Analyze my Claude Code usage for this project:\n\n%s\n\nProvide insights on:\n1. How I've been using Claude in this project\n2. Common tasks and patterns\n3. Suggestions for more effective usage", context.String()),
				},
			},
		},
	}, nil
}

func (s *Server) promptFindSolutions(args map[string]interface{}) (promptGetResult, error) {
	topic, _ := args["topic"].(string)
	if topic == "" {
		return promptGetResult{}, fmt.Errorf("topic argument is required")
	}

	// Search for relevant conversations
	parsed := search.Parse(topic)
	searcher := search.New(s.db.DB)
	results, err := searcher.Search(parsed, 10)
	if err != nil {
		return promptGetResult{}, err
	}

	var context strings.Builder
	fmt.Fprintf(&context, "## Search Results for: %s\n\n", topic)
	fmt.Fprintf(&context, "Found %d relevant conversations:\n\n", len(results))

	for i, r := range results {
		fmt.Fprintf(&context, "### Result %d\n", i+1)
		fmt.Fprintf(&context, "- **Session**: %s\n", r.SessionID[:8])
		fmt.Fprintf(&context, "- **Date**: %s\n", r.Turn.Timestamp.Format("Jan 2, 2006 15:04"))
		fmt.Fprintf(&context, "- **Type**: %s\n", r.Turn.Type)
		fmt.Fprintf(&context, "- **Snippet**: %s\n\n", r.Snippet)
	}

	return promptGetResult{
		Description: fmt.Sprintf("Past solutions and approaches for: %s", topic),
		Messages: []promptMessage{
			{
				Role: "user",
				Content: contentItem{
					Type: "text",
					Text: fmt.Sprintf("I'm looking for past solutions related to: %s\n\nHere are relevant conversations from my Claude Code history:\n\n%s\n\nPlease:\n1. Summarize the approaches used\n2. Identify common patterns\n3. Suggest which sessions might be most helpful to review in detail", topic, context.String()),
				},
			},
		},
	}, nil
}

func (s *Server) promptReviewSession(args map[string]interface{}) (promptGetResult, error) {
	sessionID, _ := args["session_id"].(string)
	if sessionID == "" {
		return promptGetResult{}, fmt.Errorf("session_id argument is required")
	}

	session, err := s.db.GetSession(sessionID)
	if err != nil || session == nil {
		return promptGetResult{}, fmt.Errorf("session not found: %s", sessionID)
	}

	turns, err := s.db.GetTurns(sessionID)
	if err != nil {
		return promptGetResult{}, err
	}

	// Get project info — cosmetic in a prompt, so lookup failures only log
	projectPath, projectWarning := s.lookupProjectPath(session.ProjectID)
	if projectWarning != "" {
		s.log("%s", projectWarning)
	}

	// Export to markdown for easier reading; the markdown IS the prompt,
	// so a failed export must error rather than produce an empty review
	var buf strings.Builder
	exporter := export.NewMarkdownExporter(
		export.WithThinking(false), // Skip thinking for summary
	)
	if err := exporter.Export(&buf, session, turns, projectPath); err != nil {
		return promptGetResult{}, fmt.Errorf("export session markdown: %w", err)
	}

	return promptGetResult{
		Description: fmt.Sprintf("Review of session %s", sessionID[:8]),
		Messages: []promptMessage{
			{
				Role: "user",
				Content: contentItem{
					Type: "text",
					Text: fmt.Sprintf("Please review and summarize this Claude Code session:\n\n%s\n\nProvide:\n1. A brief summary of what was accomplished\n2. Key decisions made\n3. Any notable tools or techniques used\n4. Lessons learned or improvements for next time", buf.String()),
				},
			},
		},
	}, nil
}

func (s *Server) promptCompareApproaches(args map[string]interface{}) (promptGetResult, error) {
	topic, _ := args["topic"].(string)
	if topic == "" {
		return promptGetResult{}, fmt.Errorf("topic argument is required")
	}

	// Search for relevant conversations
	parsed := search.Parse(topic)
	searcher := search.New(s.db.DB)
	results, err := searcher.Search(parsed, 20)
	if err != nil {
		return promptGetResult{}, err
	}

	// Group by session
	sessionSnippets := make(map[string][]string)
	for _, r := range results {
		sessionSnippets[r.SessionID] = append(sessionSnippets[r.SessionID], r.Snippet)
	}

	var context strings.Builder
	fmt.Fprintf(&context, "## Comparing Approaches for: %s\n\n", topic)
	fmt.Fprintf(&context, "Found %d sessions with relevant content:\n\n", len(sessionSnippets))

	i := 0
	for sessionID, snippets := range sessionSnippets {
		if i >= 5 {
			break
		}
		// Date and model are decoration on top of the snippets that carry
		// the comparison, so a failed lookup drops that one line and logs
		// rather than failing the prompt — the same call this file makes
		// for a prompt's cosmetic project lookup.
		session, err := s.db.GetSession(sessionID)
		if err != nil {
			s.log("session %s unavailable for compare_approaches header: %v", sessionID, err)
		}
		fmt.Fprintf(&context, "### Session %d (%s)\n", i+1, sessionID[:8])
		if session != nil {
			fmt.Fprintf(&context, "Date: %s, Model: %s\n", session.StartedAt.Format("Jan 2"), shortenModel(session.Model))
		}
		for _, snippet := range snippets {
			fmt.Fprintf(&context, "- %s\n", snippet)
		}
		context.WriteString("\n")
		i++
	}

	return promptGetResult{
		Description: fmt.Sprintf("Comparison of approaches for: %s", topic),
		Messages: []promptMessage{
			{
				Role: "user",
				Content: contentItem{
					Type: "text",
					Text: fmt.Sprintf("Compare the different approaches I've used for: %s\n\n%s\n\nAnalyze:\n1. Different strategies attempted\n2. What worked well vs. what didn't\n3. Evolution of approach over time\n4. Recommended best practices based on past experience", topic, context.String()),
				},
			},
		},
	}, nil
}

func (s *Server) promptToolUsageReport(args map[string]interface{}) (promptGetResult, error) {
	specificTool, _ := args["tool"].(string)

	toolStats, err := s.db.GetToolUsageStats(20)
	if err != nil {
		return promptGetResult{}, err
	}

	var context strings.Builder
	context.WriteString("## Tool Usage Report\n\n")
	context.WriteString("### Tool Frequency\n")
	for tool, count := range toolStats {
		fmt.Fprintf(&context, "- **%s**: %d uses\n", tool, count)
	}

	if specificTool != "" {
		fmt.Fprintf(&context, "\n### Focus: %s\n", specificTool)
		// Could add more detailed analysis for specific tool
	}

	return promptGetResult{
		Description: "Analysis of tool usage patterns",
		Messages: []promptMessage{
			{
				Role: "user",
				Content: contentItem{
					Type: "text",
					Text: fmt.Sprintf("Analyze my Claude Code tool usage:\n\n%s\n\nProvide insights on:\n1. Most relied-upon tools\n2. Tool usage patterns\n3. Suggestions for tools I might be underutilizing\n4. Workflow optimization opportunities", context.String()),
				},
			},
		},
	}, nil
}

// Helper functions

// shortenModel is a thin adapter around compact.Model that discards the
// Shortened flag (MCP prompt content doesn't have styling). Preserves
// semantic identity — NEVER strips a trailing datestamp, because
// opus-4-5-20251101 is a genuinely different model from opus-4-5.
// See internal/compact/compact.go for the discipline.
func shortenModel(model string) string {
	return compact.Model(model, 20).Text
}

func (s *Server) sendResult(id interface{}, result interface{}) {
	resp := jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}
	s.send(id, resp)
}

func (s *Server) sendError(id interface{}, code int, message string, data interface{}) {
	resp := jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &rpcError{
			Code:    code,
			Message: message,
			Data:    data,
		},
	}
	s.send(id, resp)
}

// send writes one JSON-RPC response to the transport.
//
// id is passed alongside the assembled response because a response that
// will not marshal still has to be answered. The client is holding an
// outstanding request id; dropping the write leaves it waiting for a
// reply that never comes, which blocks the conversation until the client
// times out — if it has a timeout at all. So a marshal failure answers
// that id with a protocol error instead of silence.
//
// Notifications never reach here: handleRequest returns without sending
// for every notifications/* method and for an unknown method with no id,
// per JSON-RPC 2.0. Nothing is waiting on a notification, so there is
// nothing to answer. The one response that legitimately carries a nil id
// is the parse error, where the request's id could not be read.
//
// tools/call already catches an unserializable tool payload one layer up
// — handleToolsCall marshals the payload into text itself and turns a
// failure there into an isError result — so no tool response reaches this
// fallback today. That is one handler's accident, not a protocol
// guarantee: initialize, tools/list, prompts/* and ping all hand their
// results straight here, and a future field of theirs could fail. The
// guarantee belongs at the one place every response passes through.
func (s *Server) send(id interface{}, v interface{}) {
	data, err := json.Marshal(v)
	if err != nil {
		s.log("Marshal error: %v", err)
		data = marshalFailureResponse(id)
	}
	s.log("Sending: %s", string(data))
	_, _ = fmt.Fprintln(s.out, string(data))
}

// marshalFailureResponse builds the reply sent in place of a response that
// would not marshal. It carries a fixed code and message and none of the
// caller's payload, so it cannot fail for the reason the response it
// replaces did — echoing the payload back would reproduce the failure and
// hang the client anyway.
//
// If even the id will not marshal, the id is dropped rather than the
// reply: a reply with a null id is still something the client can react
// to, while silence is not. What remains is literals, which marshal.
func marshalFailureResponse(id interface{}) []byte {
	resp := jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &rpcError{
			Code:    internalErrorCode,
			Message: "Internal error",
			Data:    "the response could not be serialized",
		},
	}
	if data, err := json.Marshal(resp); err == nil {
		return data
	}

	resp.ID = nil
	data, err := json.Marshal(resp)
	if err != nil {
		// Unreachable: resp now holds only a string, an int and nil.
		return []byte(`{"jsonrpc":"2.0","id":null,` +
			`"error":{"code":-32603,"message":"Internal error"}}`)
	}
	return data
}
