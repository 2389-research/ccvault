# Plugin Architecture Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Refactor ccvault to support multiple AI coding tool session formats via a source adapter interface.

**Architecture:** A `SourceAdapter` interface abstracts discovery and parsing. Built-in adapters (Claude Code, Codex) implement it. Config supports multiple sources. A versioned migration system replaces the current ad-hoc ALTER approach. The sync layer iterates over configured sources instead of hardcoding Claude Code.

**Tech Stack:** Go, SQLite, go:embed for migrations

---

### Task 1: Versioned Migration System

**Files:**
- Create: `internal/db/migrations/001_initial_schema.sql`
- Create: `internal/db/migrations/002_add_error_subagent.sql`
- Create: `internal/db/migrator.go`
- Modify: `internal/db/db.go:60-88` (replace init with migrator)
- Modify: `internal/db/schema.sql` (keep as reference, migrations are source of truth)
- Test: `internal/db/migrator_test.go`

**Step 1: Write the failing test**

```go
// internal/db/migrator_test.go
package db

import (
    "database/sql"
    "testing"

    _ "github.com/mattn/go-sqlite3"
)

func TestMigrator_FreshDatabase(t *testing.T) {
    db, err := sql.Open("sqlite3", ":memory:")
    if err != nil {
        t.Fatal(err)
    }
    defer db.Close()

    err = RunMigrations(db)
    if err != nil {
        t.Fatalf("RunMigrations failed: %v", err)
    }

    // Verify schema_version table exists and has correct version
    var version int
    err = db.QueryRow("SELECT MAX(version) FROM schema_version").Scan(&version)
    if err != nil {
        t.Fatalf("query schema_version: %v", err)
    }
    if version != 2 {
        t.Errorf("expected version 2, got %d", version)
    }

    // Verify tables exist
    tables := []string{"projects", "sessions", "turns", "tool_uses", "turns_fts", "source_files", "schema_version"}
    for _, table := range tables {
        var name string
        err := db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&name)
        if err != nil {
            t.Errorf("table %s not found: %v", table, err)
        }
    }

    // Verify has_error and has_subagent columns exist (from migration 002)
    var hasError int
    err = db.QueryRow("SELECT has_error FROM sessions LIMIT 0").Scan(&hasError)
    // Should not error — column exists (no rows is fine)
}

func TestMigrator_ExistingDatabase(t *testing.T) {
    db, err := sql.Open("sqlite3", ":memory:")
    if err != nil {
        t.Fatal(err)
    }
    defer db.Close()

    // Simulate existing database: run migrations once
    err = RunMigrations(db)
    if err != nil {
        t.Fatalf("first RunMigrations failed: %v", err)
    }

    // Run again — should be idempotent
    err = RunMigrations(db)
    if err != nil {
        t.Fatalf("second RunMigrations failed: %v", err)
    }

    var count int
    err = db.QueryRow("SELECT COUNT(*) FROM schema_version").Scan(&count)
    if err != nil {
        t.Fatal(err)
    }
    // Should have exactly 2 entries (one per migration), not 4
    if count != 2 {
        t.Errorf("expected 2 migration records, got %d", count)
    }
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/db/ -run TestMigrator -v`
Expected: FAIL — `RunMigrations` not defined

**Step 3: Create migration SQL files**

Create `internal/db/migrations/001_initial_schema.sql` — extract the current `schema.sql` content (the CREATE TABLE IF NOT EXISTS statements for projects, sessions, turns, tool_uses, turns_fts, triggers, sync_state, source_files).

Create `internal/db/migrations/002_add_error_subagent.sql`:
```sql
ALTER TABLE sessions ADD COLUMN has_error BOOLEAN DEFAULT 0;
ALTER TABLE sessions ADD COLUMN has_subagent BOOLEAN DEFAULT 0;
CREATE INDEX IF NOT EXISTS idx_sessions_has_error ON sessions(has_error) WHERE has_error = 1;
CREATE INDEX IF NOT EXISTS idx_sessions_has_subagent ON sessions(has_subagent) WHERE has_subagent = 1;
```

