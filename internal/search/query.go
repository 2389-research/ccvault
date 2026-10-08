// ABOUTME: Query parser for ccvault search syntax
// ABOUTME: Parses Gmail-like search operators into structured queries

package search

import (
	"regexp"
	"strings"
	"time"
)

// Query represents a parsed search query
type Query struct {
	Text     string    // Free-text search terms
	Project  string    // project: filter
	Model    string    // model: filter
	Tool     string    // tool: filter
	File     string    // file: filter
	Before   time.Time // before: filter (upper bound, exclusive; a bare date is local midnight of that day)
	After    time.Time // after: filter (lower bound, exclusive; a bare date is local midnight of that day)
	HasError bool      // has:error filter — per session
	HasAgent bool      // has:subagent filter
	Source   string    // source: filter

	// HasToolError is the has:toolerror filter: this turn issued a call whose
	// result reported a failure (#83).
	//
	// A separate filter from HasError rather than a refinement of it, because
	// they answer different questions at different granularities — "this
	// conversation held a failure" and "this call failed" — and giving has:error
	// the second meaning would silently change what every existing query
	// returns.
	HasToolError bool
}

// Parse parses a search query string into a Query struct
func Parse(input string) *Query {
	q := &Query{}

	// Extract operators using regex
	operatorRe := regexp.MustCompile(`(\w+):("[^"]*"|[^\s]+)`)
	matches := operatorRe.FindAllStringSubmatch(input, -1)

	for _, match := range matches {
		operator := strings.ToLower(match[1])
		value := strings.Trim(match[2], `"`)

		switch operator {
		case "project":
			q.Project = value
		case "model":
			q.Model = value
		case "tool":
			q.Tool = value
		case "file":
			q.File = value
		case "before":
			q.Before = parseDate(value)
		case "after":
			q.After = parseDate(value)
		case "source":
			q.Source = value
		case "has":
			switch strings.ToLower(value) {
			case "error":
				q.HasError = true
			case "subagent", "agent":
				q.HasAgent = true
			// Three spellings of one filter. An operator value this parser
			// does not recognise is dropped silently, so a near miss returns
			// an unfiltered result set that looks like an answer — and
			// "toolerror", "tool-error" and "tool_error" are all plausible
			// first guesses at a name nobody has seen before.
			case "toolerror", "tool-error", "tool_error":
				q.HasToolError = true
			}
		}
	}

	// Remove operators from input to get free text
	text := operatorRe.ReplaceAllString(input, "")
	q.Text = strings.TrimSpace(text)

	return q
}

// parseDate parses a date string as the caller's local midnight for that day.
// Relative tokens (today, yesterday, …) use the local calendar, not UTC Truncate.
func parseDate(s string) time.Time {
	loc := time.Local
	formats := []string{
		"2006-01-02",
		"2006/01/02",
		"01/02/2006",
		"Jan 2, 2006",
		"January 2, 2006",
	}

	for _, format := range formats {
		if t, err := time.ParseInLocation(format, s, loc); err == nil {
			return t
		}
	}

	now := time.Now().In(loc)
	y, m, d := now.Date()
	today := time.Date(y, m, d, 0, 0, 0, 0, loc)

	switch strings.ToLower(s) {
	case "today":
		return today
	case "yesterday":
		return today.AddDate(0, 0, -1)
	case "week", "thisweek":
		return today.AddDate(0, 0, -7)
	case "month", "thismonth":
		return today.AddDate(0, 0, -30)
	}

	return time.Time{}
}

// IsEmpty returns true if the query has no filters
func (q *Query) IsEmpty() bool {
	return q.Text == "" &&
		q.Project == "" &&
		q.Model == "" &&
		q.Tool == "" &&
		q.File == "" &&
		q.Before.IsZero() &&
		q.After.IsZero() &&
		!q.HasError &&
		!q.HasAgent &&
		!q.HasToolError &&
		q.Source == ""
}

// HasFilters returns true if the query has any non-text filters
func (q *Query) HasFilters() bool {
	return q.Project != "" ||
		q.Model != "" ||
		q.Tool != "" ||
		q.File != "" ||
		!q.Before.IsZero() ||
		!q.After.IsZero() ||
		q.HasError ||
		q.HasAgent ||
		q.HasToolError ||
		q.Source != ""
}
