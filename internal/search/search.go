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

	// The terms that bound the index scan to the window the caller asked for.
	// Resolved before the statement is built because a one-sided filter takes
	// its other side from the archive's own date range.
	periods, err := s.periodTermsFor(q)
	if err != nil {
		return nil, err
	}

	// Build the query
	query, args := s.buildQuery(q, limit, periods)

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

// buildQuery constructs the SQL query from parsed search.
//
// periods are the FTS5 terms that bound the text match to the window the date
// filters ask for, empty for a search that has no date filter or whose range
// is too wide to be worth compiling. They narrow the scan; they never decide
// an answer — the timestamp predicate below does that, and it stays whether or
// not the terms are there.
func (s *Searcher) buildQuery(q *Query, limit int, periods []string) (string, []interface{}) {
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
	//
	// Both branches carry the period terms when there is a date filter, which
	// is what keeps that materialisation proportional to the window rather
	// than to the archive (issue #80). Both, not just the payload index that
	// showed up in the measurements: the UNION is only as bounded as its
	// looser branch.
	prefix := ""
	snippetMatchArg := 0
	escaped := ""

	if q.Text != "" {
		// Escape the search text for FTS5 to handle special characters like hyphens
		escaped = escapeFTS5Query(q.Text)
		turnsMatchArg := argNum
		argNum++
		payloadMatchArg := argNum
		argNum++
		// The payload branch joins back to turns on the turn's full identity,
		// (session_id, id), not on its uuid alone. A resumed transcript copies
		// the earlier session's lines verbatim, uuids included, so since
		// migration 012 one uuid can name a turn in each of two sessions. On
		// the uuid alone this join yields both copies for a call that belongs
		// to one of them, and the UNION cannot collapse that away — the two
		// rowids are genuinely different rows — so a search for a command only
		// one session ran came back with the other session's turn as well, a
		// hit whose session does not contain the query anywhere.
		//
		// Both halves of the pair are index-leading — turns' primary key is
		// (session_id, id) — so the join is the same seek it was.
		prefix = fmt.Sprintf(`text_hits AS (
			SELECT rowid AS turn_rowid FROM turns_fts WHERE turns_fts MATCH $%d
			UNION
			SELECT ht.rowid FROM tool_uses_fts
			JOIN tool_uses htu ON htu.id = tool_uses_fts.rowid
			JOIN turns ht ON ht.id = htu.turn_id AND ht.session_id = htu.session_id
			WHERE tool_uses_fts MATCH $%d
		)`, turnsMatchArg, payloadMatchArg)
		args = append(args,
			ftsMatchExpr(turnsFTSColumns, escaped, periods),
			ftsMatchExpr(payloadFTSColumns, escaped, periods))
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

	// Base query with joins.
	//
	// LEFT JOIN on projects, not an inner one (#65). sessions.project_id is
	// nullable and db.MergeFrom deliberately writes a NULL for a session whose
	// project row is missing from the incoming archive, on the reasoning that
	// the turns are the part worth keeping. Under an inner join such a session
	// was not ranked lower or shown without a project name — it matched nothing,
	// ever, while still listing and exporting correctly. A row you can only
	// reach by an id you already know is close to unreachable in an archive of
	// 46,000 sessions, and finding things is what the archive is for.
	//
	// The outer join does not widen any filter: project: compares p.path with
	// LIKE, and NULL fails LIKE, so a projectless session stays out of a
	// project-filtered search. TestSearch_ProjectFilterStillExcludesProjectless-
	// Sessions holds that.
	//
	// It costs nothing measurable, which was worth checking: search was
	// optimised twice recently (#28's UNION, #80's period terms) and a join
	// change is exactly the kind of thing that quietly undoes that.
	//
	// Measured against the author's 46,061-session / 1,014,942-turn archive
	// opened read-only, both join shapes built from this same function and run
	// alternately in one process — 13 pairs per query, order swapped every
	// pair so neither shape sits behind the other's warm cache, best of each.
	// INNER -> LEFT: "git commit" 155ms -> 156ms (+0.7%), "deploy" 168 -> 172
	// (+2.1%), "error" 799 -> 812 (+1.6%), "after:2026-09-01 refactor" 11 -> 11
	// (+0.3%), "the" (a word in almost every turn) 20.0s -> 19.6s (-2.2%). Every
	// delta is inside the run-to-run noise, and the same rows come back.
	//
	// The reason it is free: the join is a seek on the projects primary key for
	// one row per candidate, and projects is four orders of magnitude smaller
	// than turns, so it was never the selective end of the plan. Dropping the
	// inner join's implicit "project_id must resolve" predicate pruned nothing,
	// because the archive has 46,061 sessions and 0 of them fail it today —
	// which is the point: the rows this makes visible are ones MergeFrom has
	// yet to write, and search should not be the one surface that loses them.
	//
	// COALESCE on p.path for the same reason GetSessionsPage has one: the
	// column is NULL for these rows and project_path is scanned into a plain
	// string, so the join fix without it would turn #65 into #64. Same for
	// s.model, which is nullable in the schema with no default.
	baseQuery := `
		SELECT DISTINCT t.id, t.session_id, t.type, t.timestamp, t.ordinal, t.content,
			COALESCE(p.path, '') as project_path, COALESCE(s.model, '') as model,
			s.source, s.parent_session_id` + matchedToolCols + `
		FROM turns t
		JOIN sessions s ON t.session_id = s.id
		LEFT JOIN projects p ON s.project_id = p.id`

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

	// has:toolerror — this turn issued a call whose result reported a failure
	// (#83). Per call, which is the granularity the flag exists for:
	// s.has_error above returns every turn of a session that broke something
	// somewhere, and a reader asking what broke wants the turn it broke in.
	//
	// An EXISTS rather than a join, for two reasons. A join on turn_id would
	// multiply a turn that made several failing calls into several result rows
	// — the SELECT DISTINCT above would collapse them again, but only after
	// sorting them — and a correlated EXISTS composes with the tool: and file:
	// joins already attached to this query without any of them having to know
	// about the others.
	//
	// Seeks idx_tool_uses_is_error, which is partial on is_error = 1 and
	// carries turn_id, so the predicate is answered out of 10,909 index
	// entries rather than by looking at 279,383 rows.
	if q.HasToolError {
		conditions = append(conditions,
			"EXISTS (SELECT 1 FROM tool_uses etu WHERE etu.turn_id = t.id AND etu.is_error = 1)")
	}

	// Source filter
	if q.Source != "" {
		conditions = append(conditions, fmt.Sprintf("s.source = $%d", argNum))
		args = append(args, q.Source)
		argNum++
	}

	// The expression the match-attribution subqueries use, bound last because
	// it is read after every filter above has claimed its parameter.
	//
	// The same text as the payload branch of text_hits, minus the period
	// terms. Those subqueries look up one turn's own tool uses (see the shape
	// below), so there is nothing left for a period term to prune — and
	// leaving them out keeps snippet()'s automatic column choice looking at
	// only the two columns that hold payload text, rather than at a third
	// column whose single token would be competing with them for the best
	// match.
	if q.Text != "" {
		snippetMatchArg = argNum
		args = append(args, ftsMatchExpr(payloadFTSColumns, escaped, nil))
		argNum++ //nolint:ineffassign // keep argNum consistent for future parameters
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
	//
	// Each subquery starts from the turn's tool uses and probes the index for
	// one document, rather than starting from the index's match set and
	// filtering it down to this turn. Both answer identically; the second costs
	// a walk of every all-time match looking for a row that belongs to this
	// turn, which measured 1.08s of a 1.19s date-filtered search on the
	// author's archive — and does not shrink when the window does, because it
	// is per returned row rather than per match.
	//
	// CROSS JOIN is load-bearing, not decoration. It is how SQLite is told not
	// to reorder the join, and left to choose it puts the virtual table first:
	// it has no row estimate for an fts5 MATCH that would tell it a seek on
	// the index on tool_uses.turn_id is the cheaper end to start from.
	// (That index is idx_tool_uses_turn_ordinal since migration 011, which
	// keys the table on (turn_id, session_id, turn_ordinal) and leads with
	// turn_id. Migration 012 added session_id to it, because a turn's identity
	// is (session_id, id) and one uuid can name a turn in each of two sessions
	// — which is also why both subqueries below match on the session as well,
	// so a turn is attributed its own session's call rather than the other
	// copy's. Both columns are index-leading, so the seek narrows.)
	//
	// Which makes that index a hard dependency of this shape rather than an
	// optimisation of it. Pinning the order without an index to seek turns the
	// lookup into a scan of all 264,199 tool uses per returned row — measured
	// at 3.0s against the 1.2s the unpinned shape costs. Migration 010 creates
	// the index and TestSearch_PayloadAttributionDrivesFromTheTurn asserts the
	// plan still uses it, because a wrong plan here is invisible in the rows.
	return fmt.Sprintf(`WITH %s, page AS (%s)
		SELECT page.*,
			(SELECT mtu.tool_name FROM tool_uses mtu
			 CROSS JOIN tool_uses_fts ON tool_uses_fts.rowid = mtu.id
			 WHERE mtu.turn_id = page.id AND mtu.session_id = page.session_id
			   AND tool_uses_fts MATCH $%d LIMIT 1),
			(SELECT snippet(tool_uses_fts, -1, '', '', '…', 24) FROM tool_uses mtu
			 CROSS JOIN tool_uses_fts ON tool_uses_fts.rowid = mtu.id
			 WHERE mtu.turn_id = page.id AND mtu.session_id = page.session_id
			   AND tool_uses_fts MATCH $%d LIMIT 1)
		FROM page
		ORDER BY page.timestamp DESC, page.id ASC`,
		prefix, baseQuery, snippetMatchArg, snippetMatchArg), args
}

// FTS5 column names as the query builder sees them, one list per index. The
// caller's text is scoped to these so that a search for a period token finds
// the rows whose text holds it rather than the rows that belong to that
// period — see the note on the token namespace in period.go.
var (
	turnsFTSColumns   = []string{"content"}
	payloadFTSColumns = []string{"input_json", "result_content"}
)

// The oldest and newest calendar date the archive holds, as two separate
// single-aggregate queries. SQLite answers each with a seek on
// idx_turns_timestamp; one query selecting both aggregates would scan the
// table instead.
//
// substr in SQL rather than in Go so the value arrives as the text SQLite
// compares, not as a time.Time the driver has reinterpreted. The date filter
// is a lexicographic comparison of that text and the period token is sliced
// out of it, so a bound taken anywhere else could name a different day than
// the comparison uses.
const (
	oldestTurnDateQuery = `SELECT substr(MIN(timestamp), 1, 10) FROM turns`
	newestTurnDateQuery = `SELECT substr(MAX(timestamp), 1, 10) FROM turns`
)

// periodTermsFor resolves a query's date filters into the period terms that
// bound its text match, or nil for a query that cannot or should not be pruned.
//
// A one-sided filter takes its other side from the archive itself. after: with
// no before: is the common case and has no upper bound of its own; using "now"
// for it would drop any row stamped in the future, and a clock-skewed
// transcript is a thing that happens. The newest row in the archive is both an
// honest bound and a cheap one.
func (s *Searcher) periodTermsFor(q *Query) ([]string, error) {
	if q.Text == "" || (q.After.IsZero() && q.Before.IsZero()) {
		return nil, nil
	}

	from := periodFilterDate(q.After)
	if from == "" {
		date, err := s.turnDate(oldestTurnDateQuery)
		if err != nil {
			return nil, err
		}
		from = date
	}

	to := periodFilterDate(q.Before)
	if to == "" {
		date, err := s.turnDate(newestTurnDateQuery)
		if err != nil {
			return nil, err
		}
		to = date
	}

	return periodTerms(from, to), nil
}

// turnDate runs one of the bound queries above, returning "" for an archive
// with no turns in it.
func (s *Searcher) turnDate(query string) (string, error) {
	var date sql.NullString
	if err := s.db.QueryRow(query).Scan(&date); err != nil {
		return "", fmt.Errorf("read the archive's date range: %w", err)
	}
	return date.String, nil
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