**Step 4: Write the migrator**

```go
// internal/db/migrator.go
package db

import (
    "database/sql"
    "embed"
    "fmt"
    "sort"
    "strings"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// RunMigrations creates the schema_version table and applies any pending migrations.
// For existing databases without schema_version, it detects current state and bootstraps.
func RunMigrations(db *sql.DB) error {
    // Create schema_version table if not exists
    _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (
        version INTEGER NOT NULL,
        applied_at TEXT NOT NULL DEFAULT (datetime('now'))
    )`)
    if err != nil {
        return fmt.Errorf("create schema_version: %w", err)
    }

    // Get current version
    var currentVersion int
    err = db.QueryRow("SELECT COALESCE(MAX(version), 0) FROM schema_version").Scan(&currentVersion)
    if err != nil {
        return fmt.Errorf("query current version: %w", err)
    }

    // If schema_version is empty but tables exist, bootstrap
    if currentVersion == 0 {
        currentVersion, err = detectExistingState(db)
        if err != nil {
            return fmt.Errorf("detect existing state: %w", err)
        }
        // Record bootstrapped versions
        for v := 1; v <= currentVersion; v++ {
            _, err = db.Exec("INSERT INTO schema_version (version) VALUES (?)", v)
            if err != nil {
                return fmt.Errorf("record bootstrap version %d: %w", v, err)
            }
        }
    }

    // Load and sort migration files
    entries, err := migrationFS.ReadDir("migrations")
    if err != nil {
        return fmt.Errorf("read migrations dir: %w", err)
    }
    sort.Slice(entries, func(i, j int) bool {
        return entries[i].Name() < entries[j].Name()
    })

    // Apply pending migrations
    for i, entry := range entries {
        version := i + 1
        if version <= currentVersion {
            continue
        }
        content, err := migrationFS.ReadFile("migrations/" + entry.Name())
        if err != nil {
            return fmt.Errorf("read migration %s: %w", entry.Name(), err)
        }

        tx, err := db.Begin()
        if err != nil {
            return fmt.Errorf("begin tx for migration %d: %w", version, err)
        }

        // Execute each statement in the migration (split on semicolons)
        for _, stmt := range splitStatements(string(content)) {
            stmt = strings.TrimSpace(stmt)
            if stmt == "" {
                continue
            }
            if _, err := tx.Exec(stmt); err != nil {
                tx.Rollback()
                return fmt.Errorf("migration %d (%s): %w", version, entry.Name(), err)
            }
        }

        _, err = tx.Exec("INSERT INTO schema_version (version) VALUES (?)", version)
        if err != nil {
            tx.Rollback()
            return fmt.Errorf("record migration %d: %w", version, err)
        }

        if err := tx.Commit(); err != nil {
            return fmt.Errorf("commit migration %d: %w", version, err)
        }
    }

    return nil
}

// detectExistingState checks which migrations have already been applied
// by inspecting the actual schema.
func detectExistingState(db *sql.DB) (int, error) {
    // Check if sessions table exists (migration 001)
    var tableName string
    err := db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name='sessions'").Scan(&tableName)
    if err != nil {
        return 0, nil // Fresh database
    }

    // Check if has_error column exists (migration 002)
    rows, err := db.Query("PRAGMA table_info(sessions)")
    if err != nil {
        return 1, nil
    }
    defer rows.Close()

    for rows.Next() {
        var cid int
        var name, typ string
        var notNull int
        var dflt sql.NullString
        var pk int
        if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
            continue
        }
        if name == "has_error" {
            return 2, nil
        }
    }

    return 1, nil
}

