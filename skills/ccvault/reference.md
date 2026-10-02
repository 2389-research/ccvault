# ccvault Quick Reference

## 1. Tools Quick Reference

| Tool | Required Params | Optional Params | Returns | Notes |
|------|----------------|-----------------|---------|-------|
| `search_conversations` | `query` (string) | `limit` (number, default 10, max 50), `offset` (number) | Paginated results with 200-char snippets | Check `has_more` / `next_offset` for pagination. Each result carries `project_name` (the adapter-provided label, or the path basename as fallback) alongside `project_path`. An empty result for a `tool:` query adds `similar_tool_names` with close matches; both that lookup and the project-name enrichment are enrichment queries — if one fails, its field is omitted and a `warnings` entry (`similar_tool_names unavailable: …` / `project enrichment unavailable: …`) appears instead. Search is **never** filtered by the subagent listing default, so a hit can sit in a transcript `list_sessions` does not show: each result carries `parent_session_id` (null when the hit is in a top-level session) |
| `get_session_summary` | `session_id` (string) | — | Metadata, turn counts by type, top 10 tools used, first/last user messages (500 chars each) | Most cost-effective entry point for any session |
| `get_turns` | `session_id` (string) | `offset` (number, default 0), `limit` (number, default 20, max 50), `type` (user/assistant/tool_result) | Paginated turns, content truncated at 1000 chars | Includes tool names; `has_thinking` flag on assistant turns. Accepts a subagent session id with no extra parameter |
| `get_session` | `session_id` (string) | — | Full session as markdown | Sessions over 100 turns come back without `markdown`: the response carries `session_id`, `turn_count`, `hint`, and a `markdown unavailable: large session with N turns...` entry in the top-level `warnings[]` array — there is no singular `warning` field. Markdown truncates at 50K chars. Prefer summary + turns for large sessions |
| `list_sessions` | — | `project` (string, partial match), `limit` (number, default 20, max 100), `offset` (number, default 0), `include_subagents` (bool, default false), `subagents_of` (string, a session id) | Recent **top-level** sessions sorted by date desc, OR an `ambiguous_project_filter` object when the `project` filter matches multiple projects | Partial match on path or display name; returns error (not empty) if no project matches; when the filter matches N>1 projects, returns `{ambiguous_project_filter: true, filter, matched_projects: [{name, path}], hint}` instead of sessions — the agent then re-calls with a more specific filter (typically a full path). Paginates like `search_conversations`: check `has_more` / `next_offset`. Session objects include both `project_path` and `project_name` — the adapter-provided label if any, or the basename fallback — plus `parent_session_id` (null for top-level) and `subagent_count`, always, filtered or not. Subagent sessions are hidden by default; `include_subagents: true` flattens them in, `subagents_of: "<id>"` returns just one session's children. |
| `list_projects` | — | `sort` (name/activity/tokens/sessions, default: activity), `limit` (number, default 50, max 100), `offset` (number, default 0) | Projects with session counts and token usage | Use to discover project names before searching; paginates like `search_conversations` — check `has_more` / `next_offset`. Each project object carries both `name` (the adapter-provided label) and `path` (the disambiguating identifier) — always prefer `path` when uniqueness matters. Sort order includes `path ASC` as tiebreaker so pagination stays stable. |
| `get_stats` | — | — | Archive-wide counts: projects, sessions, turns, total tokens, model breakdown, top tools, date range | Fast overview of the entire archive. `first_activity`, `last_activity`, `days_span`, and `top_tools` are enrichment fields: if their queries fail the fields are omitted and a `warnings` entry is added instead |
| `get_analytics` | — | `days` (number, default 30) | Daily token breakdown, top projects, model breakdown | Requires DuckDB analytics cache; when the cache is missing, returns `analytics.available: false` with a build-cache hint. Warnings from any degraded query — stats or analytics — appear at `result.warnings`; the embedded `summary` never carries its own `warnings` |

Degraded-query warnings are uniform: each one reads `<what> unavailable: <why>` and arrives in a top-level `warnings[]` array. A warning means the field it names is **absent** from the response, not empty — so check presence rather than trusting a zero value. The one exception names an enrichment rather than a field: `project enrichment unavailable: …` (on `search_conversations` and `list_sessions`) means `project_name` is still present but fell back to the path basename, so it is not the adapter-provided label.

Pagination is uniform across `search_conversations`, `get_turns`, `list_sessions`, and `list_projects`: a paginated response echoes `offset` and the `limit` actually applied (an over-max `limit` is clamped, so read it back rather than assuming), and a truncated one adds `has_more: true` plus `next_offset`. Page by re-calling with `offset: next_offset` — not by raising `limit`. One response is not paginated: when `list_sessions` receives a `project` filter matching several projects it returns `ambiguous_project_filter` with `matched_projects[]` and none of the pagination fields.

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

## 7. Subagent Sessions

