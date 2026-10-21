# ccvault Design Document

**Date:** 2026-02-13
**Status:** Approved
**Approach:** Direct msgvault port for Claude Code conversations

## Overview

ccvault archives and indexes Claude Code conversation history for offline search, analytics, and AI integration. It mirrors msgvault's architecture, replacing email concepts with conversation concepts.

## Data Model

### Entity Mapping

| msgvault Concept | ccvault Concept | Source |
|------------------|-----------------|--------|
| Gmail account | Installation | `~/.claude/` directory |
| Email label | Project | `project` field in session data |
| Email thread | Session | `sessionId` + `.jsonl` files |
| Email message | Turn | Individual JSONL entries |
| Attachment | Referenced file | Files from tool calls |

### Core Entities

**Installation**
- Represents a Claude Code installation
- Source: `~/.claude/` (configurable)
- Contains global history and per-project sessions

**Project**
- Extracted from session `project` field (filesystem path)
- Examples: `/Users/harper/Public/src/2389/window-scroller`
- Normalized for display: `2389/window-scroller`

**Session**
- UUID identifier from Claude Code
- Maps to `<session-id>.jsonl` file in projects directory
- Metadata: start time, model(s) used, git branch, total tokens

**Turn**
- Individual JSONL entry within a session
- Types: `user`, `assistant`, `tool_use`, `tool_result`, `progress`
- Contains: uuid, parent_uuid (for threading), timestamp, content

### Extracted Metadata

- **Tools used**: Bash, Read, Write, Edit, Grep, Glob, Task, etc.
- **Files touched**: Paths from Read/Write/Edit tool calls
- **Token usage**: input_tokens, output_tokens, cache stats
- **Subagents**: From `subagents/` subdirectories
- **Git context**: Branch name when available

## Data Sources

### Primary: `~/.claude/projects/`

Structure:
```
~/.claude/projects/
  -Users-harper-Public-src-2389-window-scroller/    # URL-encoded path
    sessions-index.json                              # Session metadata
    0684b40f-4463-4492-83c6-3baa18bfb9ad.jsonl      # Full session transcript
    0684b40f-4463-4492-83c6-3baa18bfb9ad/
      subagents/
        agent-*.jsonl                                # Subagent transcripts
```

### Secondary: `~/.claude/history.jsonl`

- Quick index of user prompts only
- Contains: display text, timestamp, project, sessionId
- Useful for fast search without loading full sessions

## Storage Architecture

**Location:** `~/.ccvault/` (configurable via `CCVAULT_HOME` or config)

```
~/.ccvault/
  config.toml           # Configuration
  ccvault.db            # SQLite database (metadata + FTS5)
  analytics/
    sessions.parquet    # Analytics cache for DuckDB
    turns.parquet       # NOT BUILT — see note below
    tools.parquet       # NOT BUILT — see note below
  sync_state.json       # Tracks last sync position
```

