# ccvault

Archive and search your Claude Code conversation history.

Inspired by [msgvault](https://github.com/wesm/msgvault), ccvault provides offline search, analytics, and AI integration for Claude Code sessions stored in `~/.claude`.

<img width="557" height="563" alt="image" src="https://github.com/user-attachments/assets/5ce5ecee-f3eb-4a93-8685-c7b138ea08dd" />


## Features

- **Full-text search** across all conversations with Gmail-like query syntax
- **Interactive TUI** for browsing and analyzing sessions
- **DuckDB analytics** for fast aggregate queries over Parquet exports
- **MCP server** for AI assistant integration

## Installation

### Homebrew (macOS/Linux)

```bash
brew install 2389-research/tap/ccvault
```

### Using `go install`

Requires Go 1.25 or later:

```bash
go install github.com/2389-research/ccvault/cmd/ccvault@latest
```

Make sure `$GOPATH/bin` (or `$HOME/go/bin`) is in your `PATH`.

### Build from source

```bash
git clone https://github.com/2389-research/ccvault.git
cd ccvault
go build -o ccvault ./cmd/ccvault
sudo mv ccvault /usr/local/bin/
```

### Verify installation

```bash
ccvault version
```

## Quick Start

```bash
# Sync conversations from ~/.claude
ccvault sync

# Launch interactive TUI
ccvault tui

# Search conversations
ccvault search "debugging async"
ccvault search "project:myapp model:opus"

# View statistics
ccvault stats
```

## Commands

| Command | Description |
|---------|-------------|
| `quickstart` | Interactive setup guide for new users |
| `orient` | Database state summary for AI agents (use `--json`) |
| `sync` | Sync conversations from Claude Code (see [Sync modes](#sync-modes)) |
| `import [db-path]` | Merge another ccvault database into this archive |
| `tui` | Launch interactive terminal UI |
| `search [query]` | Full-text search across conversations |
| `stats` | Show archive statistics, including reclaimable space |
| `vacuum` | Reclaim dead space in the database file (see [Storage reclaim](#storage-reclaim)) |
| `list-projects` | List all indexed projects |
| `list-sessions` | List sessions (optionally filtered by project) |
| `show [session-id]` | Display a specific session |
| `export [session-id]` | Export a session to markdown |
| `build-cache` | Build Parquet analytics cache |
| `mcp` | Start MCP server for AI integration |
| `version` | Print the version number |

## Sync modes

ccvault is an archive, not a cache of what Claude Code currently has on disk.
Claude Code prunes its own session files, so the archive is usually the only
place older conversations still exist. The sync modes differ in whether they
respect that.

| Mode | What it does | Destroys pruned history? |
|------|--------------|--------------------------|
| `ccvault sync` | Re-parses only files whose mtime changed | No |
| `ccvault sync --full` | Re-parses every discovered file, ignoring mtimes. Each file replaces its own rows; rows whose source file is gone are left alone | No |
| `ccvault sync --rebuild` | Wipes every table, then re-scans, so the archive holds exactly what is on disk now | **Yes** |

Reach for `--full` after a parser change that extracts more from the same
JSONL. Reach for `--rebuild` only when you actually want the archive reduced
to current disk state; it prompts for confirmation, reports how many sessions
have no source file left (those are gone for good), and takes a `VACUUM INTO`
backup to `~/.ccvault/backups/` first.

To recover from a rebuild — or to consolidate two machines' archives — merge a
database back in:

```bash
ccvault import ~/.ccvault/backups/ccvault-20260905-140418.db
```

`import` only adds. Sessions absent locally are inserted with their turns and
tool uses; a session present in both is kept as-is unless the incoming copy
ended later. The source database is only read, and the whole merge is one
transaction.

### One-time counter correction

`projects.session_count` and `total_tokens` were maintained additively, so
every re-parse of an already-indexed session added to them again. Sync now
recomputes both from the `sessions` rows that actually exist.

The first sync after upgrading therefore **corrects these two numbers
downward**, sometimes by a lot — an archive synced many times may show project
token counts an order of magnitude above the real figure. Nothing is deleted
and no session, turn, or tool use is affected; only the two display counters
on `projects` change, and they change to the truth. `first_seen_at` and
`last_activity_at` are left alone.

## Storage reclaim

Incremental sync replaces the rows of every changed session file. SQLite keeps
the pages it frees on an internal freelist instead of returning them to the
filesystem, so the database file grows away from the data it holds. A
long-running archive can end up majority dead space — one real archive measured
4.6 GB on disk holding 1.9 GB of data.

`ccvault stats` and `ccvault orient --json` report the condition:

```
Storage:
  Database file: 4.6 GB
  Reclaimable:   2.9 GB (60% of the file)
  Reclaim it with 'ccvault vacuum'.
```

`sync` prints the same one-line note when the waste crosses 25% of the file and
32 MB. Nothing is reclaimed automatically — compaction rewrites the whole file
and needs the database to itself, which is no business of a sync run.

`ccvault vacuum` reclaims it:

```bash
ccvault vacuum          # human-readable progress and before/after figures
ccvault vacuum --json   # same numbers for machine consumption
```

It writes a compacted copy beside the live database with `VACUUM INTO`, checks
the copy with `integrity_check` and a row count per table, and renames it over
the original in one atomic step. The original is never modified, so any failure
leaves it exactly as it was. Consequences worth knowing:

- It needs free disk space for the compacted copy — roughly the size of the
  live data, not of the whole file. It refuses up front if the space isn't
  there, rather than failing partway through a multi-gigabyte write.
- It takes the database exclusively. Close the TUI, MCP server, and any running
  sync first; it refuses rather than compacting a file someone else is writing.
- It rewrites the whole file. Expect tens of seconds on a multi-gigabyte
  archive (4.6 GB compacted to 1.9 GB in about 21 seconds).
- A WAL-mode archive stays in WAL mode. `VACUUM INTO` always writes a
  rollback-journal database, so the mode is restored after the swap; and the
  write-ahead log is only removed once SQLite confirms every frame of it was
  folded into the main file.

## Search Syntax

ccvault supports Gmail-like query syntax:

```
project:name     Filter by project path/name
model:opus       Filter by model (partial match)
tool:Bash        Sessions using specific tool
file:path        Filter by file path
before:date      Sessions before date (YYYY-MM-DD)
after:date       Sessions after date
has:error        Sessions with errors
has:subagent     Sessions with subagent usage
"exact phrase"   Exact phrase match
```

Examples:
```bash
ccvault search "debugging the API endpoint"
ccvault search "project:myapp after:2024-01-01"
ccvault search "tool:Edit model:opus"
ccvault search '"error handling" project:backend'
```

## MCP Server

ccvault includes an MCP (Model Context Protocol) server for AI assistant integration.

```bash
ccvault mcp
```

Available tools:
- `search_conversations` - Full-text search across conversations
- `get_session_summary` - Quick overview of a session (metadata, stats, tools used)
- `get_turns` - Paginated turns from a session
- `get_session` - Full session in markdown format
- `list_sessions` - List recent sessions
- `list_projects` - List all indexed projects
- `get_stats` - Archive statistics
- `get_analytics` - Detailed usage analytics

### Claude Desktop Configuration

Add to your Claude Desktop config (`~/Library/Application Support/Claude/claude_desktop_config.json`):

```json
{
  "mcpServers": {
    "ccvault": {
      "command": "ccvault",
      "args": ["mcp"]
    }
  }
}
```

## Claude Code Skill

ccvault ships with a [Claude Code skill](skills/ccvault/SKILL.md) that teaches AI agents how to effectively mine conversation history. It includes search strategy patterns, workflow prompts for session orientation and on-demand recall, and a full tool/query reference card.

See [`skills/ccvault/`](skills/ccvault/) for the full skill.

## Configuration

ccvault uses sensible defaults but can be configured via environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `CCVAULT_CLAUDE_HOME` | `~/.claude` | Claude Code data directory |
| `CCVAULT_DATA_DIR` | `~/.ccvault` | ccvault data directory |

### Pointing ccvault at another archive

Two persistent flags are available on every command:

| Flag | Answers |
|------|---------|
| `--data-dir <path>` | Where the database lives |
| `--config <path>` | Which config file to read |

`data_dir` is resolved highest precedence first:

1. `--data-dir`
2. `CCVAULT_DATA_DIR`
3. `data_dir = "..."` in the config file
4. `~/.ccvault`

`--data-dir` also **replaces** the config search path, so `~/.ccvault` is not
read at all when it is given — which is what makes the CLI runnable against a
throwaway archive in a sandbox that must not touch the real one:

```bash
mkdir -p /tmp/fixture
printf 'claude_home = "/tmp/fixture/claude"\n' > /tmp/fixture/config.toml
ccvault --data-dir /tmp/fixture sync
ccvault --data-dir /tmp/fixture stats --json
```

`--config` names a single file instead of searching for one, and a path that
does not exist is an error rather than a silent fall-back to defaults. Given
both, `--config` decides where settings come from and `--data-dir` still wins
for `data_dir` itself.

Note that `--data-dir` redirects the **archive**, not the **source** it reads.
`claude_home` still defaults to `~/.claude`, so a sandbox that must not read
the real conversation files needs to redirect that too — via `claude_home` (or
a `sources` list) in the fixture config, as above, or `CCVAULT_CLAUDE_HOME`.
A config file that exists but cannot be parsed is now an error for the same
reason: silently falling back to defaults would send a fixture run at the real
`~/.claude`.

## Data Storage

- **SQLite database**: `~/.ccvault/ccvault.db` - Session data with FTS5 full-text search
- **Analytics cache**: `~/.ccvault/analytics/sessions.parquet` - Parquet export for DuckDB queries

## Architecture

```
ccvault/
├── cmd/ccvault/     # CLI entry point
├── pkg/
│   ├── models/      # Data structures
│   └── parser/      # JSONL session parser
└── internal/
    ├── config/      # Configuration
    ├── db/          # SQLite layer with FTS5
    ├── sync/        # Incremental sync logic
    ├── search/      # Query parsing and execution
    ├── export/      # Markdown export
    ├── tui/         # Bubble Tea terminal UI
    ├── analytics/   # DuckDB/Parquet export
    └── mcp/         # MCP server
```

## License

MIT
