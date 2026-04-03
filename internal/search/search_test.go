// ABOUTME: Tests for the search package
// ABOUTME: Verifies query parsing and snippet generation

package search

import (
	"testing"
	"time"
)

func TestParse_SimpleText(t *testing.T) {
	q := Parse("hello world")

	if q.Text != "hello world" {
		t.Errorf("Expected text 'hello world', got '%s'", q.Text)
	}
	if q.Project != "" {
		t.Errorf("Expected empty project, got '%s'", q.Project)
	}
}

func TestParse_ProjectFilter(t *testing.T) {
	q := Parse("project:myproject some text")

	if q.Project != "myproject" {
		t.Errorf("Expected project 'myproject', got '%s'", q.Project)
	}
	if q.Text != "some text" {
		t.Errorf("Expected text 'some text', got '%s'", q.Text)
	}
}

func TestParse_QuotedProject(t *testing.T) {
	q := Parse(`project:"my project" search term`)

	if q.Project != "my project" {
		t.Errorf("Expected project 'my project', got '%s'", q.Project)
	}
	if q.Text != "search term" {
		t.Errorf("Expected text 'search term', got '%s'", q.Text)
	}
}

func TestParse_ModelFilter(t *testing.T) {
	q := Parse("model:opus find something")

	if q.Model != "opus" {
		t.Errorf("Expected model 'opus', got '%s'", q.Model)
	}
	if q.Text != "find something" {
		t.Errorf("Expected text 'find something', got '%s'", q.Text)
	}
}

func TestParse_ToolFilter(t *testing.T) {
	q := Parse("tool:Bash")

	if q.Tool != "Bash" {
		t.Errorf("Expected tool 'Bash', got '%s'", q.Tool)
	}
}

func TestParse_FileFilter(t *testing.T) {
	q := Parse("file:main.go")

	if q.File != "main.go" {
		t.Errorf("Expected file 'main.go', got '%s'", q.File)
	}
}

func TestParse_DateFilters(t *testing.T) {
	q := Parse("after:2024-01-15 before:2024-02-01")

	if q.After.IsZero() {
		t.Error("After date should not be zero")
	}
	if q.Before.IsZero() {
		t.Error("Before date should not be zero")
	}

	expectedAfter := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	if !q.After.Equal(expectedAfter) {
		t.Errorf("Expected after date %v, got %v", expectedAfter, q.After)
	}

	expectedBefore := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)
	if !q.Before.Equal(expectedBefore) {
		t.Errorf("Expected before date %v, got %v", expectedBefore, q.Before)
	}
}

func TestParse_HasError(t *testing.T) {
	q := Parse("has:error")

	if !q.HasError {
		t.Error("Expected HasError to be true")
	}
}

func TestParse_HasAgent(t *testing.T) {
	tests := []string{"has:subagent", "has:agent"}
	for _, input := range tests {
		q := Parse(input)
		if !q.HasAgent {
			t.Errorf("Expected HasAgent to be true for '%s'", input)
		}
	}
}

func TestParse_CombinedFilters(t *testing.T) {
	q := Parse("project:myapp model:opus tool:Edit search terms")

	if q.Project != "myapp" {
		t.Errorf("Expected project 'myapp', got '%s'", q.Project)
	}
	if q.Model != "opus" {
		t.Errorf("Expected model 'opus', got '%s'", q.Model)
	}
	if q.Tool != "Edit" {
		t.Errorf("Expected tool 'Edit', got '%s'", q.Tool)
	}
	if q.Text != "search terms" {
		t.Errorf("Expected text 'search terms', got '%s'", q.Text)
	}
}

func TestQuery_IsEmpty(t *testing.T) {
	tests := []struct {
		query    *Query
		expected bool
	}{
		{&Query{}, true},
		{&Query{Text: "hello"}, false},
		{&Query{Project: "myproject"}, false},
		{&Query{Model: "opus"}, false},
		{&Query{Tool: "Bash"}, false},
		{&Query{HasError: true}, false},
		{&Query{Source: "codex"}, false},
	}

	for _, tt := range tests {
		result := tt.query.IsEmpty()
		if result != tt.expected {
			t.Errorf("IsEmpty() for %+v = %v, expected %v", tt.query, result, tt.expected)
		}
	}
}