// splitStatements splits SQL content on semicolons, respecting triggers.
func splitStatements(content string) []string {
    // Split on semicolons that are followed by a newline or end of string.
    // This is a simplification — doesn't handle semicolons inside strings,
    // but works for our migration SQL.
    var stmts []string
    for _, s := range strings.Split(content, ";") {
        s = strings.TrimSpace(s)
        if s != "" {
            stmts = append(stmts, s)
        }
    }
    return stmts
}
```

Note: `splitStatements` splits on bare semicolons. The trigger SQL in migration 001 contains `END;` which will need special handling. The trigger bodies use `BEGIN ... END` blocks — the simplest approach is to split on `;\n` (semicolon followed by newline) instead of just `;`. Adjust the implementation to handle this correctly for the triggers in `001_initial_schema.sql`.

**Step 5: Update db.go to use migrator**

In `internal/db/db.go`, replace the `init()` method (lines 60-88):

```go
func (db *DB) init() error {
    return RunMigrations(db.DB.DB) // pass the underlying *sql.DB
}
```

Keep `schema.sql` as a reference file but it's no longer executed directly.

**Step 6: Run tests to verify they pass**

Run: `go test ./internal/db/ -run TestMigrator -v`
Expected: PASS

**Step 7: Run full test suite**

Run: `make test`
Expected: All tests pass — existing functionality unchanged

**Step 8: Commit**

```bash
git add internal/db/migrations/ internal/db/migrator.go internal/db/migrator_test.go internal/db/db.go
git commit -m "feat: add versioned migration system with bootstrap detection"
```

---

### Task 2: Source Adapter Interface and Registry

**Files:**
- Create: `pkg/adapter/adapter.go` (interface + types)
- Create: `pkg/adapter/registry.go` (adapter registry)
- Test: `pkg/adapter/registry_test.go`

**Step 1: Write the failing test**

```go
// pkg/adapter/registry_test.go
package adapter

import (
    "testing"
    "time"
)

// stubAdapter is a minimal adapter for testing the registry
type stubAdapter struct{}

func (s *stubAdapter) Name() string { return "stub" }
func (s *stubAdapter) Discover(root string) ([]SessionFile, error) {
    return []SessionFile{{Path: root + "/test.jsonl", ModTime: time.Now()}}, nil
}
func (s *stubAdapter) Parse(path string) (*ParsedSession, error) {
    return &ParsedSession{ID: "test-id", SourceName: "stub"}, nil
}

func TestRegistry_RegisterAndGet(t *testing.T) {
    Register("stub", func() SourceAdapter { return &stubAdapter{} })

    adpt, err := Get("stub")
    if err != nil {
        t.Fatalf("Get failed: %v", err)
    }
    if adpt.Name() != "stub" {
        t.Errorf("expected name 'stub', got %q", adpt.Name())
    }
}

