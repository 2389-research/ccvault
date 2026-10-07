// ABOUTME: MCP server for ccvault AI integration
// ABOUTME: Implements JSON-RPC 2.0 over stdio for Model Context Protocol

package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/2389-research/ccvault/internal/analytics"
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
	debug       bool
	out         io.Writer // stdout by default; overridable for tests
}

// NewServer creates a new MCP server
func NewServer(database *db.DB, cfg *config.Config) (*Server, error) {
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
		debug:       os.Getenv("CCVAULT_MCP_DEBUG") == "1",
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

// Run starts the MCP server on stdio
func (s *Server) Run() error {
	s.log("Starting MCP server")
	reader := bufio.NewReader(os.Stdin)

	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				s.log("EOF received, shutting down")
				return nil
			}
			return fmt.Errorf("read stdin: %w", err)
		}

		s.log("Received: %s", strings.TrimSpace(string(line)))

		var req jsonRPCRequest
		if err := json.Unmarshal(line, &req); err != nil {
			s.log("Parse error: %v", err)
			s.sendError(nil, -32700, "Parse error", err.Error())
			continue
		}

		s.handleRequest(&req)
	}
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

func (s *Server) handleInitialize(req *jsonRPCRequest) {
	result := initializeResult{
		ProtocolVersion: "2024-11-05",
		Capabilities: capabilities{
			Tools:   &toolsCapability{},
			Prompts: &promptsCapability{},
		},
		ServerInfo: serverInfo{
			Name:    "ccvault",
			Version: "0.1.0",
		},
	}
	s.sendResult(req.ID, result)
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
						Type:        "string",
						Description: "Search query. Supports: project:name, model:opus, tool:Bash, before:2024-01-01, after:2024-01-01, \"exact phrase\"",
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
				"messages. Use this first before get_turns. tools_used is counted from each assistant " +
				"turn's raw_json; when some turns cannot be read the count is incomplete, and the " +
				"response says so with unreadable_turns and a 'tools_used incomplete' warnings[] entry.",
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
				"but counts rows in the last response rather than naming a turn. An assistant turn whose " +
				"raw_json could not be read carries raw_unavailable: true and omits tools and " +
				"has_thinking — absence of tools on such a turn is not evidence it called none. The page " +
				"reports how many in unreadable_turns plus one warnings[] entry.",
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
	allProjects, enrichErr := s.db.GetProjects("activity", 0)
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

// readTurnEnrichment pulls the tool names and the thinking flag out of an
// assistant turn's raw_json. ok is false when raw_json cannot be read at
// all — absent, because db.GetTurns dropped bytes that were not valid
// JSON, or present in a shape these fields do not live in. Callers must
// branch on ok rather than on an empty tool list: no tools and no answer
// are different facts, and conflating them is the defect this returns ok
// to prevent.
//
// The parse stays tolerant of unknown keys because raw_json's shape
// differs across the source adapters; only a shape that cannot yield
// these fields at all counts as a failure.
func readTurnEnrichment(rawJSON []byte) (tools []string, hasThinking, ok bool) {
	if len(rawJSON) == 0 {
		return nil, false, false
	}

	var raw struct {
		Message struct {
			Content []struct {
				Type     string `json:"type"`
				Name     string `json:"name,omitempty"`
				Thinking string `json:"thinking,omitempty"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(rawJSON, &raw); err != nil {
		return nil, false, false
	}

	for _, c := range raw.Message.Content {
		switch c.Type {
		case "tool_use":
			tools = append(tools, c.Name)
		case "thinking":
			hasThinking = true
		}
	}
	return tools, hasThinking, true
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

	// Count turn types and extract tool usage
	turnTypeCounts := make(map[string]int)
	toolCounts := make(map[string]int)
	var firstUserMsg, lastUserMsg string

	// tools_used aggregates tool calls over every assistant turn, so a turn
	// whose raw_json cannot be read silently undercounts it — and an empty
	// tools_used otherwise reads as a session that called no tools. Count
	// the turns that did not contribute so the response can say so.
	unreadableTurns := 0

	for _, t := range turns {
		turnTypeCounts[t.Type]++

		// Track tool usage from raw JSON
		if t.Type == "assistant" {
			tools, _, ok := readTurnEnrichment(t.RawJSON)
			if !ok {
				unreadableTurns++
			}
			for _, name := range tools {
				if name != "" {
					toolCounts[name]++
				}
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

	// Build top tools list
	type toolCount struct {
		name  string
		count int
	}
	var topTools []toolCount
	for name, count := range toolCounts {
		topTools = append(topTools, toolCount{name, count})
	}
	// Sort by count desc
	for i := 0; i < len(topTools); i++ {
		for j := i + 1; j < len(topTools); j++ {
			if topTools[j].count > topTools[i].count {
				topTools[i], topTools[j] = topTools[j], topTools[i]
			}
		}
	}
	// Take top 10
	if len(topTools) > 10 {
		topTools = topTools[:10]
	}
	topToolsMap := make([]map[string]interface{}, len(topTools))
	for i, t := range topTools {
		topToolsMap[i] = map[string]interface{}{"tool": t.name, "count": t.count}
	}

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
	// the readable turns did report, which on a half-corrupt session is
	// most of them.
	if unreadableTurns > 0 {
		result["unreadable_turns"] = unreadableTurns
		warnings = append(warnings, fmt.Sprintf(
			"tools_used incomplete: %d of %d assistant turns have unreadable raw_json, "+
				"so the tools they called are not counted",
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

		// tools and has_thinking are read out of the turn's raw_json. When
		// that cannot be read the fields are omitted, and the turn says so
		// — otherwise a turn we could not read is indistinguishable from a
		// turn that used no tools, and a caller has no way to know which
		// of its results to distrust.
		//
		// Both failure routes are real. Measured over the 46k-session
		// archive (493,908 assistant turns): 52,851 reach here with no
		// raw_json at all, because db.GetTurns drops bytes that are not
		// valid JSON, and those rows are the mid-document fragments left
		// by claude-code syncs predating the oversized-line fix. Zero
		// turns had valid raw_json in a shape this struct would not fit,
		// so that route is guarded rather than observed.
		if t.Type == "assistant" {
			tools, hasThinking, ok := readTurnEnrichment(t.RawJSON)
			if ok {
				if len(tools) > 0 {
					turn["tools"] = tools
				}
				turn["has_thinking"] = hasThinking
			} else {
				turn["raw_unavailable"] = true
				unreadableTurns++
			}
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
				"so tools and has_thinking are omitted for them; those turns carry raw_unavailable: true",
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
		projects, err := s.db.GetProjects("activity", 0)
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
	allProjects, enrichErr := s.db.GetProjects("activity", 0)
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
	projects, err := s.db.GetProjectsPage(sortBy, limit+1, offset)
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
	projects, err := s.db.GetProjects("activity", 0)
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
