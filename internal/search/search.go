// ABOUTME: Search execution for ccvault
// ABOUTME: Executes parsed queries against the database with FTS5

package search

import (
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/2389-research/ccvault/pkg/models"
)

// Searcher executes search queries
type Searcher struct {
	db *sql.DB
}

// New creates a new Searcher
func New(db *sql.DB) *Searcher {
	return &Searcher{db: db}
}

// Result represents a search result
type Result struct {
	Turn        models.Turn `json:"turn"`
	SessionID   string      `json:"session_id"`
	ProjectPath string      `json:"project_path"`
	Model       string      `json:"model,omitempty"`
	Source      string      `json:"source,omitempty"`
	Snippet     string      `json:"snippet"`

	// ParentSessionID is set when the hit is inside a subagent transcript.
	// Search is never filtered by the hidden-by-default rule that applies to
	// listings — the work a subagent did is most of what there is to find —
	// so renders use this to label a hit with the session that dispatched it.
	ParentSessionID string `json:"parent_session_id,omitempty"`

	// MatchedToolName names the tool whose stored input or result matched the
	// query, empty when the match was in the turn's own content.
	//
	// It is not decoration. A turn that issued a tool call summarises itself
	// as "[Tool: Bash]", so a hit found through the payload would otherwise
	// render a snippet with nothing of the query in it and no indication why
	// it was returned.
	MatchedToolName string `json:"matched_tool_name,omitempty"`
}

// Search executes a search query and returns results
func (s *Searcher) Search(q *Query, limit int) ([]Result, error) {
	if limit <= 0 {
		limit = 20
	}

	// Build the query
	query, args := s.buildQuery(q, limit)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("search query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var results []Result
	for rows.Next() {
		var r Result
		var content sql.NullString
		var parentSessionID sql.NullString
		var matchedTool, matchedPayload sql.NullString
		var rawTS any
		err := rows.Scan(
			&r.Turn.ID,
			&r.SessionID,
			&r.Turn.Type,
			&rawTS,
			&r.Turn.Ordinal,
			&content,
			&r.ProjectPath,
			&r.Model,
			&r.Source,
			&parentSessionID,
			&matchedTool,
			&matchedPayload,
		)
		if err != nil {
			return nil, fmt.Errorf("scan result: %w", err)
		}
		r.Turn.Timestamp = coerceTimestamp(rawTS)
		r.ParentSessionID = parentSessionID.String
		if content.Valid {
			r.Turn.Content = content.String
			r.Snippet = makeSnippet(content.String, q.Text, 150)
		}
		// Prefer the turn's own content when it actually holds the query, and
		// fall back to the payload that matched. The fallback is what makes a
		// payload hit legible: a turn that issued a tool call has
		// "[Tool: Bash]" for content, which says nothing about why it matched.
		//
		// matchedPayload is already centred on the match by fts5's snippet(),
		// so it only needs whitespace flattening, not re-snipping.
		if matchedPayload.Valid && !containsFold(content.String, q.Text) {
			r.MatchedToolName = matchedTool.String
			r.Snippet = flattenWhitespace(matchedPayload.String)
		}
		r.Turn.SessionID = r.SessionID
		results = append(results, r)
	}

	return results, rows.Err()
}

// containsFold reports whether haystack holds needle, ignoring ASCII case.
// Used only to decide which text a snippet is drawn from, never to decide
// whether a row matched — FTS5 has already answered that, and it tokenizes
// rather than substring-matching, so the two disagree on quoted phrases and
// prefix queries. Disagreeing in this direction is harmless: the payload
// snippet is the more informative of the two.
func containsFold(haystack, needle string) bool {
	if needle == "" {
		return false
	}
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(strings.Trim(needle, `"`)))
}

// flattenWhitespace collapses newlines and runs of spaces so a snippet taken
// from command output renders on one line, the same shaping makeSnippet applies
// to turn content.
func flattenWhitespace(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " ")), " ")
}

