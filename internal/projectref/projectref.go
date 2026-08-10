// ABOUTME: Canonical rendering + matching helpers for projects across CLI/TUI/MCP.
// ABOUTME: Consumers must go through here rather than reading Project.DisplayName directly.

// Package projectref is the single source of truth for how projects are
// referenced across the four surface classes ccvault ships. Every surface
// that displays or matches on projects should go through the function here
// that fits its class, rather than reading models.Project.DisplayName or
// filepath.Base(path) inline. An AST allowlist test in test/integration
// enforces this — the only files allowed to touch DisplayName directly are
// this package, adapter code that writes it, and the persistence layer.
//
// Four surface classes and the function each uses:
//
//	Class A — tabular UI (multiple projects with columns): CLI list-projects,
//	TUI Projects/Sessions/Dashboard/Analytics. Uses Label for the short
//	column and reads Path separately for the PATH column.
//
//	Class B — inline UI (one line per row of a larger list): CLI search
//	results, CLI orient text bullets, TUI Search results, TUI Sessions
//	title when scoped. Uses Inline for a combined "name (~/path)" form.
//
//	Class C — structured output for agents (JSON / MCP responses): CLI
//	orient --json, MCP list_projects, MCP session-carrying responses.
//	Uses Ref to emit {name, path} objects — never just name.
//
//	Class D — input matching (filter strings resolving to projects): MCP
//	list_sessions {"project": ...}, MCP promptAnalyzeProject. Uses
//	ResolveAll to return every match rather than silently picking one.
//
// See docs and internal/projectref_test.go for the doctrine behind this.
package projectref

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/2389-research/ccvault/pkg/models"
)

// Label returns the short human-friendly label for a project, suitable
// for Class A (tabular) surfaces. Prefers the adapter-provided
// DisplayName; falls back to filepath.Base(Path) so a project row with
// an empty DisplayName still renders something sensible.
func Label(p *models.Project) string {
	if p == nil {
		return ""
	}
	if p.DisplayName != "" {
		return p.DisplayName
	}
	if p.Path == "" {
		return ""
	}
	return filepath.Base(p.Path)
}

// Inline returns the combined "name (~/short/path)" form for Class B
// surfaces where a project identity is embedded in a one-line row. The
// path is tilde-substituted when it lives under the user's HOME (with a
// separator boundary check so /Users/dyl doesn't rewrite /Users/dylan/x).
// When path is empty, only the label is returned. When both are empty,
// returns "".
func Inline(p *models.Project) string {
	if p == nil {
		return ""
	}
	name := Label(p)
	if p.Path == "" {
		return name
	}
	path := tildeAbbreviate(p.Path)
	if name == "" || name == filepath.Base(p.Path) {
		// Degenerate case: label is just the basename, so "name (path)"
		// would repeat the basename. Show the path alone, which already
		// implies the basename.
		return path
	}
	return fmt.Sprintf("%s (%s)", name, path)
}

// Ref returns the structured {name, path} object every Class C surface
// (JSON / MCP responses) must emit. Both fields are always present, even
// if empty, so agent consumers never need to nil-check individual fields.
func Ref(p *models.Project) map[string]any {
	if p == nil {
		return map[string]any{"name": "", "path": ""}
	}
	return map[string]any{
		"name": Label(p),
		"path": p.Path,
	}
}

// RefsFromValues is the []Project convenience for Ref. Adapts callers
// that iterate the value-slice returned by db.GetProjects without
// requiring them to build a []*Project first.
func RefsFromValues(projects []models.Project) []map[string]any {
	out := make([]map[string]any, 0, len(projects))
	for i := range projects {
		out = append(out, Ref(&projects[i]))
	}
	return out
}

// ResolveAll returns every project whose Path or DisplayName contains the
// (case-insensitive) filter substring. It never silently picks a single
// match on ambiguity — that's the whole point of Class D. An empty filter
// matches nothing (callers should special-case "no filter" upstream so
// this function's contract stays crisp).
//
// Order preserved from the input slice.
func ResolveAll(projects []models.Project, filter string) []models.Project {
	if filter == "" {
		return nil
	}
	needle := strings.ToLower(filter)
	var out []models.Project
	for _, p := range projects {
		if strings.Contains(strings.ToLower(p.Path), needle) ||
			strings.Contains(strings.ToLower(p.DisplayName), needle) {
			out = append(out, p)
		}
	}
	return out
}

// tildeAbbreviate rewrites paths under $HOME as "~/rest". The separator
// boundary check prevents /Users/dyl from rewriting /Users/dylan/x into
// "~an/x" — that's the bug fresh-eyes review caught in PR #22.
func tildeAbbreviate(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == home {
		return "~"
	}
	if strings.HasPrefix(path, home+string(os.PathSeparator)) {
		return "~" + path[len(home):]
	}
	return path
}
