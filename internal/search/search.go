// ABOUTME: Search execution for ccvault
// ABOUTME: Executes parsed queries against the database with FTS5

package search

import (
	"database/sql"
	"fmt"
	"regexp"
	"strings"

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
		err := rows.Scan(
			&r.Turn.ID,
			&r.SessionID,
			&r.Turn.Type,
			&r.Turn.Timestamp,
			&content,
			&r.ProjectPath,
			&r.Model,
			&r.Source,
		)
		if err != nil {
			return nil, fmt.Errorf("scan result: %w", err)
		}
		if content.Valid {
			r.Turn.Content = content.String
			r.Snippet = makeSnippet(content.String, q.Text, 150)
		}
		r.Turn.SessionID = r.SessionID
		results = append(results, r)
	}

	return results, rows.Err()
}

// buildQuery constructs the SQL query from parsed search
func (s *Searcher) buildQuery(q *Query, limit int) (string, []interface{}) {
	var conditions []string
	var args []interface{}
	argNum := 1

	// Base query with joins
	baseQuery := `
		SELECT DISTINCT t.id, t.session_id, t.type, t.timestamp, t.content,
			p.path as project_path, s.model, s.source
		FROM turns t
		JOIN sessions s ON t.session_id = s.id
		JOIN projects p ON s.project_id = p.id`

	// FTS join if text search
	if q.Text != "" {
		baseQuery += ` JOIN turns_fts fts ON t.rowid = fts.rowid`
		conditions = append(conditions, fmt.Sprintf("turns_fts MATCH $%d", argNum))
		// Escape the search text for FTS5 to handle special characters like hyphens
		args = append(args, escapeFTS5Query(q.Text))
		argNum++
	}

	// Tool filter requires join
	if q.Tool != "" {
		baseQuery += ` JOIN tool_uses tu ON t.session_id = tu.session_id`
		conditions = append(conditions, fmt.Sprintf("tu.tool_name = $%d COLLATE NOCASE", argNum))
		args = append(args, q.Tool)
		argNum++
	}

	// Project filter
	if q.Project != "" {
		conditions = append(conditions, fmt.Sprintf("(p.path LIKE $%d OR p.display_name LIKE $%d)", argNum, argNum+1))
		pattern := "%" + q.Project + "%"
		args = append(args, pattern, pattern)
		argNum += 2
	}

	// Model filter
	if q.Model != "" {
		conditions = append(conditions, fmt.Sprintf("s.model LIKE $%d", argNum))
		args = append(args, "%"+q.Model+"%")
		argNum++
	}

	// File filter (in tool_uses)
	if q.File != "" {
		if q.Tool == "" {
			// Need to add tool_uses join
			baseQuery += ` LEFT JOIN tool_uses tu2 ON t.session_id = tu2.session_id`
			conditions = append(conditions, fmt.Sprintf("tu2.file_path LIKE $%d", argNum))
		} else {
			conditions = append(conditions, fmt.Sprintf("tu.file_path LIKE $%d", argNum))
		}
		args = append(args, "%"+q.File+"%")
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

	baseQuery += " ORDER BY t.timestamp DESC"
	baseQuery += fmt.Sprintf(" LIMIT %d", limit)

	return baseQuery, args
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