// buildQuery constructs the SQL query from parsed search
func (s *Searcher) buildQuery(q *Query, limit int) (string, []interface{}) {
	var conditions []string
	var args []interface{}
	argNum := 1

	// Text search spans two indexes: turns_fts over the conversation itself,
	// and tool_uses_fts over the stored tool inputs and results. A caller
	// looking for a command they ran or an error a tool printed is searching
	// for text that exists only in the second one, which is what issue #28 was
	// about — the turn that issued the call summarises itself as "[Tool: Bash]"
	// and holds none of it.
	//
	// Driven off a UNION of turn rowids rather than two LEFT JOINs with an OR
	// between them. Both indexes answer a MATCH with a small set, so joining
	// turns to that set is an index lookup; an OR across outer joins would
	// make the planner scan all 965,000 turns. UNION also collapses a turn
	// whose content and several of whose payloads all matched down to the one
	// row the caller asked for.
	//
	// It is not free. Measured on a copy of the author's 965,061-turn archive,
	// against the same binary minus this change: "git commit" 0.56s -> 1.67s,
	// "deploy" 0.38s -> 0.87s, a query matching nothing unchanged at 0.03s. A
	// UNION has to materialise both hit sets where the single-index form could
	// stream one. "the", which matches almost every turn, was 17.0s before and
	// 15.5s after — that query's cost is the sort, not the match. Searching
	// two indexes for roughly twice the time of searching one is the trade;
	// the absolute numbers stay inside a second for a query anyone would type.
	prefix := ""
	matchArg := 0

	if q.Text != "" {
		matchArg = argNum
		prefix = fmt.Sprintf(`text_hits AS (
			SELECT rowid AS turn_rowid FROM turns_fts WHERE turns_fts MATCH $%d
			UNION
			SELECT ht.rowid FROM tool_uses_fts
			JOIN tool_uses htu ON htu.id = tool_uses_fts.rowid
			JOIN turns ht ON ht.id = htu.turn_id
			WHERE tool_uses_fts MATCH $%d
		)`, matchArg, matchArg)
		// Escape the search text for FTS5 to handle special characters like hyphens
		args = append(args, escapeFTS5Query(q.Text))
		argNum++
	}

	// A query with no text has no payload to attribute a match to, and takes
	// the two trailing columns as literals so Search scans one row shape
	// either way. Selected here rather than by wrapping the finished query in
	// an outer SELECT: the wrap would put this query's ORDER BY inside a
	// subquery, and SQLite does not promise that order survives into the
	// enclosing SELECT. The ordering is load-bearing — see the comment on it
	// below.
	matchedToolCols := ""
	if q.Text == "" {
		matchedToolCols = ", NULL, NULL"
	}

	// Base query with joins
	baseQuery := `
		SELECT DISTINCT t.id, t.session_id, t.type, t.timestamp, t.ordinal, t.content,
			p.path as project_path, s.model, s.source, s.parent_session_id` + matchedToolCols + `
		FROM turns t
		JOIN sessions s ON t.session_id = s.id
		JOIN projects p ON s.project_id = p.id`

	if q.Text != "" {
		baseQuery += ` JOIN text_hits ON text_hits.turn_rowid = t.rowid`
	}

	// Tool filter requires join
	if q.Tool != "" {
		baseQuery += ` JOIN tool_uses tu ON t.session_id = tu.session_id`
		conditions = append(conditions, fmt.Sprintf("tu.tool_name = $%d COLLATE NOCASE", argNum))
		args = append(args, q.Tool)
		argNum++
	}

	// Project filter — user input flows into LIKE, so escape SQLite wildcards
	// (%, _, \) to prevent an agent passing "project:foo%bar" from matching
	// unrelated projects via silent wildcard expansion.
	if q.Project != "" {
		conditions = append(conditions, fmt.Sprintf("(p.path LIKE $%d ESCAPE '\\' OR p.display_name LIKE $%d ESCAPE '\\')", argNum, argNum+1))
		pattern := "%" + escapeLike(q.Project) + "%"
		args = append(args, pattern, pattern)
		argNum += 2
	}

	// Model filter — same escape discipline.
	if q.Model != "" {
		conditions = append(conditions, fmt.Sprintf("s.model LIKE $%d ESCAPE '\\'", argNum))
		args = append(args, "%"+escapeLike(q.Model)+"%")
		argNum++
	}

	// File filter (in tool_uses) — same escape discipline as project/model.
	if q.File != "" {
		if q.Tool == "" {
			// Need to add tool_uses join
			baseQuery += ` LEFT JOIN tool_uses tu2 ON t.session_id = tu2.session_id`
			conditions = append(conditions, fmt.Sprintf("tu2.file_path LIKE $%d ESCAPE '\\'", argNum))
		} else {
			conditions = append(conditions, fmt.Sprintf("tu.file_path LIKE $%d ESCAPE '\\'", argNum))
		}
		args = append(args, "%"+escapeLike(q.File)+"%")
		argNum++
	}

	// Date filters
	if !q.Before.IsZero() {
		conditions = append(conditions, fmt.Sprintf("t.timestamp < $%d", argNum))
		args = append(args, q.Before)
		argNum++
	}
	if !q.After.IsZero() {
		conditions = append(conditions, fmt.Sprintf("t.timestamp > $%d", argNum))
		args = append(args, q.After)
		argNum++
	}

	// has:error — sessions flagged during sync as containing tool errors
	if q.HasError {
		conditions = append(conditions, "s.has_error = 1")
	}

	// has:subagent — sessions flagged during sync as using Task tool
	if q.HasAgent {
		conditions = append(conditions, "s.has_subagent = 1")
	}

	// Source filter
	if q.Source != "" {
		conditions = append(conditions, fmt.Sprintf("s.source = $%d", argNum))
		args = append(args, q.Source)
		argNum++ //nolint:ineffassign // keep argNum consistent for future filters
	}

	// Build final query
	if len(conditions) > 0 {
		baseQuery += " WHERE " + strings.Join(conditions, " AND ")
	}

	// Results span sessions, so a turn's ordinal is not an ordering key here —
	// a position only means something within one session. `t.id` is a
	// tiebreaker, not a preference: turns sharing a timestamp would otherwise
	// change places between calls, so which ones fall inside LIMIT would vary
	// run to run on the same query against the same data.
	baseQuery += " ORDER BY t.timestamp DESC, t.id ASC"
	baseQuery += fmt.Sprintf(" LIMIT %d", limit)

	if q.Text == "" {
		return baseQuery, args
	}

	// The page is computed first and the payload lookup applied to it, not the
	// other way round.
	//
	// SQLite evaluates result-column subqueries while feeding rows to the
	// sorter, which is before LIMIT takes effect. Selecting these alongside
	// the page would run two correlated lookups for every candidate row a
	// common word matched — tens of thousands of them on a real archive — to
	// use twenty. Wrapping puts them after the LIMIT, so the cost is the page
	// size rather than the match size.
	//
	// fts5's own snippet() rather than picking a column and letting
	// makeSnippet centre it. Column -1 asks fts5 which of input_json and
	// result_content actually matched, which is a question the row cannot
	// answer for itself: a command lives in the input and an error message in
	// the result, so COALESCE-ing to one of them shows the wrong half about as
	// often as the right one.
	//
	// LIMIT 1 on each: a turn may have several matching calls, and the snippet
	// shows one of them.
	return fmt.Sprintf(`WITH %s, page AS (%s)
		SELECT page.*,
			(SELECT mtu.tool_name FROM tool_uses_fts
			 JOIN tool_uses mtu ON mtu.id = tool_uses_fts.rowid
			 WHERE tool_uses_fts MATCH $%d AND mtu.turn_id = page.id LIMIT 1),
			(SELECT snippet(tool_uses_fts, -1, '', '', '…', 24) FROM tool_uses_fts
			 JOIN tool_uses mtu ON mtu.id = tool_uses_fts.rowid
			 WHERE tool_uses_fts MATCH $%d AND mtu.turn_id = page.id LIMIT 1)
		FROM page
		ORDER BY page.timestamp DESC, page.id ASC`,
		prefix, baseQuery, matchArg, matchArg), args
}

