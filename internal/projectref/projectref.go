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
//	TUI Projects/Sessions/Dashboard/Analytics/Search. Uses Label (or
//	LabelFromPath when the caller has a session's ProjectPath but not a
//	full Project) for the short column and reads Path separately for the
//	PATH column. TUI Search sits in Class A because its per-result cell
//	is compact and there's a separate row context (timestamp/session ID)
//	that disambiguates identical labels.
//
//	Class B — inline UI (one line per row of a larger list): CLI search
//	results, CLI orient text bullets, TUI Sessions title when scoped.
//	Uses Inline for a combined "name (~/path)" form.
//
//	Class C — structured output for agents (JSON / MCP responses): CLI
//	orient --json, CLI list-projects --json, CLI list-sessions --json,
//	MCP list_projects, MCP list_sessions, MCP session-carrying responses.
//	Uses Ref/EnrichedRef/SessionRef so agents get {name, path} at minimum
//	alongside the operational fields (id, counts, timestamps).
//
//	Class D — input matching (filter strings resolving to projects): MCP
//	list_sessions {"project": ...}, MCP promptAnalyzeProject, CLI
//	list-sessions --project. Uses ResolveAll to return every match rather
//	than silently picking one.
//
// See docs and internal/projectref_test.go for the doctrine behind this.
package projectref

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/2389-research/ccvault/internal/compact"
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
	path := compact.Tilde(p.Path)
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

// EnrichedRef returns the Ref-shaped {name, path} contract PLUS the
// operational Project fields (id, timestamps, counts, source, and the
// raw display_name for callers that still key on it). Class C
// consumers that need to surface project-level analytics (MCP
// list_projects, CLI list-projects --json) use this so agents get
// BOTH the Ref-doctrine keys AND the rich data — no need to migrate
// consumers off the current field names to fix the doctrine gap.
func EnrichedRef(p *models.Project) map[string]any {
	if p == nil {
		return map[string]any{"name": "", "path": ""}
	}
	return map[string]any{
		"id":               p.ID,
		"name":             Label(p),
		"path":             p.Path,
		"display_name":     p.DisplayName,
		"first_seen_at":    p.FirstSeenAt,
		"last_activity_at": p.LastActivityAt,
		"session_count":    p.SessionCount,
		"total_tokens":     p.TotalTokens,
		"source":           p.Source,
	}
}

// EnrichedRefsFromValues is the []Project convenience for EnrichedRef.
func EnrichedRefsFromValues(projects []models.Project) []map[string]any {
	out := make([]map[string]any, 0, len(projects))
	for i := range projects {
		out = append(out, EnrichedRef(&projects[i]))
	}
	return out
}

// SessionRef enriches a Session with a project_name so the Class C
// doctrine {name, path} shape is present on session-carrying responses.
// The projectsByID lookup carries the adapter-provided DisplayName so
// jeff/hex/nanoclaw branded labels surface at Class C emitters instead
// of falling through to the basename when the session only knows its
// project's path. Pass nil to skip the lookup — Label falls back to
// filepath.Base(project_path) in that case.
func SessionRef(s *models.Session, projectsByID map[int64]*models.Project) map[string]any {
	if s == nil {
		return map[string]any{}
	}
	// Derive project_name from the enriched Project when available so
	// adapter-provided DisplayNames don't get silently dropped.
	var projectName string
	if p, ok := projectsByID[s.ProjectID]; ok && p != nil {
		projectName = Label(p)
	} else {
		projectName = Label(&models.Project{Path: s.ProjectPath})
	}
	return map[string]any{
		"id":                 s.ID,
		"project_id":         s.ProjectID,
		"project_path":       s.ProjectPath,
		"project_name":       projectName,
		"model":              s.Model,
		"git_branch":         s.GitBranch,
		"started_at":         s.StartedAt,
		"ended_at":           s.EndedAt,
		"turn_count":         s.TurnCount,
		"input_tokens":       s.InputTokens,
		"output_tokens":      s.OutputTokens,
		"cache_read_tokens":  s.CacheReadTokens,
		"cache_write_tokens": s.CacheWriteTokens,
		"source_file":        s.SourceFile,
		"has_error":          s.HasError,
		"has_subagent":       s.HasSubagent,
		"source":             s.Source,
	}
}

// SessionRefsFromValues is the []Session convenience for SessionRef.
// projectsByID (nil is fine) supplies adapter DisplayName context.
func SessionRefsFromValues(sessions []models.Session, projectsByID map[int64]*models.Project) []map[string]any {
	out := make([]map[string]any, 0, len(sessions))
	for i := range sessions {
		out = append(out, SessionRef(&sessions[i], projectsByID))
	}
	return out
}

// ProjectsByID builds a lookup from a value-slice for SessionRef's
// projectsByID parameter. Common in MCP handlers that already load
// all projects for filtering.
func ProjectsByID(projects []models.Project) map[int64]*models.Project {
	out := make(map[int64]*models.Project, len(projects))
	for i := range projects {
		out[projects[i].ID] = &projects[i]
	}
	return out
}

// ProjectsByPath is the path-keyed counterpart to ProjectsByID, used by
// CLI/TUI renders that only have a session's ProjectPath (not its
// project_id) but still want to surface adapter DisplayNames rather
// than falling through to basename.
func ProjectsByPath(projects []models.Project) map[string]*models.Project {
	out := make(map[string]*models.Project, len(projects))
	for i := range projects {
		out[projects[i].Path] = &projects[i]
	}
	return out
}

// LabelFromPath returns the Class-A label for a project identified by
// path — preferring the adapter-provided DisplayName from the lookup,
// falling back to filepath.Base(path) when the lookup doesn't have it.
// Use in CLI/TUI session-list renders that have session.ProjectPath but
// don't want to synthesize a bare Project stub (which would silently
// drop jeff/hex/nanoclaw branded labels).
func LabelFromPath(path string, byPath map[string]*models.Project) string {
	if p, ok := byPath[path]; ok && p != nil {
		return Label(p)
	}
	// Not in lookup — fall back to basename via a synthetic project.
	return Label(&models.Project{Path: path})
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