func TestRegistry_GetUnknown(t *testing.T) {
    _, err := Get("nonexistent")
    if err == nil {
        t.Fatal("expected error for unknown adapter")
    }
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./pkg/adapter/ -run TestRegistry -v`
Expected: FAIL — package doesn't exist

**Step 3: Write the adapter interface and registry**

```go
// pkg/adapter/adapter.go
package adapter

import (
    "encoding/json"
    "time"
)

// SessionFile represents a discovered session file from a source.
type SessionFile struct {
    Path        string
    ProjectPath string
    ModTime     time.Time
}

// ParsedSession is the normalized output of parsing a session file.
type ParsedSession struct {
    ID          string
    ProjectPath string
    Turns       []ParsedTurn
    Model       string
    GitBranch   string
    StartedAt   time.Time
    EndedAt     time.Time
    SourceName  string
    Metadata    map[string]any
}

// ParsedTurn is a single normalized turn within a session.
type ParsedTurn struct {
    ID           string
    ParentID     string
    Type         string // "user", "assistant", "system"
    Timestamp    time.Time
    Content      string
    RawJSON      json.RawMessage
    InputTokens  int64
    OutputTokens int64
    ToolUses     []ParsedToolUse
    HasError     bool
}

// ParsedToolUse represents a tool invocation within a turn.
type ParsedToolUse struct {
    ToolName string
    FilePath string
}

// SourceAdapter defines the interface for discovering and parsing sessions from an AI tool.
type SourceAdapter interface {
    Name() string
    Discover(root string) ([]SessionFile, error)
    Parse(path string) (*ParsedSession, error)
}
```

```go
// pkg/adapter/registry.go
package adapter

import (
    "fmt"
    "sync"
)

// AdapterFactory creates a new instance of a SourceAdapter.
type AdapterFactory func() SourceAdapter

var (
    mu       sync.RWMutex
    registry = map[string]AdapterFactory{}
)

// Register adds an adapter factory to the registry.
func Register(typeName string, factory AdapterFactory) {
    mu.Lock()
    defer mu.Unlock()
    registry[typeName] = factory
}

// Get returns a new adapter instance for the given type name.
func Get(typeName string) (SourceAdapter, error) {
    mu.RLock()
    defer mu.RUnlock()
    factory, ok := registry[typeName]
    if !ok {
        return nil, fmt.Errorf("unknown adapter type: %q", typeName)
    }
    return factory(), nil
}
```

**Step 4: Run test to verify it passes**

Run: `go test ./pkg/adapter/ -run TestRegistry -v`
Expected: PASS

**Step 5: Commit**

```bash
git add pkg/adapter/
git commit -m "feat: add source adapter interface and registry"
```

---

### Task 3: Claude Code Adapter

**Files:**
- Create: `pkg/adapter/claudecode/claudecode.go`
- Test: `pkg/adapter/claudecode/claudecode_test.go`
- Modify: `pkg/parser/parser.go` — no changes needed, adapter wraps existing functions
- Modify: `pkg/parser/scanner.go` — no changes needed, adapter wraps existing functions

**Step 1: Write the failing test**

```go
// pkg/adapter/claudecode/claudecode_test.go
package claudecode

import (
    "os"
    "path/filepath"
    "testing"

    "github.com/anthropics/ccvault/pkg/adapter"
)

func TestClaudeCodeAdapter_Name(t *testing.T) {
    a := New()
    if a.Name() != "claude-code" {
        t.Errorf("expected name 'claude-code', got %q", a.Name())
    }
}

func TestClaudeCodeAdapter_Discover(t *testing.T) {
    // Create a minimal Claude Code directory structure
    tmpDir := t.TempDir()
    projectDir := filepath.Join(tmpDir, "projects", "-Users-test-myproject")
    os.MkdirAll(projectDir, 0o755)

    // Create a fake session file
    sessionFile := filepath.Join(projectDir, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee.jsonl")
    os.WriteFile(sessionFile, []byte(`{"type":"summary"}`+"\n"), 0o644)

    a := New()
    files, err := a.Discover(tmpDir)
    if err != nil {
        t.Fatalf("Discover failed: %v", err)
    }
    if len(files) == 0 {
        t.Fatal("expected at least 1 session file")
    }
}

func TestClaudeCodeAdapter_ImplementsInterface(t *testing.T) {
    var _ adapter.SourceAdapter = New()
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./pkg/adapter/claudecode/ -run TestClaudeCode -v`
Expected: FAIL — package doesn't exist

**Step 3: Write the Claude Code adapter**

```go
// pkg/adapter/claudecode/claudecode.go
package claudecode

import (
    "encoding/json"

    "github.com/anthropics/ccvault/pkg/adapter"
    "github.com/anthropics/ccvault/pkg/parser"
)

func init() {
    adapter.Register("claude-code", func() adapter.SourceAdapter { return New() })
}

// Adapter wraps the existing parser and scanner for Claude Code sessions.
type Adapter struct{}

// New creates a Claude Code source adapter.
func New() *Adapter {
    return &Adapter{}
}

func (a *Adapter) Name() string { return "claude-code" }

func (a *Adapter) Discover(root string) ([]adapter.SessionFile, error) {
    scannerFiles, err := parser.ScanClaudeHome(root)
    if err != nil {
        return nil, err
    }
    files := make([]adapter.SessionFile, len(scannerFiles))
    for i, sf := range scannerFiles {
        files[i] = adapter.SessionFile{
            Path:        sf.Path,
            ProjectPath: sf.ProjectPath,
            ModTime:     sf.ModTime,
        }
    }
    return files, nil
}

func (a *Adapter) Parse(path string) (*adapter.ParsedSession, error) {
    turns, session, err := parser.ParseSession(path)
    if err != nil {
        return nil, err
    }

    parsed := &adapter.ParsedSession{
        ID:          session.ID,
        ProjectPath: session.ProjectPath,
        Model:       session.Model,
        GitBranch:   session.GitBranch,
        StartedAt:   session.StartedAt,
        EndedAt:     session.EndedAt,
        Metadata:    make(map[string]any),
    }

    for _, turn := range turns {
        toolUses := convertToolUses(turn)
        hasError := detectError(turn)

        pt := adapter.ParsedTurn{
            ID:           turn.ID,
            ParentID:     turn.ParentID,
            Type:         turn.Type,
            Timestamp:    turn.Timestamp,
            Content:      turn.Content,
            RawJSON:      json.RawMessage(turn.RawJSON),
            InputTokens:  turn.InputTokens,
            OutputTokens: turn.OutputTokens,
            ToolUses:     toolUses,
            HasError:     hasError,
        }
        parsed.Turns = append(parsed.Turns, pt)
    }

    return parsed, nil
}

// convertToolUses extracts tool uses from a turn using the existing parser.
func convertToolUses(turn models.Turn) []adapter.ParsedToolUse {
    toolUses := parser.ExtractToolUses(turn)
    result := make([]adapter.ParsedToolUse, len(toolUses))
    for i, tu := range toolUses {
        result[i] = adapter.ParsedToolUse{
            ToolName: tu.ToolName,
            FilePath: tu.FilePath,
        }
    }
    return result
}

// detectError checks for error markers in raw JSON.
func detectError(turn models.Turn) bool {
    return strings.Contains(turn.RawJSON, `"is_error":true`) ||
        strings.Contains(turn.RawJSON, `"is_error": true`)
}
```

Note: The exact implementation will need to import `models` and adjust based on how `parser.ParseSession` returns data. The existing `parser.ParseSession` (line 19 of parser.go) returns `([]models.Turn, models.Session, error)` — adapt accordingly. Also move `detectSubagents` logic here: check if any tool use has `ToolName == "Task"` and set it as metadata `parsed.Metadata["has_subagent"] = true`.

**Step 4: Run test to verify it passes**

Run: `go test ./pkg/adapter/claudecode/ -run TestClaudeCode -v`
Expected: PASS

**Step 5: Commit**

```bash
git add pkg/adapter/claudecode/
git commit -m "feat: add Claude Code source adapter wrapping existing parser"
```

---

### Task 4: Multi-Source Config

**Files:**
- Modify: `internal/config/config.go:14-62`
- Test: `internal/config/config_test.go` (create or modify)

**Step 1: Write the failing test**

```go
// internal/config/config_test.go
package config

import (
    "os"
    "path/filepath"
    "testing"
)

func TestLoad_MultiSource(t *testing.T) {
    tmpDir := t.TempDir()
    configFile := filepath.Join(tmpDir, "config.yaml")
    os.WriteFile(configFile, []byte(`
data_dir: /tmp/ccvault-test
sources:
  - name: claude-code
    type: claude-code
    path: ~/.claude
  - name: codex
    type: codex
    path: ~/.codex
`), 0o644)

    cfg, err := LoadFrom(configFile)
    if err != nil {
        t.Fatalf("LoadFrom failed: %v", err)
    }
    if len(cfg.Sources) != 2 {
        t.Fatalf("expected 2 sources, got %d", len(cfg.Sources))
    }
    if cfg.Sources[0].Name != "claude-code" {
        t.Errorf("expected first source name 'claude-code', got %q", cfg.Sources[0].Name)
    }
    if cfg.Sources[1].Type != "codex" {
        t.Errorf("expected second source type 'codex', got %q", cfg.Sources[1].Type)
    }
}

func TestLoad_BackwardCompat(t *testing.T) {
    tmpDir := t.TempDir()
    configFile := filepath.Join(tmpDir, "config.yaml")
    os.WriteFile(configFile, []byte(`
claude_home: ~/.claude
data_dir: /tmp/ccvault-test
`), 0o644)

    cfg, err := LoadFrom(configFile)
    if err != nil {
        t.Fatalf("LoadFrom failed: %v", err)
    }
    // Should auto-create a single claude-code source
    if len(cfg.Sources) != 1 {
        t.Fatalf("expected 1 source from backward compat, got %d", len(cfg.Sources))
    }
    if cfg.Sources[0].Type != "claude-code" {
        t.Errorf("expected type 'claude-code', got %q", cfg.Sources[0].Type)
    }
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestLoad -v`
Expected: FAIL — `Sources` field and `LoadFrom` not defined

**Step 3: Update config struct and loader**

Add `SourceConfig` struct and `Sources` field to `Config`. Update `Load` to populate `Sources` from either the new `sources` key or the legacy `claude_home` key. Add `LoadFrom` that accepts an explicit config file path (for testing).

Key additions to `internal/config/config.go`:
```go
type SourceConfig struct {
    Name string `mapstructure:"name"`
    Type string `mapstructure:"type"`
    Path string `mapstructure:"path"`
}

type Config struct {
    ClaudeHome string         `mapstructure:"claude_home"` // legacy, kept for backward compat
    DataDir    string         `mapstructure:"data_dir"`
    Sources    []SourceConfig `mapstructure:"sources"`
}
```

In `Load()`, after loading, if `cfg.Sources` is empty and `cfg.ClaudeHome` is set, create a default source:
```go
if len(cfg.Sources) == 0 {
    cfg.Sources = []SourceConfig{{
        Name: "claude-code",
        Type: "claude-code",
        Path: cfg.ClaudeHome,
    }}
}
```

**Step 4: Run test to verify it passes**

Run: `go test ./internal/config/ -run TestLoad -v`
Expected: PASS

**Step 5: Commit**

```bash
git add internal/config/
git commit -m "feat: add multi-source config with backward compatibility"
```

---

### Task 5: Add Source Column (Migration 003)

**Files:**
- Create: `internal/db/migrations/003_add_source_columns.sql`
- Modify: `internal/db/migrator_test.go` (add test for migration 003)
- Modify: `pkg/models/models.go` (add Source field to Session, Project)

**Step 1: Write the failing test**

Add to `internal/db/migrator_test.go`:
```go
func TestMigrator_SourceColumns(t *testing.T) {
    db, err := sql.Open("sqlite3", ":memory:")
    if err != nil {
        t.Fatal(err)
    }
    defer db.Close()

    err = RunMigrations(db)
    if err != nil {
        t.Fatalf("RunMigrations failed: %v", err)
    }

    // Verify source column exists with default value
    _, err = db.Exec("INSERT INTO projects (id, path, display_name) VALUES ('p1', '/test', 'test')")
    if err != nil {
        t.Fatal(err)
    }
    var source string
    err = db.QueryRow("SELECT source FROM projects WHERE id = 'p1'").Scan(&source)
    if err != nil {
        t.Fatalf("query source: %v", err)
    }
    if source != "claude-code" {
        t.Errorf("expected default source 'claude-code', got %q", source)
    }
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/db/ -run TestMigrator_SourceColumns -v`
Expected: FAIL — no `source` column

**Step 3: Create migration 003**

```sql
-- internal/db/migrations/003_add_source_columns.sql
ALTER TABLE sessions ADD COLUMN source TEXT NOT NULL DEFAULT 'claude-code';
ALTER TABLE projects ADD COLUMN source TEXT NOT NULL DEFAULT 'claude-code';
ALTER TABLE source_files ADD COLUMN source TEXT NOT NULL DEFAULT 'claude-code';
CREATE INDEX IF NOT EXISTS idx_sessions_source ON sessions(source);
CREATE INDEX IF NOT EXISTS idx_projects_source ON projects(source);
```

**Step 4: Add Source field to models**

In `pkg/models/models.go`, add `Source string` to `Project` (after line 20), `Session` (after line 44), and any relevant query structs.

**Step 5: Run test to verify it passes**

Run: `go test ./internal/db/ -run TestMigrator -v`
Expected: All migrator tests PASS

**Step 6: Commit**

```bash
git add internal/db/migrations/003_add_source_columns.sql internal/db/migrator_test.go pkg/models/models.go
git commit -m "feat: add source column to sessions, projects, source_files"
```

---

### Task 6: Update DB Operations for Source

**Files:**
- Modify: `internal/db/projects.go:16-75` (UpsertProject — include source)
- Modify: `internal/db/sessions.go:15-96` (UpsertSession — include source)
- Modify: `internal/db/sessions.go:262-279` (GetAllSourceMtimes — include source)

**Step 1: Write failing tests**

Add tests that insert sessions/projects with a source field and verify it's stored and queryable. Test that `GetAllSourceMtimes` returns source-scoped results.

**Step 2: Run tests to verify they fail**

Run: `go test ./internal/db/ -v`
Expected: FAIL on new source-aware tests

**Step 3: Update UpsertProject, UpsertSession, GetAllSourceMtimes**

Add `source` parameter to upsert functions. Update SQL to include the source column in INSERT and SELECT statements. Update `GetAllSourceMtimes` to accept an optional source filter.

**Step 4: Run tests to verify they pass**

Run: `go test ./internal/db/ -v`
Expected: PASS

**Step 5: Run full test suite**

Run: `make test`
Expected: All tests pass (callers updated in next task)

**Step 6: Commit**

```bash
git add internal/db/projects.go internal/db/sessions.go
git commit -m "feat: thread source column through DB operations"
```

---

### Task 7: Refactor Sync to Use Adapters

**Files:**
- Modify: `internal/sync/sync.go:30-37` (Syncer struct — accept sources + adapters)
- Modify: `internal/sync/sync.go:71-82` (New — accept config sources)
- Modify: `internal/sync/sync.go:85-139` (Run — iterate over sources)
- Modify: `internal/sync/sync.go:142-242` (processSession — accept source name)
- Modify: `internal/sync/sync.go:260-281` (move detectErrors/detectSubagents to adapter)

**Step 1: Write failing tests**

Update existing sync tests (if any) or write new ones that create a Syncer with a stub adapter and verify it discovers + parses + inserts correctly with source tagging.

**Step 2: Run tests to verify they fail**

Run: `go test ./internal/sync/ -v`

**Step 3: Refactor Syncer**

Update the `Syncer` struct to hold `[]config.SourceConfig` instead of `claudeHome string`. Update `Run()` to:
1. Iterate over configured sources
2. Call `adapter.Get(src.Type)` for each source
3. Call `adapter.Discover(src.Path)` to find session files
4. Call `adapter.Parse(path)` for each file
5. Set `SourceName` on the parsed session
6. Pass source through to DB upserts

Remove `detectErrors` and `detectSubagents` from sync — these now live in the Claude Code adapter.

**Step 4: Run tests to verify they pass**

Run: `go test ./internal/sync/ -v`
Expected: PASS

**Step 5: Commit**

```bash
git add internal/sync/sync.go
git commit -m "refactor: sync layer uses adapter interface for multi-source support"
```

---

### Task 8: Add source: Search Filter

**Files:**
- Modify: `internal/search/query.go:13-23` (add Source field to Query)
- Modify: `internal/search/query.go:26-65` (parse `source:` operator)
- Modify: `internal/search/search.go:77-165` (add source filter to buildQuery)
- Test: `internal/search/query_test.go` or `internal/search/search_test.go`

**Step 1: Write failing test**

```go
func TestParse_SourceFilter(t *testing.T) {
    q := Parse("source:codex auth middleware")
    if q.Source != "codex" {
        t.Errorf("expected source 'codex', got %q", q.Source)
    }
    if q.Text != "auth middleware" {
        t.Errorf("expected text 'auth middleware', got %q", q.Text)
    }
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/search/ -run TestParse_SourceFilter -v`
Expected: FAIL — `Source` field not defined

**Step 3: Add source to Query and buildQuery**

Add `Source string` to `Query` struct. Add `"source"` case to the operator parsing switch in `Parse()`. Add SQL WHERE clause for source in `buildQuery()`:
```go
if q.Source != "" {
    conditions = append(conditions, "s.source = ?")
    args = append(args, q.Source)
}
```

**Step 4: Run test to verify it passes**

Run: `go test ./internal/search/ -v`
Expected: PASS

**Step 5: Commit**

```bash
git add internal/search/
git commit -m "feat: add source: filter to search queries"
```

---

### Task 9: Wire Up Main CLI

**Files:**
- Modify: `cmd/ccvault/main.go:266-326` (syncCmd — use config sources)
- Modify: `cmd/ccvault/main.go` (import adapter packages for init registration)

**Step 1: Update imports**

Add blank import for the Claude Code adapter so its `init()` runs:
```go
import (
    _ "github.com/anthropics/ccvault/pkg/adapter/claudecode"
)
```

**Step 2: Update syncCmd**

Replace the current `sync.New(db, cfg.ClaudeHome, ...)` call to pass `cfg.Sources` instead. The Syncer now iterates over sources internally.

**Step 3: Run full test suite**

Run: `make test`
Expected: All tests pass

**Step 4: Manual smoke test**

Run: `make build && ./ccvault sync`
Expected: Syncs from configured sources (default: Claude Code at ~/.claude)

**Step 5: Commit**

```bash
git add cmd/ccvault/main.go
git commit -m "feat: wire multi-source sync into CLI"
```

---

### Task 10: Codex Adapter (Stub)

**Files:**
- Create: `pkg/adapter/codex/codex.go`
- Test: `pkg/adapter/codex/codex_test.go`

**Step 1: Research Codex session format**

Investigate `~/.codex/` directory structure and session file format. Document the JSONL schema differences from Claude Code.

**Step 2: Write failing test**

Write tests for Discover and Parse based on the Codex format discovered in Step 1.

**Step 3: Implement the Codex adapter**

Implement `Discover()` and `Parse()` for Codex's specific directory layout and JSONL schema.

**Step 4: Run tests**

Run: `go test ./pkg/adapter/codex/ -v`
Expected: PASS

**Step 5: Add blank import to main.go**

```go
import (
    _ "github.com/anthropics/ccvault/pkg/adapter/claudecode"
    _ "github.com/anthropics/ccvault/pkg/adapter/codex"
)
```

**Step 6: Commit**

```bash
git add pkg/adapter/codex/ cmd/ccvault/main.go
git commit -m "feat: add Codex source adapter"
```

---

### Task 11: End-to-End Integration Test

**Files:**
- Create: `test/integration/multisource_test.go`

**Step 1: Write integration test**

Create a test that:
1. Sets up temp directories with fake Claude Code and Codex session files
2. Creates a multi-source config pointing to both
3. Runs a full sync
4. Verifies sessions from both sources are in the DB with correct `source` values
5. Searches with `source:` filter and verifies results
6. Searches without filter and verifies both sources appear

**Step 2: Run test**

Run: `go test ./test/integration/ -run TestMultiSource -v`
Expected: PASS

**Step 3: Run full test suite**

Run: `make test`
Expected: All tests pass

**Step 4: Commit**

```bash
git add test/integration/
git commit -m "test: add multi-source integration test"
```
