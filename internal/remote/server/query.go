// ABOUTME: Read-side handlers: search, sessions, show, stats
// ABOUTME: Reuse internal/search and internal/db as-is; stream results as ndjson

package server

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/2389-research/ccvault/internal/search"
)

func handleSearch(ctx HandlerCtx) int {
	args := parseKV(ctx.Args)
	q := args["q"]
	limit := 20
	if s, ok := args["limit"]; ok {
		if n, err := strconv.Atoi(s); err == nil {
			limit = n
		}
	}
	query := search.Parse(q)
	s := search.New(ctx.Server.db.DB)
	results, err := s.Search(query, limit)
	if err != nil {
		_, _ = fmt.Fprintf(ctx.Stderr, "search: %v\n", err)
		return 1
	}
	enc := json.NewEncoder(ctx.Stdout)
	for _, r := range results {
		if err := enc.Encode(r); err != nil {
			return 1
		}
	}
	return 0
}

func handleSessions(ctx HandlerCtx) int {
	args := parseKV(ctx.Args)
	limit := 50
	if s, ok := args["limit"]; ok {
		if n, err := strconv.Atoi(s); err == nil {
			limit = n
		}
	}
	var projectID int64 = 0
	// TODO: project filter deferred; v1 lists all
	sessions, err := ctx.Server.db.GetSessions(projectID, limit)
	if err != nil {
		_, _ = fmt.Fprintf(ctx.Stderr, "sessions: %v\n", err)
		return 1
	}
	enc := json.NewEncoder(ctx.Stdout)
	for _, s := range sessions {
		if err := enc.Encode(s); err != nil {
			return 1
		}
	}
	return 0
}

func handleShow(ctx HandlerCtx) int {
	args := parseKV(ctx.Args)
	id := args["id"]
	if id == "" {
		_, _ = fmt.Fprintln(ctx.Stderr, "show: id=<session-id> required")
		return 2
	}
	session, err := ctx.Server.db.GetSession(id)
	if err != nil || session == nil {
		_, _ = fmt.Fprintf(ctx.Stderr, "session not found: %s\n", id)
		return 2
	}
	turns, err := ctx.Server.db.GetTurns(id)
	if err != nil {
		_, _ = fmt.Fprintf(ctx.Stderr, "get turns: %v\n", err)
		return 1
	}
	resp := map[string]any{"session": session, "turns": turns}
	return writeJSON(ctx, resp)
}

func handleStats(ctx HandlerCtx) int {
	d := ctx.Server.db
	projects, projectTokens, _ := d.GetProjectStats()
	sessions, turns, tokens, _ := d.GetSessionStats()
	first, last, _ := d.GetFirstAndLastActivity()
	tools, _ := d.GetToolUsageStats(10)
	tokensByModel, _ := d.GetTokensByModel()

	resp := map[string]any{
		"projects":        projects,
		"project_tokens":  projectTokens,
		"sessions":        sessions,
		"turns":           turns,
		"tokens":          tokens,
		"first_activity":  first,
		"last_activity":   last,
		"tool_usage":      tools,
		"tokens_by_model": tokensByModel,
	}
	return writeJSON(ctx, resp)
}

func writeJSON(ctx HandlerCtx, v any) int {
	if err := json.NewEncoder(ctx.Stdout).Encode(v); err != nil {
		_, _ = fmt.Fprintln(ctx.Stderr, err)
		return 1
	}
	return 0
}

func parseKV(args string) map[string]string {
	out := map[string]string{}
	for _, part := range tokenize(args) {
		if i := strings.Index(part, "="); i >= 0 {
			out[part[:i]] = part[i+1:]
		}
	}
	return out
}

// tokenize splits a command-argument string into whitespace-separated tokens
// while respecting double-quoted spans, so `q="two words" project=foo` yields
// [`q=two words`, `project=foo`]. Backslash escapes a following character
// inside a quoted span (`"has \"nested\" quotes"` → `has "nested" quotes`).
// Unterminated quotes and stray escapes are handled best-effort: the current
// token is emitted as-is at end of input.
func tokenize(s string) []string {
	var tokens []string
	var cur strings.Builder
	inQuote := false
	hasContent := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && inQuote && i+1 < len(s):
			// Escape sequence inside quotes — take the next byte literally.
			cur.WriteByte(s[i+1])
			hasContent = true
			i++
		case c == '"':
			inQuote = !inQuote
			hasContent = true
		case (c == ' ' || c == '\t') && !inQuote:
			if hasContent {
				tokens = append(tokens, cur.String())
				cur.Reset()
				hasContent = false
			}
		default:
			cur.WriteByte(c)
			hasContent = true
		}
	}
	if hasContent {
		tokens = append(tokens, cur.String())
	}
	return tokens
}