A subagent (`Task`/Agent dispatch) writes its own transcript, which the archive
stores as its **own session row** with a minted composite id:

```
claude-code:04fb5717-c508-4503-ac85-dc11787cafaa:agent-a01b71e80ea28b3ad
nanoclaw:845f7a4e-2827-4f87-8c31-2e4d0b429405:agent-a07c3516373ab4719
 \___ source ___/ \______ parent session uuid ______/ \___ agentId ___/
```

The id is minted because the transcript's own `sessionId` field holds the
*parent's* uuid — trusting it would overwrite the session that dispatched the
work. `parent_session_id` carries the relationship; it is null for a top-level
session.

Why this matters for reading the archive:

- Subagent transcripts **outnumber top-level ones** (roughly 2.6:1 on a real
  machine), and they hold about **29% more tokens on top of their parents** —
  none of it double-counted, because a parent transcript contains no sidechain
  lines at all. That is where most of the actual tool work lives.
- **Listings hide them by default** — `list_sessions`, `ccvault list-sessions`,
  and the TUI session list all show top-level sessions only. Every row reports
  `subagent_count`, so a filtered listing still tells you what it filtered.
- **Nothing is unreachable.** `get_session`, `get_turns`, `ccvault show`, and
  `ccvault export` all take a subagent id with no flag. To expand them:
  `list_sessions {include_subagents: true}` or `{subagents_of: "<id>"}`;
  `ccvault list-sessions --include-subagents` / `--subagents-of <id>`; in the
  TUI, press `a` in a session's conversation view.
- **Search is never filtered.** A hit inside a subagent transcript comes back
  like any other, with `parent_session_id` naming the session that dispatched
  it.
- **Analytics and `get_stats` always count them.** Totals taken before this
  landed are lower for that reason; they were undercounting, not drifting.
- A subagent whose parent is *not* in the archive is shown in the default
  listing rather than hidden, since no parent's `subagent_count` would
  otherwise account for it.

## 8. Reading While a Sync Is Running

The MCP server reads the same SQLite file `ccvault sync` writes, in WAL mode,
with no cross-process lock. A reader gets a consistent snapshot taken when its
transaction starts, so it is never blocked by the in-flight writer and never
sees a half-written transaction. Where it does have to wait — SQLite still
serialises some operations on the WAL index — it waits up to 5 seconds
(`busy_timeout`) before returning an error.

> Accurate only since the fix for issue #47. Before that the DSN's options
> were silently discarded by the driver, so the archive was really in
> rollback-journal mode with `busy_timeout = 0`, and a reader contending with
> the writer **errored immediately** instead of observing any of the
> behaviour described below. An archive created before that fix is converted
> to WAL the first time any ccvault command opens it.

Because WAL is now real, the database has `-wal` and `-shm` sidecar files
beside `ccvault.db`. Anything that copies the archive at the file level must
take all three, or take a backup with `ccvault sync --rebuild`'s automatic
backup / `BackupTo`, which produce a single self-contained file.

What a concurrent reader can observe depends on which sync mode is running.

| Mode | What a reader sees mid-sync |
|------|-----------------------------|
| `ccvault sync` (incremental) | Rows for changed files appear as the scan reaches them. Counts grow; nothing disappears. |
| `ccvault sync --full` | Every file is re-parsed, each in its own transaction. A session's turns are deleted and re-inserted inside one transaction, so a session never appears empty. Counts shift slightly as re-parses land; nothing disappears. |
| `ccvault sync --rebuild` | **The archive is emptied first.** `list_sessions`, `get_stats`, and `search_conversations` return zero rows immediately after the wipe commits, then climb back as the re-scan proceeds. |

Practical guidance:

- Zero results from `get_stats` or `list_sessions` on an archive that had data a
  moment ago means a `--rebuild` is in flight, not an empty archive. Retry
  rather than concluding there is no history.
- Don't treat counts as stable across two calls during any sync — pagination
  (`offset` / `next_offset`) can skip or repeat rows if the row set changed in
  between. Re-run the first page if totals move.
- `--rebuild` is the only destructive mode and the only one that can briefly
  present an empty archive. `--full` re-parses without wiping, so it is the
  safe mode to run on a schedule.
- `ccvault import <db>` is additive and runs in a single transaction: readers
  see the archive before or after the merge, never partway through.
- `ccvault vacuum` is the one command an idle MCP server can block. Compaction
  takes an exclusive lock, and in WAL mode holding the `-shm` index is enough
  to deny it — so any process that has opened the archive and run a single
  query will make `vacuum` refuse with "database is in use by another
  process", even with no query in flight. Stop the MCP server (and the TUI)
  before compacting. Nothing is modified when it refuses.

## 9. Staleness Note

This reference reflects the ccvault MCP server as of its creation. If a query or tool call fails unexpectedly, check the actual MCP server tool descriptions (via the `tools/list` method) which are the authoritative source of truth.
