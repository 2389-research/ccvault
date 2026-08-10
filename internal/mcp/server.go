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
			Name:        "get_session_summary",
			Description: "Get a quick overview of a session: metadata, stats, tools used, and first/last messages. Use this first before get_turns.",
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
			Name:        "get_turns",
			Description: "Get paginated turns from a session. Use offset to navigate through conversation.",
			InputSchema: inputSchema{
				Type: "object",
				Properties: map[string]property{
					"session_id": {
						Type:        "string",
						Description: "Session UUID",
					},
					"offset": {
						Type:        "number",
						Description: "Skip first N turns (default: 0)",
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
			Description: "Get full session in markdown format. WARNING: Large sessions may be truncated. Use get_session_summary + get_turns for big sessions.",
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
			Description: "List recent sessions, optionally filtered by project.",
			InputSchema: inputSchema{
				Type: "object",
				Properties: map[string]property{
					"project": {
						Type:        "string",
						Description: "Filter by project path or name (partial match)",
					},
					"limit": {
						Type:        "number",
						Description: "Maximum sessions to return (default 20, max 100)",
					},
				},
			},
		},
		{
			Name:        "list_projects",
			Description: "List all indexed projects with session counts and token usage.",
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
						Description: "Maximum projects to return (default 50, max 100)",
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

	// Transform to compact results
	compactResults := make([]map[string]interface{}, 0, len(results))
	for _, r := range results {
		// Truncate snippet to 200 chars
		snippet := r.Snippet
		if len(snippet) > 200 {
			snippet = snippet[:200] + "..."
		}

		compactResults = append(compactResults, map[string]interface{}{
			"session_id":   r.SessionID,
			"turn_id":      r.Turn.ID,
			"turn_type":    r.Turn.Type,
			"timestamp":    r.Turn.Timestamp.Format(time.RFC3339),
			"project_path": r.ProjectPath,
			"model":        r.Model,
			"source":       r.Source,
			"snippet":      snippet,
		})
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
			if names, err := s.db.GetToolNamesLike(parsed.Tool, 5); err == nil && len(names) > 0 {
				response["similar_tool_names"] = names
				hint = fmt.Sprintf("No results for tool:%s — tool matching requires the full tool name. See similar_tool_names for close matches.", parsed.Tool)
			}
		}
		response["hint"] = hint
	}

	return response, nil
}

// lookupProjectPath resolves a session's project path. Failures come back
// as warning strings so callers surface them instead of dropping them.
func (s *Server) lookupProjectPath(projectID int64) (string, []string) {
	if projectID <= 0 {
		return "", nil
	}

	project, err := s.db.GetProject(projectID)
	switch {
	case err != nil:
		return "", []string{fmt.Sprintf("project lookup failed for project %d: %v", projectID, err)}
	case project == nil:
		return "", []string{fmt.Sprintf("project %d not found — session references a missing project", projectID)}
	default:
		return project.Path, nil
	}
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

	// Get project info
	projectPath, warnings := s.lookupProjectPath(session.ProjectID)

	// Count turn types and extract tool usage
	turnTypeCounts := make(map[string]int)
	toolCounts := make(map[string]int)
	var firstUserMsg, lastUserMsg string

	for i, t := range turns {
		turnTypeCounts[t.Type]++

		// Track tool usage from raw JSON
		if t.Type == "assistant" && len(t.RawJSON) > 0 {
			var raw struct {
				Message struct {
					Content []struct {
						Type string `json:"type"`
						Name string `json:"name,omitempty"`
					} `json:"content"`
				} `json:"message"`
			}
			if json.Unmarshal(t.RawJSON, &raw) == nil {
				for _, c := range raw.Message.Content {
					if c.Type == "tool_use" && c.Name != "" {
						toolCounts[c.Name]++
					}
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
				// Keep track of index for "last"
				_ = i
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
		"hint":           "Use get_turns to paginate through the conversation",
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

	limit := 20
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

	// Apply offset
	if offset >= len(turns) {
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
	for _, t := range turns {
		content := t.Content
		// Truncate long content
		if len(content) > 1000 {
			content = content[:1000] + "... [truncated, " + fmt.Sprintf("%d", len(t.Content)) + " chars total]"
		}

		turn := map[string]interface{}{
			"id":        t.ID,
			"type":      t.Type,
			"timestamp": t.Timestamp.Format(time.RFC3339),
			"content":   content,
		}

		// Extract tool info for assistant turns
		if t.Type == "assistant" && len(t.RawJSON) > 0 {
			var raw struct {
				Message struct {
					Content []struct {
						Type     string `json:"type"`
						Name     string `json:"name,omitempty"`
						Thinking string `json:"thinking,omitempty"`
					} `json:"content"`
				} `json:"message"`
			}
			if json.Unmarshal(t.RawJSON, &raw) == nil {
				var tools []string
				hasThinking := false
				for _, c := range raw.Message.Content {
					if c.Type == "tool_use" {
						tools = append(tools, c.Name)
					}
					if c.Type == "thinking" {
						hasThinking = true
					}
				}
				if len(tools) > 0 {
					turn["tools"] = tools
				}
				turn["has_thinking"] = hasThinking
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
	projectPath, warnings := s.lookupProjectPath(session.ProjectID)

	// For large sessions, recommend using get_session_summary + get_turns
	if len(turns) > 100 {
		result := map[string]interface{}{
			"warning":    "Large session with " + fmt.Sprintf("%d", len(turns)) + " turns. Use get_session_summary and get_turns for better results.",
			"session_id": sessionID,
			"turn_count": len(turns),
			"hint":       "Call get_session_summary first, then use get_turns with offset/limit to paginate",
		}
		if len(warnings) > 0 {
			result["warnings"] = warnings
		}
		return result, nil
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
	if len(warnings) > 0 {
		result["warnings"] = warnings
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
			// path) filter. No sessions returned; that would silently
			// mix rows from different projects.
			candidates := make([]map[string]any, len(matches))
			for i := range matches {
				candidates[i] = projectref.Ref(&matches[i])
			}
			return map[string]interface{}{
				"ambiguous_project_filter": true,
				"filter":                   projectFilter,
				"matched_projects":         candidates,
				"hint":                     "Filter matched multiple projects. Re-call list_sessions with 'project' set to one of matched_projects[].path.",
			}, nil
		}
	}

	// Fetch one extra row to detect whether more sessions exist
	sessions, err := s.db.GetSessions(projectID, limit+1)
	if err != nil {
		return nil, fmt.Errorf("failed to get sessions: %w", err)
	}

	hasMore := len(sessions) > limit
	if hasMore {
		sessions = sessions[:limit]
	}

	response := map[string]interface{}{
		"count":    len(sessions),
		"sessions": sessions,
	}
	if hasMore {
		response["has_more"] = true
		response["hint"] = "More sessions exist. Raise limit (max 100) or narrow with the project filter."
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

	// Fetch one extra row to detect whether more projects exist
	projects, err := s.db.GetProjects(sortBy, limit+1)
	if err != nil {
		return nil, fmt.Errorf("failed to get projects: %w", err)
	}

	hasMore := len(projects) > limit
	if hasMore {
		projects = projects[:limit]
	}

	response := map[string]interface{}{
		"count":    len(projects),
		"projects": projects,
	}
	if hasMore {
		response["has_more"] = true
		response["hint"] = "More projects exist. Raise limit (max 100) or use sort to surface the relevant ones."
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

	toolStats, err := s.db.GetToolUsageStats(10)
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("tool stats unavailable: %v", err))
	}

	result := map[string]interface{}{
		"projects":     projectCount,
		"sessions":     sessionCount,
		"turns":        turnCount,
		"total_tokens": totalTokens,
		"models":       tokensByModel,
		"top_tools":    toolStats,
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

	// Get basic stats
	stats, err := s.getStats(nil)
	if err != nil {
		return nil, err
	}
	result["summary"] = stats

	// DuckDB analytics: report failures instead of silently omitting sections
	if s.analyzer != nil {
		var warnings []string

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

		if len(warnings) > 0 {
			result["warnings"] = warnings
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

	// Get recent sessions
	sessions, err := s.db.GetSessions(0, 20)
	if err != nil {
		return promptGetResult{}, err
	}

	// Build context
	var context strings.Builder
	fmt.Fprintf(&context, "## Claude Code Activity Summary (Last %d Days)\n\n", days)
	context.WriteString("### Archive Statistics\n")
	statsJSON, _ := json.MarshalIndent(stats, "", "  ")
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

	// Get sessions for this project
	sessions, err := s.db.GetSessions(project.ID, 50)
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
	projectPath, projectWarnings := s.lookupProjectPath(session.ProjectID)
	for _, w := range projectWarnings {
		s.log("%s", w)
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
		session, _ := s.db.GetSession(sessionID)
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

func shortenModel(model string) string {
	if len(model) <= 20 {
		return model
	}
	parts := strings.Split(model, "-")
	if len(parts) >= 2 {
		return parts[1]
	}
	return model[:17] + "..."
}

func (s *Server) sendResult(id interface{}, result interface{}) {
	resp := jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}
	s.send(resp)
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
	s.send(resp)
}

func (s *Server) send(v interface{}) {
	data, err := json.Marshal(v)
	if err != nil {
		s.log("Marshal error: %v", err)
		return
	}
	s.log("Sending: %s", string(data))
	_, _ = fmt.Fprintln(s.out, string(data))
}