func TestQuery_HasFilters(t *testing.T) {
	tests := []struct {
		query    *Query
		expected bool
	}{
		{&Query{}, false},
		{&Query{Text: "hello"}, false},
		{&Query{Project: "myproject"}, true},
		{&Query{Model: "opus"}, true},
		{&Query{Tool: "Bash"}, true},
		{&Query{HasError: true}, true},
		{&Query{Text: "hello", Project: "myproject"}, true},
		{&Query{Source: "codex"}, true},
	}

	for _, tt := range tests {
		result := tt.query.HasFilters()
		if result != tt.expected {
			t.Errorf("HasFilters() for %+v = %v, expected %v", tt.query, result, tt.expected)
		}
	}
}

func TestMakeSnippet_Basic(t *testing.T) {
	content := "This is a test content with some searchable text in it."
	snippet := makeSnippet(content, "searchable", 50)

	if snippet == "" {
		t.Error("Snippet should not be empty")
	}
	if len(snippet) > 60 { // 50 + some buffer for ellipsis
		t.Errorf("Snippet too long: %d chars", len(snippet))
	}
}

func TestMakeSnippet_EmptyContent(t *testing.T) {
	snippet := makeSnippet("", "test", 50)
	if snippet != "" {
		t.Errorf("Expected empty snippet for empty content, got '%s'", snippet)
	}
}

func TestMakeSnippet_NoSearchTerm(t *testing.T) {
	content := "Some content here"
	snippet := makeSnippet(content, "", 50)

	if snippet == "" {
		t.Error("Snippet should not be empty")
	}
}

func TestMakeSnippet_SearchTermNotFound(t *testing.T) {
	content := "Some content here"
	snippet := makeSnippet(content, "nonexistent", 50)

	// Should still return content from the beginning
	if snippet == "" {
		t.Error("Snippet should not be empty")
	}
}

func TestMakeSnippet_WhitespaceNormalization(t *testing.T) {
	content := "Line one\nLine two\nLine three"
	snippet := makeSnippet(content, "", 100)

	// Should replace newlines with spaces
	if snippet != "Line one Line two Line three" {
		t.Errorf("Expected whitespace to be normalized, got '%s'", snippet)
	}
}

func TestParseDate_Formats(t *testing.T) {
	tests := []struct {
		input    string
		expected time.Time
	}{
		{"2024-01-15", time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)},
		{"2024/01/15", time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)},
	}

	for _, tt := range tests {
		result := parseDate(tt.input)
		if !result.Equal(tt.expected) {
			t.Errorf("parseDate(%s) = %v, expected %v", tt.input, result, tt.expected)
		}
	}
}

func TestParseDate_Invalid(t *testing.T) {
	result := parseDate("not-a-date")
	if !result.IsZero() {
		t.Errorf("Expected zero time for invalid date, got %v", result)
	}
}

func TestParse_SourceFilter(t *testing.T) {
	q := Parse("source:codex auth middleware")

	if q.Source != "codex" {
		t.Errorf("Expected source 'codex', got '%s'", q.Source)
	}
	if q.Text != "auth middleware" {
		t.Errorf("Expected text 'auth middleware', got '%s'", q.Text)
	}
}

func TestParse_SourceFilterQuoted(t *testing.T) {
	q := Parse(`source:"claude-code" auth`)

	if q.Source != "claude-code" {
		t.Errorf("Expected source 'claude-code', got '%s'", q.Source)
	}
	if q.Text != "auth" {
		t.Errorf("Expected text 'auth', got '%s'", q.Text)
	}
}

func TestEscapeFTS5Query_HyphenatedTerms(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"tree-sitter", `"tree-sitter"`},
		{"simple", "simple"},
		{"hello world", "hello world"},
		{"vue-router react-dom", `"vue-router" "react-dom"`},
		{`"already quoted"`, `"already quoted"`},
		{"asterisk*", `"asterisk*"`},
		{"(parens)", `"(parens)"`},
		{"mixed-term normal", `"mixed-term" normal`},
	}

	for _, tt := range tests {
		result := escapeFTS5Query(tt.input)
		if result != tt.expected {
			t.Errorf("escapeFTS5Query(%q) = %q, expected %q", tt.input, result, tt.expected)
		}
	}
}

func TestEscapeFTS5Query_InternalQuotes(t *testing.T) {
	// Test that internal quotes are escaped
	result := escapeFTS5Query(`say-"hello"`)
	// The hyphen triggers quoting, and internal quotes should be escaped
	if result != `"say-""hello"""` {
		t.Errorf("escapeFTS5Query with internal quotes = %q, expected %q", result, `"say-""hello"""`)
	}
}
