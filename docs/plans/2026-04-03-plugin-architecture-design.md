# Plugin Architecture Design

## Goal

Make ccvault support multiple AI coding tool session formats (Claude Code, Codex, Gemini CLI, custom agentic loops) via a source adapter abstraction, while keeping the existing DB, analytics, TUI, and MCP layers largely unchanged.

## Design Decisions

- **Approach A (Simple Adapter Interface)** — one interface per source, no pipeline decomposition. Sources are similar enough that this covers everything. Optional hook interfaces (Approach C) can be added later if needed.
- **Config-driven with compiled adapters** — adapters are Go packages in-repo, activated via config. External plugin binaries deferred to future work.
- **Common core + extensions data model** — shared fields are first-class columns; source-specific data preserved in `raw_json` and a `metadata` JSON column.
- **Single shared database** — all sources sync into one SQLite DB. A `source` column distinguishes provenance.
- **Versioned migrations** — replace the current fire-and-forget ALTER approach with numbered migrations and a `schema_version` table.

## Source Adapter Interface

New package: `pkg/adapter/`

```go
type SessionFile struct {
    Path        string
    ProjectPath string
    ModTime     time.Time
}

type ParsedSession struct {
    ID          string
    ProjectPath string
    Turns       []ParsedTurn
    Model       string
    GitBranch   string
    StartedAt   time.Time
    EndedAt     time.Time
    SourceName  string           // "claude-code", "codex", etc.
    Metadata    map[string]any
}

type ParsedTurn struct {
    ID           string
    ParentID     string
    Type         string          // "user", "assistant", "system"
    Timestamp    time.Time
    Content      string          // normalized text content
    RawJSON      json.RawMessage
    InputTokens  int64
    OutputTokens int64
    ToolUses     []ParsedToolUse
    HasError     bool
}

type ParsedToolUse struct {
    ToolName string
    FilePath string
}

type SourceAdapter interface {
    Name() string
    Discover(root string) ([]SessionFile, error)
    Parse(path string) (*ParsedSession, error)
}
```

## Adapter Registry

`pkg/adapter/registry.go` — a map of type name → factory function.

```go
type AdapterFactory func() SourceAdapter

var registry = map[string]AdapterFactory{}

func Register(typeName string, factory AdapterFactory)
func Get(typeName string) (SourceAdapter, error)
```

Built-in adapters register via `init()`:
- `pkg/adapter/claudecode/` — wraps existing `pkg/parser/` logic
- `pkg/adapter/codex/` — Codex session format
- `pkg/adapter/gemini/` — Gemini CLI format (future)

Custom loops that share Claude Code's format use `type: claude-code` with a different `path` — no new adapter code needed.

## Config

Multi-source config in `~/.ccvault/config.yaml`:

```yaml
data_dir: ~/.ccvault

sources:
  - name: claude-code
    type: claude-code
    path: ~/.claude
  - name: codex
    type: codex
    path: ~/.codex
  - name: my-agent
    type: claude-code
    path: ~/work/agent-logs
```

**Backward compatibility:** if no `sources` key exists, fall back to current single-source behavior (`claude_home` key → one claude-code source with that path).

## DB Schema Changes

### Versioned Migration System

New table:

```sql
CREATE TABLE IF NOT EXISTS schema_version (
    version INTEGER NOT NULL,
    applied_at TEXT NOT NULL DEFAULT (datetime('now'))
);
```

Numbered migration files (embedded via `//go:embed`):

```text
internal/db/migrations/
  001_initial_schema.sql       -- current schema.sql content
  002_add_error_subagent.sql   -- has_error, has_subagent columns + indexes
  003_add_source_columns.sql   -- source column on sessions, projects, source_files
```

Migration runner:
1. Create `schema_version` table if not exists
2. Query `MAX(version)` (0 if empty)
3. For existing databases with no `schema_version` table, detect current state and bootstrap at version 2
4. Run any migrations with version > current, in order, within a transaction
5. Record each applied migration in `schema_version`

### Source Columns (Migration 003)

```sql
ALTER TABLE sessions ADD COLUMN source TEXT NOT NULL DEFAULT 'claude-code';
ALTER TABLE projects ADD COLUMN source TEXT NOT NULL DEFAULT 'claude-code';
ALTER TABLE source_files ADD COLUMN source TEXT NOT NULL DEFAULT 'claude-code';
```

Defaults ensure existing data is tagged correctly with no manual intervention.

## Sync Layer Changes

`internal/sync/sync.go` changes from calling `parser.ScanClaudeHome()` directly to iterating over configured sources:

```go
for _, src := range s.sources {
    adpt, err := adapter.Get(src.Type)
    files, err := adpt.Discover(src.Path)
    for _, f := range files {
        parsed, err := adpt.Parse(f.Path)
        parsed.SourceName = src.Name
        // existing merge/dedupe/insert logic unchanged
    }
}
```

The inner loop (upsert project → upsert session → insert turns → extract tool uses) receives `ParsedSession`/`ParsedTurn` structs instead of calling parser functions directly.

Incremental sync continues to work via `source_files` mtime tracking. The `source` column prevents collisions between files from different sources that happen to share a path.

Existing detection logic (`detectErrors`, `detectSubagents`, `formatToolUse`) moves into the Claude Code adapter's `Parse()` method.

## Search Changes

Add `source:` filter to `internal/search/` query parser. Example: `source:codex auth middleware` searches only Codex sessions.

## What Stays Unchanged

- DB CRUD operations (just receive the new `source` field)
- FTS5 triggers and full-text search
- Analytics / Parquet export (pulls from DB, source-agnostic)
- TUI (works against DB, may add source column to display)
- MCP server (works against search/DB layer)
- Markdown export

## Adapter Implementation Notes

### Claude Code Adapter

Wraps existing `pkg/parser/` and `pkg/parser/scanner.go`:
- `Discover()` → calls `ScanClaudeHome()`
- `Parse()` → calls `ParseSession()`, runs `detectErrors`, `detectSubagents`, `formatToolUse`, maps to `ParsedSession`/`ParsedTurn`

### Codex Adapter

Needs investigation of Codex session file format. Expected to be structurally similar (JSONL, turns with roles) but with different field names and tool schemas.

### Custom Loops (Claude Code-like)

Use `type: claude-code` adapter with a different `path`. No new code needed unless the format diverges.