> **Historical note (2026-10-13).** `turns.parquet` and `tools.parquet` were
> never built. `Exporter.Export` writes `sessions.parquet` and nothing else.
> The struct that described each file — `analytics.TurnRecord`,
> `analytics.ToolRecord` — was declared and never written to, and both have
> since been deleted (#77, #139). They are listed here because this document
> is the record of what was planned, not of what shipped; nothing in the tree
> produces or reads either file, and a reader looking for the code behind
> them will not find it.
>
> A turn-level or tool-level export is not ruled out, but it is unbuilt design
> work rather than a missing implementation, and #29 is its first problem: the
> deleted records carried no ordinal, so a naive per-turn export would have
> reproduced the ordering bug #29 describes.

### SQLite Schema

```sql
-- Core tables
CREATE TABLE projects (
    id INTEGER PRIMARY KEY,
    path TEXT UNIQUE NOT NULL,
    display_name TEXT NOT NULL,
    first_seen_at DATETIME,
    last_activity_at DATETIME,
    session_count INTEGER DEFAULT 0,
    total_tokens INTEGER DEFAULT 0
);

CREATE TABLE sessions (
    id TEXT PRIMARY KEY,  -- UUID
    project_id INTEGER REFERENCES projects(id),
    started_at DATETIME NOT NULL,
    ended_at DATETIME,
    model TEXT,
    git_branch TEXT,
    turn_count INTEGER DEFAULT 0,
    input_tokens INTEGER DEFAULT 0,
    output_tokens INTEGER DEFAULT 0,
    cache_read_tokens INTEGER DEFAULT 0,
    cache_write_tokens INTEGER DEFAULT 0,
    source_file TEXT NOT NULL  -- Path to .jsonl file
);

CREATE TABLE turns (
    id TEXT PRIMARY KEY,  -- UUID
    session_id TEXT REFERENCES sessions(id),
    parent_id TEXT,
    type TEXT NOT NULL,  -- user, assistant, tool_use, tool_result, progress
    timestamp DATETIME NOT NULL,
    content TEXT,  -- Extracted text content for search
    raw_json TEXT,  -- Original JSONL entry
    input_tokens INTEGER,
    output_tokens INTEGER
);

CREATE TABLE tool_uses (
    id INTEGER PRIMARY KEY,
    turn_id TEXT REFERENCES turns(id),
    session_id TEXT REFERENCES sessions(id),
    tool_name TEXT NOT NULL,
    file_path TEXT,  -- For file-related tools
    timestamp DATETIME NOT NULL
);

-- Full-text search
CREATE VIRTUAL TABLE turns_fts USING fts5(
    content,
    content=turns,
    content_rowid=rowid
);
```

### Parquet Analytics Cache

Built via `ccvault build-cache`, enables DuckDB queries:

- `sessions.parquet`: Session-level aggregates — **the only file built.**
  Carries `source` and, since #74, `parent_session_id`, so a DuckDB query over
  the cache alone can split subagent spend from top-level spend. Since #167 it
  carries `cache_write_tokens` beside `cache_read_tokens`, so the same query
  can split cheap cache reads from writes costing roughly ten times as much —
  and can recompute `total_tokens` from its own components, which until then
  came up short by exactly the write counter.
- `turns.parquet`: Turn-level data for time series — **never built** (see the
  historical note under Storage Architecture).
- `tools.parquet`: Tool usage patterns — **never built** (same note).

## CLI Commands

```
ccvault sync [--full]
    Incrementally sync from ~/.claude to ccvault database.
    --full: Force full rescan instead of incremental.

ccvault tui [--project <name>]
    Launch interactive TUI for drill-down analytics.
    --project: Start filtered to specific project.

ccvault search <query> [--json] [--limit N]
    Full-text search across all conversations.
    --json: Output results as JSON.
    --limit: Maximum results (default 20).

ccvault stats
    Display archive statistics.

ccvault list-projects [--json]
    List all indexed projects with activity stats.

ccvault list-sessions [--project <name>] [--json]
    List sessions, optionally filtered by project.

ccvault show <session-id>
    Display a specific session's conversation.

ccvault build-cache
    Rebuild Parquet analytics cache.

ccvault serve [--port 8765]
    Start MCP server for AI assistant integration.

ccvault config
    Show/edit configuration.
```

## Search Syntax

Gmail-style query syntax:

| Operator | Example | Description |
|----------|---------|-------------|
| `project:` | `project:window-scroller` | Filter by project name (partial match) |
| `model:` | `model:opus` | Filter by model name |
| `tool:` | `tool:Bash` | Sessions using specific tool |
| `file:` | `file:main.go` | Sessions touching specific file |
| `has:` | `has:error`, `has:subagent` | Feature filters |
| `before:` | `before:2026-02-01` | Date filter |
| `after:` | `after:2026-01-15` | Date filter |
| `tokens>` | `tokens>10000` | Token usage filter |
| `"..."` | `"exact phrase"` | Exact phrase match |
| bare words | `fix bug auth` | Full-text search |

## TUI Design

Built with Bubble Tea, mirroring msgvault's drill-down interface.

### Views

1. **Dashboard** (home)
   - Total sessions, turns, tokens
   - Activity sparkline (last 30 days)
   - Top projects by recent activity

2. **Projects List**
   - Sortable by name, activity, token usage
   - Shows session count, last activity
   - Enter to drill into project

3. **Sessions List**
   - Sessions for selected project (or all)
   - Shows date, model, turn count, tokens
   - Enter to view conversation

4. **Conversation View**
   - Threaded display of turns
   - Syntax highlighting for code
   - Expandable tool calls
   - j/k navigation, / to search within

5. **Search Results**
   - Matching turns with context
   - Enter to jump to full conversation

### Keybindings

- `q` / `Esc`: Back / quit
- `j` / `k`: Navigate
- `Enter`: Select / drill in
- `/`: Search
- `?`: Help
- `r`: Refresh
- `s`: Sort options
- `f`: Filter options

## MCP Server

Exposes ccvault functionality to AI assistants.

### Tools

```json
{
  "name": "search_conversations",
  "description": "Search Claude Code conversation history",
  "parameters": {
    "query": "string - search query with optional operators",
    "limit": "number - max results (default 10)"
  }
}

{
  "name": "get_session",
  "description": "Retrieve full conversation transcript",
  "parameters": {
    "session_id": "string - session UUID"
  }
}

{
  "name": "get_analytics",
  "description": "Get usage statistics and analytics",
  "parameters": {
    "type": "string - 'summary' | 'by_project' | 'by_model' | 'by_day'",
    "days": "number - lookback period (default 30)"
  }
}

{
  "name": "list_projects",
  "description": "List all indexed projects",
  "parameters": {
    "sort_by": "string - 'name' | 'activity' | 'tokens'"
  }
}
```

## Tech Stack

- **Language**: Go 1.22+
- **Database**: SQLite 3 with FTS5
- **Analytics**: DuckDB (via go-duckdb) over Parquet
- **TUI**: Bubble Tea + Lip Gloss
- **CLI**: Cobra
- **Config**: Viper + TOML
- **MCP**: JSON-RPC over stdio

## Sync Strategy

1. **Incremental by default**: Track last-synced file modification times
2. **Checkpoint file**: `sync_state.json` stores sync position
3. **Atomic updates**: Transaction per session
4. **File watching** (future): Optional live sync via fsnotify

### Sync Process

```
1. Read sync_state.json for last sync timestamp
2. Scan ~/.claude/projects/ for modified .jsonl files
3. For each modified file:
   a. Parse JSONL entries
   b. Extract session metadata
   c. Upsert project record
   d. Upsert session record
   e. Upsert turn records
   f. Update FTS index
4. Update sync_state.json
5. Rebuild parquet cache if needed
```

## Configuration

`~/.ccvault/config.toml`:

```toml
[source]
claude_home = "~/.claude"  # Claude Code data directory

[storage]
data_dir = "~/.ccvault"    # ccvault data directory

[sync]
auto_build_cache = true    # Rebuild parquet after sync
watch_mode = false         # Live file watching (future)

[tui]
theme = "dark"             # dark | light
page_size = 50             # Items per page

[mcp]
port = 8765
```

## Project Structure

```
ccvault/
  cmd/
    ccvault/
      main.go
  internal/
    config/           # Configuration loading
    db/               # SQLite operations
    sync/             # Sync from Claude Code
    search/           # Search query parsing and execution
    analytics/        # DuckDB/Parquet analytics
    tui/              # Bubble Tea interface
      dashboard.go
      projects.go
      sessions.go
      conversation.go
      search.go
    mcp/              # MCP server
  pkg/
    parser/           # JSONL parsing
    models/           # Data structures
  go.mod
  go.sum
  README.md
```

## Future Considerations

- **Multi-installation support**: Index multiple `~/.claude` directories
- **Export formats**: Markdown, HTML conversation exports
- **Diff view**: Compare similar sessions
- **Semantic search**: Embeddings-based similarity search
- **Usage tracking**: Cost estimation from token counts
