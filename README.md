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
| `sync` | Sync conversations from Claude Code |
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

## Group Mode (Team Vault)

ccvault supports pushing your local vault to a shared team server so a whole team can search and get analytics on their combined conversation history.

### Server

Run `ccvaultd` on any box the team can reach (SSH — no HTTP or TLS setup required):

```bash
ccvaultd --data /var/lib/ccvaultd \
         --addr :2222 \
         --authorized-keys /etc/ccvaultd/authorized_keys
```

Populate `authorized_keys` with one line per authorized developer, gitolite-style — the trailing comment becomes their attribution identity:

```
ssh-ed25519 AAAAC3Nz... alice@company.com
ssh-ed25519 AAAAC3Nz... bob@company.com
```

Send `SIGHUP` after editing to reload without restarting.

### Client

```bash
ccvault remote add origin ccvault@vault.company.com
ccvault push                          # incremental, default remote=origin
ccvault push --dry-run
ccvault search "the payments bug" --remote origin
ccvault stats --remote origin
ccvault list-sessions --remote origin
```

Uses your ssh-agent (or `~/.ssh/id_ed25519`, `~/.ssh/id_rsa`). No tokens.

### What gets pushed

Every session in your local vault is pushed on first push; subsequent pushes are incremental (only new or updated sessions). Push is one-way — the team vault is not synced back to your laptop. Query it via `--remote` when you want team-wide search or analytics.

**Trust model:** ccvaultd assumes a closed, trusted team. There is no client-side redaction of secrets or sensitive content — treat the team vault like a private git repo. Only add pubkeys you trust to `authorized_keys`.

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
