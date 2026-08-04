# ccvault Quick Reference

## 1. Tools Quick Reference

| Tool | Required Params | Optional Params | Returns | Notes |
|------|----------------|-----------------|---------|-------|
| `search_conversations` | `query` (string) | `limit` (number, default 10, max 50), `offset` (number) | Paginated results with 200-char snippets | Check `has_more` / `next_offset` for pagination |
| `get_session_summary` | `session_id` (string) | — | Metadata, turn counts by type, top 10 tools used, first/last user messages (500 chars each) | Most cost-effective entry point for any session |
| `get_turns` | `session_id` (string) | `offset` (number, default 0), `limit` (number, default 20, max 50), `type` (user/assistant/tool_result) | Paginated turns, content truncated at 1000 chars | Includes tool names; `has_thinking` flag on assistant turns |
| `get_session` | `session_id` (string) | — | Full session as markdown | WARNING: returns warning object (no markdown) for 100+ turn sessions; truncates at 50K chars. Prefer summary + turns for large sessions |
| `list_sessions` | — | `project` (string, partial match), `limit` (number, default 20, max 100) | Recent sessions sorted by date desc | Partial match on path or display name; returns error (not empty) if no project matches; sets has_more when truncated |
| `list_projects` | — | `sort` (name/activity/tokens/sessions, default: activity), `limit` (number, default 50, max 100) | Projects with session counts and token usage | Use to discover project names before searching; sets has_more when truncated |
| `get_stats` | — | — | Archive-wide counts: projects, sessions, turns, total tokens, model breakdown, top tools, date range | Fast overview of the entire archive. `first_activity`, `last_activity`, `days_span`, and `top_tools` are enrichment fields: if their queries fail the fields are omitted and a `warnings` entry is added instead |
| `get_analytics` | — | `days` (number, default 30) | Daily token breakdown, top projects, model breakdown | Requires DuckDB analytics cache; when the cache is missing, returns `analytics.available: false` with a build-cache hint. Degraded stats appear as `summary.warnings`; degraded analytics queries as top-level `warnings` |

## 2. Search Query Syntax

| Operator | Format | Example | Notes |
|----------|--------|---------|-------|
| Project | `project:name` | `project:myapp` | Partial match on path or display name |
| Model | `model:name` | `model:opus` | Partial match (opus, sonnet, haiku) |
| Tool | `tool:Name` | `tool:Bash` | Case-insensitive, must match the full tool name (e.g., `Bash`, `Read`, `Edit`, `Write`, `Grep`, `Glob`, `Task`, `WebFetch`; MCP tools are stored under their full prefixed names like `mcp__ccvault__search_conversations`) |
| File | `file:path` | `file:auth.py` | Matches file paths mentioned in session |
| Before | `before:DATE` | `before:2026-02-01` | See date formats below |
| After | `after:DATE` | `after:thisweek` | See date formats below |
| Has error | `has:error` | `has:error` | Filters to sessions flagged with tool errors during sync |
| Has subagent | `has:subagent` | `has:agent` | Filters to sessions using Task (subagent) tool — `has:subagent` and `has:agent` both accepted |
| Exact phrase | `"phrase"` | `"deploy script"` | Quoted exact phrase matching |
| Free text | `terms` | `authentication bug` | FTS5 full-text search on unquoted terms |

Operators combine freely: `project:myapp tool:Bash "deploy" after:thisweek`

## 3. Date Formats

| Format | Example |
|--------|---------|
| YYYY-MM-DD | `2026-01-15` |
| YYYY/MM/DD | `2026/01/15` |
| MM/DD/YYYY | `01/15/2026` |
| Short month | `Jan 15, 2026` |
| Full month | `January 15, 2026` |
| Relative | `today`, `yesterday`, `week`/`thisweek` (last 7 days), `month`/`thismonth` (last 30 days) |

## 4. MCP Prompts

| Prompt | Required Args | Optional Args | Purpose |
|--------|--------------|---------------|---------|
| `summarize_recent` | — | `days` (default 7) | Summarize recent activity across all projects |
| `analyze_project` | `project` | — | Deep analysis of a specific project's history |
| `find_solutions` | `topic` | — | Search for past solutions to a problem domain |
| `review_session` | `session_id` | — | Detailed review of a single session |
| `compare_approaches` | `topic` | — | Find and compare different approaches tried |
| `tool_usage_report` | — | `tool` | Analyze tool usage patterns |

## 5. Common Query Recipes

```
# Recent Bash commands
tool:Bash after:thisweek

# Find discussions about a library
"react-query" project:frontend

# Find file-specific work
file:auth.py project:backend

# Model-specific sessions
model:opus project:myapp

# Combined date range
after:2026-01-01 before:2026-02-01 project:myapp

# Search for error messages
"connection refused" project:backend

# Find sessions with tool errors
has:error project:myapp

# Find sessions using subagents
has:subagent after:thisweek

# Find Edit-heavy sessions (refactoring)
tool:Edit project:myapp after:month
```

## 6. Truncation Limits

| Context | Limit |
|---------|-------|
| Search snippets | 200 chars |
| Session summary messages | 500 chars |
| Turn content | 1,000 chars |
| Full session markdown | 50,000 chars |
| Tools list in summary | Top 10 |

## 7. Staleness Note

This reference reflects the ccvault MCP server as of its creation. If a query or tool call fails unexpectedly, check the actual MCP server tool descriptions (via the `tools/list` method) which are the authoritative source of truth.