// coerceTimestamp turns a driver value into a time.Time without failing the
// whole Search when one row holds an unparseable timestamp string (#85).
func coerceTimestamp(v any) time.Time {
	switch t := v.(type) {
	case nil:
		return time.Time{}
	case time.Time:
		return t
	case string:
		return parseStoredTimestamp(t)
	case []byte:
		return parseStoredTimestamp(string(t))
	default:
		return time.Time{}
	}
}

func parseStoredTimestamp(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	formats := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999 -0700 MST", // time.Time.String()
		"2006-01-02 15:04:05.999999999 -0700",
		"2006-01-02 15:04:05",
		"2006-01-02",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// makeSnippet creates a snippet from content with the search term highlighted
func makeSnippet(content, searchTerm string, maxLen int) string {
	if content == "" {
		return ""
	}

	// Find the search term (case-insensitive)
	lower := strings.ToLower(content)
	term := strings.ToLower(searchTerm)

	var start int
	if term != "" {
		idx := strings.Index(lower, term)
		if idx > 0 {
			// Start a bit before the match
			start = idx - 50
			if start < 0 {
				start = 0
			}
		}
	}

	// Extract snippet
	end := start + maxLen
	if end > len(content) {
		end = len(content)
	}

	snippet := content[start:end]

	// Add ellipsis if truncated
	if start > 0 {
		snippet = "..." + snippet
	}
	if end < len(content) {
		snippet = snippet + "..."
	}

	// Clean up whitespace
	snippet = strings.ReplaceAll(snippet, "\n", " ")
	snippet = strings.Join(strings.Fields(snippet), " ")

	return snippet
}

// escapeFTS5Query escapes a search query for FTS5.
// FTS5 treats certain characters as operators (AND, OR, NOT, -, etc).
// This function quotes terms containing special characters to search them literally.
func escapeFTS5Query(query string) string {
	// Check if query is already quoted
	if strings.HasPrefix(query, `"`) && strings.HasSuffix(query, `"`) {
		return query
	}

	// FTS5 special characters that need escaping
	// Hyphens are interpreted as NOT operator
	// Other special chars: AND, OR, NOT, NEAR, parentheses, asterisk, caret
	specialCharsRe := regexp.MustCompile(`[-*^()]`)

	// Split into words and quote any that contain special characters
	words := strings.Fields(query)
	var result []string

	for _, word := range words {
		// Skip if already quoted
		if strings.HasPrefix(word, `"`) && strings.HasSuffix(word, `"`) {
			result = append(result, word)
			continue
		}

		// If word contains special FTS5 characters, quote it
		if specialCharsRe.MatchString(word) {
			// Escape any internal double quotes
			escaped := strings.ReplaceAll(word, `"`, `""`)
			result = append(result, `"`+escaped+`"`)
		} else {
			result = append(result, word)
		}
	}

	return strings.Join(result, " ")
}

// escapeLike escapes SQLite LIKE metacharacters (%, _, and the backslash
// escape itself) so agent-supplied filter fragments can't smuggle
// wildcards into the SQL — a project filter like "foo%bar" from an
// untrusted agent should match the literal string "foo%bar", not any
// project whose name contains "foo" followed by "bar" with anything
// between. Use with `LIKE ? ESCAPE '\\'` in the query.
func escapeLike(s string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`,
		`%`, `\%`,
		`_`, `\_`,
	)
	return replacer.Replace(s)
}
