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
| `stats` | Show archive statistics |
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
