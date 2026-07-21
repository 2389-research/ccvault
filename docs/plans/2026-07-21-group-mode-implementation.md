# Group Mode Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add a git-remote-style "group mode" to ccvault: developers push their local vault to a shared remote `ccvaultd` server over SSH, and query the remote's data (search, list-sessions, show, stats) via the same client CLI. One-way push, closed-trust team, no client-side filtering.

**Architecture:** New `cmd/ccvaultd` binary in the same repo, sharing DB schema, models, and search with the client. Transport is SSH via `golang.org/x/crypto/ssh` on both sides. Client tracks `(remote, session_id, session_ended_at, pushed_at)` in a new `remote_push_state` table for incremental push. Server adds a `pushed_by` column to `sessions` populated from the SSH pubkey identity. Wire protocol is newline-delimited JSON over an SSH channel.

**Tech Stack:** Go 1.25, `modernc.org/sqlite`, `golang.org/x/crypto/ssh` (new), Cobra for CLI, existing viper-based config.

**Full design:** See `docs/plans/2026-07-21-group-mode-design.md`.

---

## Working conventions

- All work happens on branch `feature/group-mode` (already created and checked out).
- Commit after each task. Commit messages: conventional-commit style (`feat:`, `test:`, `chore:`) matching existing history.
- Every task follows TDD: write failing test → verify failure → implement minimum → verify pass → commit.
- Run the full test suite (`make test` or `go test ./...`) at the end of every implementation task before committing.
- Never mock the DB or network — the project's standing rule is real deps only. Integration tests spin up real SQLite tempfiles and, for server tests, real `net.Listen` on a random port with real SSH.
- ABOUTME comments on every new file (two lines, both starting with `// ABOUTME: ` for Go or `-- ABOUTME: ` for SQL).

---

## Phase 0: Dependencies

### Task 1: Add golang.org/x/crypto dependency

**Files:**
- Modify: `go.mod`
- Modify: `go.sum`

**Step 1: Add the dep**

Run: `go get golang.org/x/crypto@latest`

Expected: `go.mod` gets a new require line for `golang.org/x/crypto`, `go.sum` gets updated.

**Step 2: Sanity check it resolves**

Create a throwaway file `internal/remote/tmp_probe_test.go`:

```go
package remote

import (
    "testing"

    _ "golang.org/x/crypto/ssh"
    _ "golang.org/x/crypto/ssh/agent"
)

func TestCryptoResolvable(t *testing.T) {
    t.Log("golang.org/x/crypto/ssh and .../agent import OK")
}
```

Run: `go test ./internal/remote/...`
Expected: PASS.

**Step 3: Delete the probe file, keep the go.mod/go.sum changes**

Run: `rm internal/remote/tmp_probe_test.go`

**Step 4: Commit**

```bash
git add go.mod go.sum
git commit -m "chore: add golang.org/x/crypto dependency for SSH transport"
```

---

## Phase 1: Database migrations

### Task 2: Migration 005 — `remote_push_state` table

**Files:**
- Create: `internal/db/migrations/005_remote_push_state.sql`
- Test: `internal/db/migrator_test.go` (add a case)

**Step 1: Write the failing test**

Add to `internal/db/migrator_test.go`:

```go
func TestMigration005CreatesRemotePushState(t *testing.T) {
    db := openTestDB(t)
    defer db.Close()

    // migrator ran on Open — the table should exist and be empty
    var count int
    err := db.QueryRow("SELECT COUNT(*) FROM remote_push_state").Scan(&count)
    if err != nil {
        t.Fatalf("query remote_push_state: %v", err)
    }
    if count != 0 {
        t.Fatalf("expected empty table, got %d rows", count)
    }

    // Insert a row and verify the composite PK is enforced
    _, err = db.Exec(`
        INSERT INTO remote_push_state (remote_name, session_id, session_ended_at, pushed_at)
        VALUES ('origin', 'sess-1', '2026-07-21T10:00:00Z', '2026-07-21T10:00:01Z')
    `)
    if err != nil {
        t.Fatalf("insert row: %v", err)
    }

    _, err = db.Exec(`
        INSERT INTO remote_push_state (remote_name, session_id, session_ended_at, pushed_at)
        VALUES ('origin', 'sess-1', '2026-07-21T11:00:00Z', '2026-07-21T11:00:01Z')
    `)
    if err == nil {
        t.Fatal("expected PK conflict on duplicate (remote_name, session_id)")
    }
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/db/... -run TestMigration005CreatesRemotePushState -v`
Expected: FAIL with `no such table: remote_push_state`.

**Step 3: Write the migration**

Create `internal/db/migrations/005_remote_push_state.sql`:

```sql
-- ABOUTME: Add remote_push_state to track per-remote incremental push watermarks
-- ABOUTME: Composite PK (remote_name, session_id); enables "push what's changed" semantics

CREATE TABLE remote_push_state (
    remote_name       TEXT NOT NULL,
    session_id        TEXT NOT NULL,
    session_ended_at  DATETIME NOT NULL,
    pushed_at         DATETIME NOT NULL,
    PRIMARY KEY (remote_name, session_id)
);

CREATE INDEX idx_remote_push_state_pushed_at
    ON remote_push_state(remote_name, pushed_at);
```

**Step 4: Run test to verify it passes**

Run: `go test ./internal/db/... -run TestMigration005CreatesRemotePushState -v`
Expected: PASS.

**Step 5: Run full db suite**

Run: `go test ./internal/db/...`
Expected: PASS.

**Step 6: Commit**

```bash
git add internal/db/migrations/005_remote_push_state.sql internal/db/migrator_test.go
git commit -m "feat: add migration 005 for remote_push_state table"
```

### Task 3: Migration 006 — `pushed_by` column on `sessions`

**Files:**
- Create: `internal/db/migrations/006_add_pushed_by.sql`
- Test: `internal/db/migrator_test.go` (add a case)

**Step 1: Write the failing test**

Add to `internal/db/migrator_test.go`:

```go
func TestMigration006AddsPushedByColumn(t *testing.T) {
    db := openTestDB(t)
    defer db.Close()

    rows, err := db.Query("PRAGMA table_info(sessions)")
    if err != nil {
        t.Fatalf("pragma: %v", err)
    }
    defer rows.Close()

    var found bool
    for rows.Next() {
        var cid int
        var name, colType string
        var notNull int
        var dflt sql.NullString
        var pk int
        if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
            t.Fatal(err)
        }
        if name == "pushed_by" {
            found = true
            if colType != "TEXT" {
                t.Errorf("pushed_by type = %q, want TEXT", colType)
            }
            if notNull != 1 {
                t.Errorf("pushed_by should be NOT NULL")
            }
        }
    }
    if !found {
        t.Fatal("sessions.pushed_by column not found after migrations")
    }
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/db/... -run TestMigration006AddsPushedByColumn -v`
Expected: FAIL with "sessions.pushed_by column not found".

**Step 3: Write the migration**

Create `internal/db/migrations/006_add_pushed_by.sql`:

```sql
-- ABOUTME: Add pushed_by column to sessions for per-user attribution on remote vaults
-- ABOUTME: Populated by ccvaultd from the SSH pubkey identity; empty string on local vaults

ALTER TABLE sessions ADD COLUMN pushed_by TEXT NOT NULL DEFAULT '';
```

**Step 4: Run test to verify it passes**

Run: `go test ./internal/db/... -run TestMigration006AddsPushedByColumn -v`
Expected: PASS.

**Step 5: Update `Session` model**

Modify `pkg/models/models.go` to add the field (following the pattern of `Source`):

```go
Source    string `json:"source"`
PushedBy  string `json:"pushed_by,omitempty"` // Empty on local; set by ccvaultd from SSH identity
```

**Step 6: Run full DB and models tests**

Run: `go test ./internal/db/... ./pkg/models/...`
Expected: PASS.

**Step 7: Commit**

```bash
git add internal/db/migrations/006_add_pushed_by.sql internal/db/migrator_test.go pkg/models/models.go
git commit -m "feat: add pushed_by column and Session field for remote attribution"
```

---

## Phase 2: Config — remotes

### Task 4: Add `Remote` and `Remotes` to Config

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

**Step 1: Write the failing test**

Add to `internal/config/config_test.go`:

```go
func TestConfigLoadsRemotes(t *testing.T) {
    dir := t.TempDir()
    cfgPath := filepath.Join(dir, "config.toml")
    contents := `
data_dir = "` + dir + `"

[[sources]]
name = "claude-code"
type = "claude-code"
path = "/tmp/whatever"

[remotes.origin]
url = "ccvault@vault.company.com"

[remotes.backup]
url = "ssh://backup.example.com:2222/team"
`
    if err := os.WriteFile(cfgPath, []byte(contents), 0644); err != nil {
        t.Fatal(err)
    }

    cfg, err := LoadFrom(cfgPath)
    if err != nil {
        t.Fatalf("load: %v", err)
    }

    if len(cfg.Remotes) != 2 {
        t.Fatalf("expected 2 remotes, got %d", len(cfg.Remotes))
    }
    if cfg.Remotes["origin"].URL != "ccvault@vault.company.com" {
        t.Errorf("origin URL = %q", cfg.Remotes["origin"].URL)
    }
    if cfg.Remotes["backup"].URL != "ssh://backup.example.com:2222/team" {
        t.Errorf("backup URL = %q", cfg.Remotes["backup"].URL)
    }
}

func TestConfigRejectsRemoteWithEmptyURL(t *testing.T) {
    dir := t.TempDir()
    cfgPath := filepath.Join(dir, "config.toml")
    contents := `
data_dir = "` + dir + `"

[remotes.origin]
url = ""
`
    if err := os.WriteFile(cfgPath, []byte(contents), 0644); err != nil {
        t.Fatal(err)
    }

    _, err := LoadFrom(cfgPath)
    if err == nil {
        t.Fatal("expected error for empty URL")
    }
    if !strings.Contains(err.Error(), "url") {
        t.Errorf("error should mention url, got %v", err)
    }
}
```

**Step 2: Run tests to verify they fail**

Run: `go test ./internal/config/... -run TestConfigLoadsRemotes -v`
Expected: FAIL — `Remotes` field doesn't exist.

**Step 3: Implement Remote struct**

Modify `internal/config/config.go` — add near `SourceConfig`:

```go
// Remote describes a remote ccvault vault (a `ccvaultd` server).
// Keyed by name in Config.Remotes (which maps to TOML `[remotes.<name>]`).
type Remote struct {
    URL string `mapstructure:"url"`
}
```

Modify `Config`:

```go
type Config struct {
    ClaudeHome string            `mapstructure:"claude_home"`
    DataDir    string            `mapstructure:"data_dir"`
    Sources    []SourceConfig    `mapstructure:"sources"`
    Remotes    map[string]Remote `mapstructure:"remotes"`
}
```

Add validation to `unmarshalAndApplyDefaults` (right before `return &cfg, nil`):

```go
if err := validateRemotes(cfg.Remotes); err != nil {
    return nil, err
}
```

Add the validator:

```go
func validateRemotes(remotes map[string]Remote) error {
    for name, r := range remotes {
        if strings.TrimSpace(name) == "" {
            return fmt.Errorf("remote name must not be empty")
        }
        if strings.TrimSpace(r.URL) == "" {
            return fmt.Errorf("remote %q: url is required", name)
        }
    }
    return nil
}
```

**Step 4: Run tests to verify they pass**

Run: `go test ./internal/config/... -v`
Expected: PASS.

**Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat: add [remotes.<name>] config support"
```

---

## Phase 3: DB layer for `remote_push_state`

### Task 5: `internal/db/remotes.go` — CRUD helpers

**Files:**
- Create: `internal/db/remotes.go`
- Test: `internal/db/remotes_test.go`

**Step 1: Write failing tests**

Create `internal/db/remotes_test.go`:

```go
// ABOUTME: Tests for remote_push_state CRUD helpers
// ABOUTME: Exercises incremental push watermark logic

package db

import (
    "testing"
    "time"
)

func TestSessionsPendingPushIncludesUntrackedSessions(t *testing.T) {
    d := openTestDB(t)
    defer d.Close()

    seedSession(t, d, "sess-a", "2026-07-21T10:00:00Z")

    ids, err := d.SessionsPendingPush("origin")
    if err != nil {
        t.Fatal(err)
    }
    if len(ids) != 1 || ids[0] != "sess-a" {
        t.Fatalf("want [sess-a], got %v", ids)
    }
}

func TestSessionsPendingPushExcludesAlreadyPushed(t *testing.T) {
    d := openTestDB(t)
    defer d.Close()

    seedSession(t, d, "sess-a", "2026-07-21T10:00:00Z")

    ended, _ := time.Parse(time.RFC3339, "2026-07-21T10:00:00Z")
    if err := d.RecordPush("origin", "sess-a", ended); err != nil {
        t.Fatal(err)
    }

    ids, _ := d.SessionsPendingPush("origin")
    if len(ids) != 0 {
        t.Fatalf("want empty, got %v", ids)
    }
}

func TestSessionsPendingPushIncludesUpdatedSessions(t *testing.T) {
    d := openTestDB(t)
    defer d.Close()

    seedSession(t, d, "sess-a", "2026-07-21T10:00:00Z")

    ended, _ := time.Parse(time.RFC3339, "2026-07-21T10:00:00Z")
    if err := d.RecordPush("origin", "sess-a", ended); err != nil {
        t.Fatal(err)
    }

    // Update the session's ended_at to a later time
    _, err := d.Exec("UPDATE sessions SET ended_at = ? WHERE id = ?",
        "2026-07-21T11:00:00Z", "sess-a")
    if err != nil {
        t.Fatal(err)
    }

    ids, _ := d.SessionsPendingPush("origin")
    if len(ids) != 1 {
        t.Fatalf("want 1 pending session after update, got %v", ids)
    }
}

// seedSession inserts a minimal project + session pair for testing.
func seedSession(t *testing.T, d *DB, id, endedAt string) {
    t.Helper()
    _, err := d.Exec(`INSERT INTO projects (path, display_name, first_seen_at, last_activity_at, session_count, total_tokens, source)
        VALUES ('/tmp/p', 'p', ?, ?, 1, 0, 'claude-code')`, endedAt, endedAt)
    if err != nil {
        t.Fatal(err)
    }
    var pid int64
    if err := d.QueryRow("SELECT last_insert_rowid()").Scan(&pid); err != nil {
        t.Fatal(err)
    }
    _, err = d.Exec(`INSERT INTO sessions (id, project_id, started_at, ended_at, turn_count, input_tokens, output_tokens, source_file, source, pushed_by)
        VALUES (?, ?, ?, ?, 0, 0, 0, '/tmp/p/x.jsonl', 'claude-code', '')`,
        id, pid, endedAt, endedAt)
    if err != nil {
        t.Fatal(err)
    }
}
```

**Step 2: Run tests to verify they fail**

Run: `go test ./internal/db/... -run TestSessionsPendingPush -v`
Expected: FAIL — methods don't exist.

**Step 3: Implement**

Create `internal/db/remotes.go`:

```go
// ABOUTME: CRUD helpers for remote_push_state — the client's incremental push watermark
// ABOUTME: SessionsPendingPush returns sessions never pushed or updated since last push

package db

import (
    "fmt"
    "time"
)

// SessionsPendingPush returns session IDs that need to be pushed to the given remote.
// A session is pending if either (a) it has no row in remote_push_state, or
// (b) its current ended_at is later than the recorded session_ended_at.
func (db *DB) SessionsPendingPush(remoteName string) ([]string, error) {
    rows, err := db.Query(`
        SELECT s.id
        FROM sessions s
        LEFT JOIN remote_push_state r
          ON r.remote_name = ? AND r.session_id = s.id
        WHERE r.session_id IS NULL
           OR s.ended_at > r.session_ended_at
        ORDER BY s.started_at
    `, remoteName)
    if err != nil {
        return nil, fmt.Errorf("query pending: %w", err)
    }
    defer rows.Close()

    var ids []string
    for rows.Next() {
        var id string
        if err := rows.Scan(&id); err != nil {
            return nil, err
        }
        ids = append(ids, id)
    }
    return ids, rows.Err()
}

// RecordPush upserts a remote_push_state row after a successful push.
func (db *DB) RecordPush(remoteName, sessionID string, sessionEndedAt time.Time) error {
    _, err := db.Exec(`
        INSERT INTO remote_push_state (remote_name, session_id, session_ended_at, pushed_at)
        VALUES (?, ?, ?, ?)
        ON CONFLICT(remote_name, session_id) DO UPDATE SET
            session_ended_at = excluded.session_ended_at,
            pushed_at        = excluded.pushed_at
    `, remoteName, sessionID, sessionEndedAt, time.Now().UTC())
    return err
}
```

**Step 4: Run tests to verify they pass**

Run: `go test ./internal/db/... -run TestSessionsPendingPush -v`
Expected: PASS.

**Step 5: Commit**

```bash
git add internal/db/remotes.go internal/db/remotes_test.go
git commit -m "feat: add remote_push_state CRUD helpers (SessionsPendingPush, RecordPush)"
```

---

## Phase 4: Wire protocol

### Task 6: `internal/remote/protocol` — ndjson message types

**Files:**
- Create: `internal/remote/protocol/messages.go`
- Test: `internal/remote/protocol/messages_test.go`

**Step 1: Write failing test**

Create `internal/remote/protocol/messages_test.go`:

```go
// ABOUTME: Tests for wire protocol message encoding
// ABOUTME: Ensures round-trip stability of ingest and query messages

package protocol

import (
    "bytes"
    "encoding/json"
    "testing"
    "time"

    "github.com/2389-research/ccvault/pkg/models"
)

func TestIngestMessageRoundTrip(t *testing.T) {
    orig := IngestMessage{
        Kind: KindSession,
        Session: &models.Session{
            ID:        "sess-1",
            StartedAt: time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC),
            EndedAt:   time.Date(2026, 7, 21, 10, 5, 0, 0, time.UTC),
            Source:    "claude-code",
        },
    }
    var buf bytes.Buffer
    if err := json.NewEncoder(&buf).Encode(orig); err != nil {
        t.Fatal(err)
    }

    var got IngestMessage
    if err := json.NewDecoder(&buf).Decode(&got); err != nil {
        t.Fatal(err)
    }
    if got.Kind != KindSession || got.Session.ID != "sess-1" {
        t.Fatalf("round trip: %+v", got)
    }
}

func TestSessionEndMessage(t *testing.T) {
    orig := IngestMessage{Kind: KindSessionEnd, SessionID: "sess-1"}
    b, _ := json.Marshal(orig)
    var got IngestMessage
    if err := json.Unmarshal(b, &got); err != nil {
        t.Fatal(err)
    }
    if got.Kind != KindSessionEnd || got.SessionID != "sess-1" {
        t.Fatalf("got %+v", got)
    }
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/remote/protocol/... -v`
Expected: FAIL — package doesn't exist.

**Step 3: Implement**

Create `internal/remote/protocol/messages.go`:

```go
// ABOUTME: Wire message types shared between ccvault client and ccvaultd server
// ABOUTME: Newline-delimited JSON over SSH channels; one message per line

package protocol

import (
    "github.com/2389-research/ccvault/pkg/models"
)

// SchemaVersion is bumped whenever the wire format changes incompatibly.
// The `version` command exposes this so clients can refuse to push to older servers.
const SchemaVersion = 1

// Kind discriminates message types on the ingest stream.
type Kind string

const (
    KindSession    Kind = "session"
    KindTurn       Kind = "turn"
    KindToolUse    Kind = "tool_use"
    KindSessionEnd Kind = "session_end"
)

// IngestMessage is one line of the client → server ingest stream.
// Exactly one of Session/Turn/ToolUse is populated per message; SessionID is
// used for KindSessionEnd (the commit marker).
type IngestMessage struct {
    Kind      Kind             `json:"kind"`
    Session   *models.Session  `json:"session,omitempty"`
    Turn      *models.Turn     `json:"turn,omitempty"`
    ToolUse   *models.ToolUse  `json:"tool_use,omitempty"`
    SessionID string           `json:"session_id,omitempty"`
}

// VersionResponse is what the server returns for the `version` command.
type VersionResponse struct {
    SchemaVersion int    `json:"schema_version"`
    BuildVersion  string `json:"build_version"`
}

// SearchResult mirrors models.SearchResult but is used as the query response type.
// Streamed one per line on the search command's stdout.
type SearchResult = models.SearchResult
```

**Step 4: Run test to verify it passes**

Run: `go test ./internal/remote/protocol/... -v`
Expected: PASS.

**Step 5: Commit**

```bash
git add internal/remote/protocol/messages.go internal/remote/protocol/messages_test.go
git commit -m "feat: add wire protocol package for group-mode transport"
```

---

## Phase 5: Server skeleton

### Task 7: `internal/remote/server/authkeys.go` — authorized_keys loader

**Files:**
- Create: `internal/remote/server/authkeys.go`
- Test: `internal/remote/server/authkeys_test.go`

**Step 1: Write failing test**

Create `internal/remote/server/authkeys_test.go`:

```go
// ABOUTME: Tests for authorized_keys parsing
// ABOUTME: Verifies pubkey → identity mapping and reload behavior

package server

import (
    "os"
    "path/filepath"
    "testing"

    "golang.org/x/crypto/ssh"
)

const testPubKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJyfW0v/YT8wYyBl4kQe0aWWQmSN5b5o0f8jqLnA0OSF alice@2389.ai"

func TestLoadAuthorizedKeys(t *testing.T) {
    dir := t.TempDir()
    path := filepath.Join(dir, "authorized_keys")
    contents := testPubKey + "\n" +
        "# a comment\n" +
        "\n" + // blank line
        "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIA1cKmvQ7GRfGjX+xxxxxxxxxxxxxxxxxxxxxxxxx bob@2389.ai\n"
    if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
        t.Fatal(err)
    }

    a, err := LoadAuthorizedKeys(path)
    if err != nil {
        t.Fatalf("load: %v", err)
    }

    // Look up alice's key
    parsed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(testPubKey))
    if err != nil {
        t.Fatal(err)
    }
    identity, ok := a.Lookup(parsed)
    if !ok {
        t.Fatal("alice key not found")
    }
    if identity != "alice@2389.ai" {
        t.Errorf("identity = %q, want alice@2389.ai", identity)
    }
}

func TestLoadAuthorizedKeysRejectsGarbage(t *testing.T) {
    dir := t.TempDir()
    path := filepath.Join(dir, "authorized_keys")
    if err := os.WriteFile(path, []byte("not-a-key oops\n"), 0600); err != nil {
        t.Fatal(err)
    }
    _, err := LoadAuthorizedKeys(path)
    if err == nil {
        t.Fatal("expected error on garbage line")
    }
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/remote/server/... -v`
Expected: FAIL — package doesn't exist.

**Step 3: Implement**

Create `internal/remote/server/authkeys.go`:

```go
// ABOUTME: Loads and looks up SSH pubkeys from an OpenSSH authorized_keys file
// ABOUTME: The trailing comment on each line is used as the pushed_by identity

package server

import (
    "bytes"
    "fmt"
    "os"
    "strings"
    "sync"

    "golang.org/x/crypto/ssh"
)

// AuthorizedKeys holds a set of authorized SSH pubkeys keyed by their marshaled form,
// mapping to an identity string (the trailing comment on the authorized_keys line).
type AuthorizedKeys struct {
    mu    sync.RWMutex
    byKey map[string]string // key: string(pubkey.Marshal()), val: identity
    path  string
}

// LoadAuthorizedKeys parses an OpenSSH authorized_keys file.
func LoadAuthorizedKeys(path string) (*AuthorizedKeys, error) {
    data, err := os.ReadFile(path)
    if err != nil {
        return nil, fmt.Errorf("read %s: %w", path, err)
    }

    a := &AuthorizedKeys{byKey: make(map[string]string), path: path}
    for lineno, raw := range bytes.Split(data, []byte("\n")) {
        line := bytes.TrimSpace(raw)
        if len(line) == 0 || line[0] == '#' {
            continue
        }
        pubkey, comment, _, _, err := ssh.ParseAuthorizedKey(line)
        if err != nil {
            return nil, fmt.Errorf("%s:%d: parse authorized_keys line: %w", path, lineno+1, err)
        }
        identity := strings.TrimSpace(comment)
        if identity == "" {
            identity = fmt.Sprintf("unknown-key-%d", lineno+1)
        }
        a.byKey[string(pubkey.Marshal())] = identity
    }
    return a, nil
}

// Lookup returns the identity for a matching pubkey, or ("", false).
func (a *AuthorizedKeys) Lookup(key ssh.PublicKey) (string, bool) {
    a.mu.RLock()
    defer a.mu.RUnlock()
    id, ok := a.byKey[string(key.Marshal())]
    return id, ok
}

// Reload re-reads the underlying file and atomically swaps the key map.
// Call this on SIGHUP.
func (a *AuthorizedKeys) Reload() error {
    fresh, err := LoadAuthorizedKeys(a.path)
    if err != nil {
        return err
    }
    a.mu.Lock()
    a.byKey = fresh.byKey
    a.mu.Unlock()
    return nil
}
```

**Step 4: Run test to verify it passes**

Run: `go test ./internal/remote/server/... -v`
Expected: PASS.

**Step 5: Commit**

```bash
git add internal/remote/server/authkeys.go internal/remote/server/authkeys_test.go
git commit -m "feat: authorized_keys loader for ccvaultd"
```

### Task 8: `internal/remote/server/server.go` — SSH server + dispatcher

**Files:**
- Create: `internal/remote/server/server.go`
- Create: `internal/remote/server/dispatch.go`
- Create: `internal/remote/server/hostkey.go`
- Test: `internal/remote/server/server_test.go`

**Step 1: Write failing integration-style test**

Create `internal/remote/server/server_test.go`:

```go
// ABOUTME: End-to-end SSH server tests
// ABOUTME: Spins up a real ccvaultd on a random port and connects via ssh client

package server

import (
    "bufio"
    "context"
    "encoding/json"
    "io"
    "net"
    "os"
    "path/filepath"
    "testing"

    "golang.org/x/crypto/ssh"

    "github.com/2389-research/ccvault/internal/db"
    "github.com/2389-research/ccvault/internal/remote/protocol"
)

func TestServerVersionCommand(t *testing.T) {
    srv, addr, clientKey, cleanup := startTestServer(t)
    defer cleanup()

    conn, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
        User: "ccvault",
        Auth: []ssh.AuthMethod{ssh.PublicKeys(clientKey)},
        HostKeyCallback: ssh.InsecureIgnoreHostKey(),
    })
    if err != nil {
        t.Fatal(err)
    }
    defer conn.Close()

    sess, err := conn.NewSession()
    if err != nil {
        t.Fatal(err)
    }
    defer sess.Close()

    stdout, _ := sess.StdoutPipe()
    if err := sess.Start("version"); err != nil {
        t.Fatal(err)
    }

    line, _, err := bufio.NewReader(stdout).ReadLine()
    if err != nil && err != io.EOF {
        t.Fatal(err)
    }
    var resp protocol.VersionResponse
    if err := json.Unmarshal(line, &resp); err != nil {
        t.Fatalf("unmarshal %q: %v", string(line), err)
    }
    if resp.SchemaVersion != protocol.SchemaVersion {
        t.Errorf("schema_version = %d, want %d", resp.SchemaVersion, protocol.SchemaVersion)
    }
    _ = sess.Wait()
    _ = srv
}

// startTestServer starts a ccvaultd on a random localhost port with a fresh
// SQLite tempdir and a temp authorized_keys containing the returned client key.
// Returns (server, addr, clientSigner, cleanup).
func startTestServer(t *testing.T) (*Server, string, ssh.Signer, func()) {
    t.Helper()

    dir := t.TempDir()
    dataDir := filepath.Join(dir, "data")
    if err := os.MkdirAll(dataDir, 0700); err != nil {
        t.Fatal(err)
    }
    database, err := db.Open(dataDir)
    if err != nil {
        t.Fatal(err)
    }

    // Generate a client key and write its pubkey to authorized_keys
    clientSigner, clientPub := generateTestKeyPair(t)
    authPath := filepath.Join(dir, "authorized_keys")
    line := string(ssh.MarshalAuthorizedKey(clientPub))
    // MarshalAuthorizedKey adds a newline but no comment; append one
    if err := os.WriteFile(authPath, []byte(line[:len(line)-1]+" test-user\n"), 0600); err != nil {
        t.Fatal(err)
    }

    authKeys, err := LoadAuthorizedKeys(authPath)
    if err != nil {
        t.Fatal(err)
    }

    hostKeyPath := filepath.Join(dir, "host_key")
    hostSigner, err := LoadOrGenerateHostKey(hostKeyPath)
    if err != nil {
        t.Fatal(err)
    }

    ln, err := net.Listen("tcp", "127.0.0.1:0")
    if err != nil {
        t.Fatal(err)
    }

    srv := New(database, authKeys, hostSigner, "0.0.1-test")
    ctx, cancel := context.WithCancel(context.Background())
    go func() { _ = srv.Serve(ctx, ln) }()

    cleanup := func() {
        cancel()
        _ = ln.Close()
        _ = database.Close()
    }
    return srv, ln.Addr().String(), clientSigner, cleanup
}

// generateTestKeyPair is a helper. Implementation deferred until host key task;
// for now use a stub that uses the host_key generator.
func generateTestKeyPair(t *testing.T) (ssh.Signer, ssh.PublicKey) {
    t.Helper()
    dir := t.TempDir()
    signer, err := LoadOrGenerateHostKey(filepath.Join(dir, "client_key"))
    if err != nil {
        t.Fatal(err)
    }
    return signer, signer.PublicKey()
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/remote/server/... -run TestServerVersion -v`
Expected: FAIL — `New`, `LoadOrGenerateHostKey`, `Server.Serve` don't exist.

**Step 3: Implement host key generation**

Create `internal/remote/server/hostkey.go`:

```go
// ABOUTME: Host key management for ccvaultd — load or generate ed25519 on first launch
// ABOUTME: Persists PEM-encoded key at the configured path with 0600 permissions

package server

import (
    "crypto/ed25519"
    "crypto/rand"
    "encoding/pem"
    "errors"
    "fmt"
    "os"

    "golang.org/x/crypto/ssh"
)

// LoadOrGenerateHostKey returns a signer for the host key at path.
// If the file doesn't exist, a new ed25519 key is generated and persisted.
func LoadOrGenerateHostKey(path string) (ssh.Signer, error) {
    data, err := os.ReadFile(path)
    if err == nil {
        return ssh.ParsePrivateKey(data)
    }
    if !errors.Is(err, os.ErrNotExist) {
        return nil, fmt.Errorf("read host key %s: %w", path, err)
    }

    _, priv, err := ed25519.GenerateKey(rand.Reader)
    if err != nil {
        return nil, fmt.Errorf("generate ed25519: %w", err)
    }
    pemBytes, err := marshalOpenSSHPrivateKey(priv)
    if err != nil {
        return nil, err
    }
    if err := os.WriteFile(path, pemBytes, 0600); err != nil {
        return nil, fmt.Errorf("write host key: %w", err)
    }
    return ssh.ParsePrivateKey(pemBytes)
}

func marshalOpenSSHPrivateKey(key ed25519.PrivateKey) ([]byte, error) {
    b, err := ssh.MarshalPrivateKey(key, "ccvaultd host key")
    if err != nil {
        return nil, fmt.Errorf("marshal ssh private key: %w", err)
    }
    return pem.EncodeToMemory(b), nil
}
```

**Step 4: Implement dispatcher**

Create `internal/remote/server/dispatch.go`:

```go
// ABOUTME: Routes an SSH channel's command string to the right handler
// ABOUTME: Handlers get the channel (stdin/stdout) and the connection identity

package server

import (
    "encoding/json"
    "fmt"
    "io"

    "github.com/2389-research/ccvault/internal/remote/protocol"
)

// HandlerCtx is passed to each command handler.
type HandlerCtx struct {
    Server   *Server
    Identity string // pushed_by, from the SSH pubkey match
    Stdin    io.Reader
    Stdout   io.Writer
    Stderr   io.Writer
    Args     string // raw args after the command name
}

// dispatch parses the SSH command string and invokes the matching handler.
// Returns the exit code.
func (s *Server) dispatch(command string, ctx HandlerCtx) int {
    name, args := splitCommand(command)
    ctx.Args = args
    switch name {
    case "version":
        return handleVersion(ctx)
    case "ingest":
        return handleIngest(ctx)
    case "search", "sessions", "show", "stats":
        // Query handlers land in Phase 7
        fmt.Fprintf(ctx.Stderr, "command %q not implemented yet\n", name)
        return 2
    default:
        fmt.Fprintf(ctx.Stderr, "unknown command: %q\n", name)
        return 2
    }
}

func splitCommand(cmd string) (name, args string) {
    for i := 0; i < len(cmd); i++ {
        if cmd[i] == ' ' {
            return cmd[:i], cmd[i+1:]
        }
    }
    return cmd, ""
}

func handleVersion(ctx HandlerCtx) int {
    resp := protocol.VersionResponse{
        SchemaVersion: protocol.SchemaVersion,
        BuildVersion:  ctx.Server.buildVersion,
    }
    if err := json.NewEncoder(ctx.Stdout).Encode(resp); err != nil {
        fmt.Fprintln(ctx.Stderr, err)
        return 1
    }
    return 0
}
```

**Step 5: Implement Server**

Create `internal/remote/server/server.go`:

```go
// ABOUTME: SSH server for ccvaultd — accepts pubkey-authed connections and dispatches commands
// ABOUTME: One connection = one command; identity is the authorized_keys comment field

package server

import (
    "context"
    "fmt"
    "io"
    "log"
    "net"

    "golang.org/x/crypto/ssh"

    "github.com/2389-research/ccvault/internal/db"
)

type Server struct {
    db           *db.DB
    authKeys     *AuthorizedKeys
    hostKey      ssh.Signer
    buildVersion string
}

func New(database *db.DB, authKeys *AuthorizedKeys, hostKey ssh.Signer, buildVersion string) *Server {
    return &Server{db: database, authKeys: authKeys, hostKey: hostKey, buildVersion: buildVersion}
}

// DB exposes the underlying database (used by handlers).
func (s *Server) DB() *db.DB { return s.db }

// Serve accepts connections until ctx is cancelled or the listener closes.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
    for {
        select {
        case <-ctx.Done():
            return nil
        default:
        }
        conn, err := ln.Accept()
        if err != nil {
            if isClosed(err) {
                return nil
            }
            return err
        }
        go s.handleConn(conn)
    }
}

func (s *Server) sshConfig() *ssh.ServerConfig {
    cfg := &ssh.ServerConfig{
        PublicKeyCallback: func(md ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
            id, ok := s.authKeys.Lookup(key)
            if !ok {
                return nil, fmt.Errorf("pubkey not authorized")
            }
            return &ssh.Permissions{Extensions: map[string]string{"ccvault-identity": id}}, nil
        },
    }
    cfg.AddHostKey(s.hostKey)
    return cfg
}

func (s *Server) handleConn(nc net.Conn) {
    defer nc.Close()
    sconn, chans, reqs, err := ssh.NewServerConn(nc, s.sshConfig())
    if err != nil {
        log.Printf("ssh handshake: %v", err)
        return
    }
    defer sconn.Close()
    identity := sconn.Permissions.Extensions["ccvault-identity"]

    go ssh.DiscardRequests(reqs)
    for ch := range chans {
        if ch.ChannelType() != "session" {
            _ = ch.Reject(ssh.UnknownChannelType, "only 'session' supported")
            continue
        }
        channel, requests, err := ch.Accept()
        if err != nil {
            log.Printf("accept channel: %v", err)
            continue
        }
        go s.handleChannel(channel, requests, identity)
    }
}

func (s *Server) handleChannel(ch ssh.Channel, reqs <-chan *ssh.Request, identity string) {
    var command string
    for req := range reqs {
        switch req.Type {
        case "exec":
            command = string(req.Payload[4:]) // strip length prefix
            _ = req.Reply(true, nil)
            code := s.dispatch(command, HandlerCtx{
                Server:   s,
                Identity: identity,
                Stdin:    ch,
                Stdout:   ch,
                Stderr:   ch.Stderr(),
            })
            _, _ = ch.SendRequest("exit-status", false, exitStatusPayload(code))
            _ = ch.Close()
            return
        case "shell":
            _ = req.Reply(false, nil)
            _ = ch.Close()
            return
        default:
            _ = req.Reply(false, nil)
        }
    }
}

func exitStatusPayload(code int) []byte {
    // SSH exit-status is a 32-bit big-endian uint
    return []byte{
        byte(code >> 24), byte(code >> 16), byte(code >> 8), byte(code),
    }
}

func isClosed(err error) bool {
    return err == io.EOF ||
        (err != nil && err.Error() == "use of closed network connection")
}
```

**Step 6: Run test to verify it passes**

Run: `go test ./internal/remote/server/... -run TestServerVersion -v`
Expected: PASS.

**Step 7: Commit**

```bash
git add internal/remote/server/
git commit -m "feat: ccvaultd SSH server with version command and dispatcher"
```

### Task 9: Ingest handler

**Files:**
- Create: `internal/remote/server/ingest.go`
- Test: extend `internal/remote/server/server_test.go`

**Step 1: Write failing test**

Add to `internal/remote/server/server_test.go`:

```go
func TestServerIngestSession(t *testing.T) {
    srv, addr, clientKey, cleanup := startTestServer(t)
    defer cleanup()

    conn, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
        User: "ccvault",
        Auth: []ssh.AuthMethod{ssh.PublicKeys(clientKey)},
        HostKeyCallback: ssh.InsecureIgnoreHostKey(),
    })
    if err != nil {
        t.Fatal(err)
    }
    defer conn.Close()

    sess, err := conn.NewSession()
    if err != nil {
        t.Fatal(err)
    }
    defer sess.Close()

    stdin, _ := sess.StdinPipe()
    if err := sess.Start("ingest"); err != nil {
        t.Fatal(err)
    }

    enc := json.NewEncoder(stdin)
    _ = enc.Encode(protocol.IngestMessage{
        Kind: protocol.KindSession,
        Session: &models.Session{
            ID:          "sess-e2e-1",
            ProjectPath: "/tmp/p",
            StartedAt:   time.Now().UTC(),
            EndedAt:     time.Now().UTC(),
            Source:      "claude-code",
        },
    })
    _ = enc.Encode(protocol.IngestMessage{
        Kind: protocol.KindSessionEnd, SessionID: "sess-e2e-1",
    })
    stdin.Close()
    _ = sess.Wait()

    var pushedBy string
    var count int
    row := srv.DB().QueryRow(
        "SELECT COUNT(*), COALESCE(MAX(pushed_by), '') FROM sessions WHERE id = ?",
        "sess-e2e-1",
    )
    if err := row.Scan(&count, &pushedBy); err != nil {
        t.Fatal(err)
    }
    if count != 1 {
        t.Fatalf("session not persisted, count=%d", count)
    }
    if pushedBy != "test-user" {
        t.Errorf("pushed_by = %q, want test-user", pushedBy)
    }
}
```

Add imports at top of the test file:

```go
"encoding/json"
"time"

"github.com/2389-research/ccvault/pkg/models"
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/remote/server/... -run TestServerIngestSession -v`
Expected: FAIL — ingest handler prints "not implemented" and returns 2.

**Step 3: Implement `ingest.go`**

Create `internal/remote/server/ingest.go`:

```go
// ABOUTME: Ingest handler — buffers per-session records until session_end, then upserts
// ABOUTME: Reuses the same DB helpers as local sync; stamps pushed_by from the SSH identity

package server

import (
    "database/sql"
    "encoding/json"
    "fmt"
    "io"

    "github.com/2389-research/ccvault/internal/remote/protocol"
    "github.com/2389-research/ccvault/pkg/models"
)

// pending holds the accumulated records for one session awaiting session_end.
type pending struct {
    session  *models.Session
    turns    []models.Turn
    toolUses []models.ToolUse
}

func handleIngest(ctx HandlerCtx) int {
    dec := json.NewDecoder(ctx.Stdin)
    buffers := make(map[string]*pending)

    for {
        var msg protocol.IngestMessage
        if err := dec.Decode(&msg); err != nil {
            if err == io.EOF {
                return 0
            }
            fmt.Fprintf(ctx.Stderr, "decode: %v\n", err)
            return 1
        }
        switch msg.Kind {
        case protocol.KindSession:
            if msg.Session == nil {
                fmt.Fprintln(ctx.Stderr, "session kind with nil Session")
                return 1
            }
            msg.Session.PushedBy = ctx.Identity
            buffers[msg.Session.ID] = &pending{session: msg.Session}
        case protocol.KindTurn:
            if msg.Turn == nil {
                fmt.Fprintln(ctx.Stderr, "turn kind with nil Turn")
                return 1
            }
            p := buffers[msg.Turn.SessionID]
            if p == nil {
                fmt.Fprintf(ctx.Stderr, "turn for unknown session %s\n", msg.Turn.SessionID)
                return 1
            }
            p.turns = append(p.turns, *msg.Turn)
        case protocol.KindToolUse:
            if msg.ToolUse == nil {
                fmt.Fprintln(ctx.Stderr, "tool_use kind with nil ToolUse")
                return 1
            }
            p := buffers[msg.ToolUse.SessionID]
            if p == nil {
                fmt.Fprintf(ctx.Stderr, "tool_use for unknown session %s\n", msg.ToolUse.SessionID)
                return 1
            }
            p.toolUses = append(p.toolUses, *msg.ToolUse)
        case protocol.KindSessionEnd:
            p := buffers[msg.SessionID]
            if p == nil {
                fmt.Fprintf(ctx.Stderr, "session_end for unknown session %s\n", msg.SessionID)
                return 1
            }
            if err := commitSession(ctx, p); err != nil {
                fmt.Fprintf(ctx.Stderr, "commit %s: %v\n", msg.SessionID, err)
                return 1
            }
            delete(buffers, msg.SessionID)
        default:
            fmt.Fprintf(ctx.Stderr, "unknown message kind: %q\n", msg.Kind)
            return 1
        }
    }
}

// commitSession does the same transactional upsert as local sync (see internal/sync).
// Delete-then-insert on (source, session_id) yields "latest push wins" semantics.
func commitSession(ctx HandlerCtx, p *pending) error {
    d := ctx.Server.db
    return d.WithTx(func(tx *sql.Tx) error {
        // Upsert project
        proj := &models.Project{
            Path:           p.session.ProjectPath,
            DisplayName:    p.session.ProjectPath, // remote doesn't know a nicer name; client's OK
            FirstSeenAt:    p.session.StartedAt,
            LastActivityAt: p.session.EndedAt,
            SessionCount:   1,
            TotalTokens:    p.session.TotalTokens(),
            Source:         p.session.Source,
        }
        if err := d.UpsertProjectTx(tx, proj); err != nil {
            return fmt.Errorf("upsert project: %w", err)
        }
        p.session.ProjectID = proj.ID

        if err := d.DeleteTurnsForSessionTx(tx, p.session.ID); err != nil {
            return err
        }
        if err := d.DeleteToolUsesForSessionTx(tx, p.session.ID); err != nil {
            return err
        }
        if err := d.UpsertSessionTx(tx, p.session); err != nil {
            return err
        }
        if err := d.InsertTurnsTx(tx, p.turns); err != nil {
            return err
        }
        if len(p.toolUses) > 0 {
            if err := d.InsertToolUsesTx(tx, p.toolUses); err != nil {
                return err
            }
        }
        return nil
    })
}
```

**Step 4: Update dispatcher to route ingest**

The dispatcher in Task 8 already routes `ingest` → `handleIngest`. No change needed.

**Step 5: Wire `pushed_by` through UpsertSessionTx**

Check `internal/db/sessions.go` — the existing `UpsertSessionTx` likely doesn't write `pushed_by`. Add it there.

Find the INSERT and UPDATE statements in `UpsertSessionTx` and add `pushed_by` to both the column list and the `?` placeholders / SET clause, sourcing the value from `session.PushedBy`.

Run: `go test ./internal/db/... -v`
Expected: PASS (existing tests should still work since default `PushedBy` is empty string).

**Step 6: Run integration test to verify it passes**

Run: `go test ./internal/remote/server/... -run TestServerIngestSession -v`
Expected: PASS.

**Step 7: Full test suite**

Run: `go test ./...`
Expected: PASS.

**Step 8: Commit**

```bash
git add internal/remote/server/ingest.go internal/remote/server/server_test.go internal/db/sessions.go
git commit -m "feat: ccvaultd ingest handler with pushed_by attribution"
```

---

## Phase 6: Client-side push

### Task 10: `internal/remote/client/client.go` — SSH dial + command runner

**Files:**
- Create: `internal/remote/client/client.go`
- Test: `internal/remote/client/client_test.go`

**Step 1: Write failing test**

Create `internal/remote/client/client_test.go`:

```go
// ABOUTME: Client-side SSH connection tests
// ABOUTME: Reuses server test helpers to drive real end-to-end scenarios

package client

import (
    "bufio"
    "encoding/json"
    "testing"

    "github.com/2389-research/ccvault/internal/remote/protocol"
    "github.com/2389-research/ccvault/internal/remote/server"
    "golang.org/x/crypto/ssh"
)

func TestClientRunVersion(t *testing.T) {
    _, addr, signer, cleanup := server.StartTestServer(t)
    defer cleanup()

    c := &Client{
        Addr:        addr,
        User:        "ccvault",
        Signers:     []ssh.Signer{signer},
        HostKey:     ssh.InsecureIgnoreHostKey(),
    }
    stdout, _, err := c.Run("version", nil)
    if err != nil {
        t.Fatal(err)
    }
    defer stdout.Close()

    line, _, _ := bufio.NewReader(stdout).ReadLine()
    var resp protocol.VersionResponse
    if err := json.Unmarshal(line, &resp); err != nil {
        t.Fatalf("%v: %q", err, line)
    }
    if resp.SchemaVersion != protocol.SchemaVersion {
        t.Errorf("wrong schema version: %d", resp.SchemaVersion)
    }
}
```

Note this references `server.StartTestServer` — that helper needs to be exported. Do that in this task: rename `startTestServer` → `StartTestServer` in `internal/remote/server/server_test.go`, or (better) extract it to a new `internal/remote/server/testing.go` build-tagged for tests. Prefer the extraction so the helper is available to the client-side test:

Move `startTestServer` and `generateTestKeyPair` into `internal/remote/server/testing.go`:

```go
// ABOUTME: Test helpers exported for cross-package use (client tests, integration tests)
// ABOUTME: Only imported from _test.go files; if you're linking this in prod, that's a bug

package server

// (paste the helpers here, renamed to StartTestServer and GenerateTestKeyPair)
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/remote/client/... -v`
Expected: FAIL — package doesn't exist.

**Step 3: Implement**

Create `internal/remote/client/client.go`:

```go
// ABOUTME: SSH client wrapper for talking to ccvaultd
// ABOUTME: Uses ssh-agent when available; falls back to identity files under ~/.ssh

package client

import (
    "fmt"
    "io"
    "net"
    "net/url"
    "os"
    "path/filepath"
    "strings"
    "time"

    "golang.org/x/crypto/ssh"
    "golang.org/x/crypto/ssh/agent"
)

// Client is a minimal SSH client for ccvaultd.
type Client struct {
    // Addr is host:port. Use ResolveAddr on a remote URL to compute it.
    Addr    string
    User    string
    Signers []ssh.Signer
    HostKey ssh.HostKeyCallback
    Timeout time.Duration
}

// FromRemoteURL builds a Client from a user-supplied URL string.
// Accepted forms:
//   - user@host             → tcp host:22
//   - user@host:port
//   - ssh://user@host:port/anything
//   - ssh://host:port       (uses "ccvault" as default user)
func FromRemoteURL(raw string) (*Client, error) {
    user, host, port, err := parseRemoteURL(raw)
    if err != nil {
        return nil, err
    }
    signers, err := defaultSigners()
    if err != nil {
        return nil, err
    }
    return &Client{
        Addr:    net.JoinHostPort(host, port),
        User:    user,
        Signers: signers,
        HostKey: knownHostsCallback(),
        Timeout: 30 * time.Second,
    }, nil
}

func parseRemoteURL(raw string) (user, host, port string, err error) {
    port = "22"
    user = "ccvault"

    if strings.HasPrefix(raw, "ssh://") {
        u, perr := url.Parse(raw)
        if perr != nil {
            return "", "", "", perr
        }
        if u.User != nil {
            user = u.User.Username()
        }
        host = u.Hostname()
        if p := u.Port(); p != "" {
            port = p
        }
        return
    }

    // user@host[:port] form
    at := strings.Index(raw, "@")
    if at >= 0 {
        user = raw[:at]
        raw = raw[at+1:]
    }
    host = raw
    if colon := strings.LastIndex(raw, ":"); colon >= 0 && !strings.Contains(raw, "]") {
        host = raw[:colon]
        port = raw[colon+1:]
    }
    if host == "" {
        return "", "", "", fmt.Errorf("no host in remote URL")
    }
    return
}

func defaultSigners() ([]ssh.Signer, error) {
    // Try ssh-agent first
    if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
        conn, err := net.Dial("unix", sock)
        if err == nil {
            ag := agent.NewClient(conn)
            signers, err := ag.Signers()
            if err == nil && len(signers) > 0 {
                return signers, nil
            }
        }
    }
    // Fall back to ~/.ssh/id_ed25519 then ~/.ssh/id_rsa
    home, err := os.UserHomeDir()
    if err != nil {
        return nil, err
    }
    for _, name := range []string{"id_ed25519", "id_rsa"} {
        path := filepath.Join(home, ".ssh", name)
        data, err := os.ReadFile(path)
        if err != nil {
            continue
        }
        signer, err := ssh.ParsePrivateKey(data)
        if err != nil {
            continue
        }
        return []ssh.Signer{signer}, nil
    }
    return nil, fmt.Errorf("no SSH keys available (no agent, no ~/.ssh/id_ed25519 or id_rsa)")
}

func knownHostsCallback() ssh.HostKeyCallback {
    // v1: use insecure ignore for MVP; TODO: wire up known_hosts properly
    // (spec deferral: OpenSSH known_hosts is a project of its own; document this in README)
    return ssh.InsecureIgnoreHostKey()
}

// Run executes a single command against the server and returns stdout as an
// io.ReadCloser. If stdin is non-nil, it is piped to the server and closed
// when this function returns. Caller must Close the returned reader.
func (c *Client) Run(command string, stdin io.Reader) (io.ReadCloser, *ssh.Session, error) {
    cfg := &ssh.ClientConfig{
        User:            c.User,
        Auth:            []ssh.AuthMethod{ssh.PublicKeys(c.Signers...)},
        HostKeyCallback: c.HostKey,
        Timeout:         c.Timeout,
    }
    conn, err := ssh.Dial("tcp", c.Addr, cfg)
    if err != nil {
        return nil, nil, fmt.Errorf("dial %s: %w", c.Addr, err)
    }
    sess, err := conn.NewSession()
    if err != nil {
        _ = conn.Close()
        return nil, nil, err
    }
    stdout, err := sess.StdoutPipe()
    if err != nil {
        _ = sess.Close()
        _ = conn.Close()
        return nil, nil, err
    }
    if stdin != nil {
        w, err := sess.StdinPipe()
        if err != nil {
            _ = sess.Close()
            _ = conn.Close()
            return nil, nil, err
        }
        go func() {
            _, _ = io.Copy(w, stdin)
            _ = w.Close()
        }()
    }
    if err := sess.Start(command); err != nil {
        _ = sess.Close()
        _ = conn.Close()
        return nil, nil, err
    }
    return &sessionReader{r: stdout, sess: sess, conn: conn}, sess, nil
}

type sessionReader struct {
    r    io.Reader
    sess *ssh.Session
    conn *ssh.Client
}

func (s *sessionReader) Read(p []byte) (int, error) { return s.r.Read(p) }
func (s *sessionReader) Close() error {
    _ = s.sess.Wait()
    _ = s.sess.Close()
    return s.conn.Close()
}
```

**Step 4: Run test to verify it passes**

Run: `go test ./internal/remote/client/... -v`
Expected: PASS.

**Step 5: Commit**

```bash
git add internal/remote/client/ internal/remote/server/testing.go internal/remote/server/server_test.go
git commit -m "feat: SSH client wrapper for ccvault-to-ccvaultd calls"
```

### Task 11: Push implementation

**Files:**
- Create: `internal/remote/client/push.go`
- Test: `internal/remote/client/push_test.go`

**Step 1: Write failing integration test**

Create `internal/remote/client/push_test.go`:

```go
// ABOUTME: End-to-end push tests
// ABOUTME: Real client + real ccvaultd; verifies data lands on the server correctly

package client

import (
    "path/filepath"
    "testing"
    "time"

    "github.com/2389-research/ccvault/internal/db"
    "github.com/2389-research/ccvault/internal/remote/server"
    "github.com/2389-research/ccvault/pkg/models"
    "golang.org/x/crypto/ssh"
)

func TestPushSyncsSessions(t *testing.T) {
    srv, addr, signer, cleanup := server.StartTestServer(t)
    defer cleanup()

    // Build a local ccvault DB with a session ready to push
    dir := t.TempDir()
    localDataDir := filepath.Join(dir, "local")
    localDB, err := db.Open(localDataDir)
    if err != nil {
        t.Fatal(err)
    }
    defer localDB.Close()

    seedLocalSession(t, localDB, "sess-push-1")

    client := &Client{
        Addr:    addr,
        User:    "ccvault",
        Signers: []ssh.Signer{signer},
        HostKey: ssh.InsecureIgnoreHostKey(),
    }
    stats, err := Push(client, localDB, "origin", false)
    if err != nil {
        t.Fatalf("push: %v", err)
    }
    if stats.SessionsPushed != 1 {
        t.Errorf("SessionsPushed = %d, want 1", stats.SessionsPushed)
    }

    // Verify server side
    var count int
    if err := srv.DB().QueryRow("SELECT COUNT(*) FROM sessions WHERE id = ?", "sess-push-1").Scan(&count); err != nil {
        t.Fatal(err)
    }
    if count != 1 {
        t.Fatalf("session missing on server")
    }

    // Second push should be a no-op
    stats, err = Push(client, localDB, "origin", false)
    if err != nil {
        t.Fatal(err)
    }
    if stats.SessionsPushed != 0 {
        t.Errorf("second push SessionsPushed = %d, want 0 (incremental should skip)", stats.SessionsPushed)
    }
}

func seedLocalSession(t *testing.T, d *db.DB, id string) {
    t.Helper()
    _, err := d.Exec(`INSERT INTO projects (path, display_name, first_seen_at, last_activity_at, session_count, total_tokens, source)
        VALUES ('/tmp/p', 'p', ?, ?, 1, 0, 'claude-code')`,
        time.Now().UTC(), time.Now().UTC())
    if err != nil {
        t.Fatal(err)
    }
    var pid int64
    if err := d.QueryRow("SELECT last_insert_rowid()").Scan(&pid); err != nil {
        t.Fatal(err)
    }
    _, err = d.Exec(`INSERT INTO sessions
        (id, project_id, started_at, ended_at, turn_count, input_tokens, output_tokens, source_file, source, pushed_by)
        VALUES (?, ?, ?, ?, 0, 0, 0, '/tmp/p/x.jsonl', 'claude-code', '')`,
        id, pid, time.Now().UTC(), time.Now().UTC())
    if err != nil {
        t.Fatal(err)
    }
    _ = models.Session{} // keep import alive
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/remote/client/... -run TestPushSyncsSessions -v`
Expected: FAIL — `Push` doesn't exist.

**Step 3: Implement**

Create `internal/remote/client/push.go`:

```go
// ABOUTME: Streams pending sessions to the remote via the `ingest` command
// ABOUTME: Updates remote_push_state on success so subsequent pushes are incremental

package client

import (
    "encoding/json"
    "fmt"
    "io"

    "github.com/2389-research/ccvault/internal/db"
    "github.com/2389-research/ccvault/internal/remote/protocol"
)

// PushStats records what a Push call did.
type PushStats struct {
    SessionsPushed int
    TurnsPushed    int
    ToolUsesPushed int
}

// Push streams every session in `db` that's pending (per remote_push_state) to
// the remote and records success. If dryRun is true, computes and returns the
// stats but sends nothing.
func Push(c *Client, database *db.DB, remoteName string, dryRun bool) (*PushStats, error) {
    ids, err := database.SessionsPendingPush(remoteName)
    if err != nil {
        return nil, fmt.Errorf("pending push: %w", err)
    }

    stats := &PushStats{}
    if dryRun {
        stats.SessionsPushed = len(ids)
        return stats, nil
    }
    if len(ids) == 0 {
        return stats, nil
    }

    pr, pw := io.Pipe()
    var runErr error
    go func() {
        defer pw.Close()
        enc := json.NewEncoder(pw)
        for _, id := range ids {
            sess, err := database.GetSession(id)
            if err != nil {
                runErr = fmt.Errorf("get session %s: %w", id, err)
                return
            }
            if err := enc.Encode(protocol.IngestMessage{Kind: protocol.KindSession, Session: sess}); err != nil {
                runErr = err
                return
            }
            turns, err := database.GetTurns(id)
            if err != nil {
                runErr = err
                return
            }
            for i := range turns {
                if err := enc.Encode(protocol.IngestMessage{Kind: protocol.KindTurn, Turn: &turns[i]}); err != nil {
                    runErr = err
                    return
                }
                stats.TurnsPushed++
            }
            // NOTE: tool_uses will be pushed once GetToolUsesForSession helper exists.
            // For MVP we omit them; add a follow-up task if needed.
            if err := enc.Encode(protocol.IngestMessage{Kind: protocol.KindSessionEnd, SessionID: id}); err != nil {
                runErr = err
                return
            }
            stats.SessionsPushed++
            _ = database.RecordPush(remoteName, id, sess.EndedAt)
        }
    }()

    reader, _, err := c.Run("ingest", pr)
    if err != nil {
        return nil, err
    }
    _, _ = io.Copy(io.Discard, reader)
    _ = reader.Close()
    if runErr != nil {
        return nil, runErr
    }
    return stats, nil
}
```

**Step 4: Run test to verify it passes**

Run: `go test ./internal/remote/client/... -run TestPushSyncsSessions -v`
Expected: PASS.

**Step 5: Commit**

```bash
git add internal/remote/client/push.go internal/remote/client/push_test.go
git commit -m "feat: incremental push from ccvault client to ccvaultd"
```

### Task 12: `ccvault remote` and `ccvault push` CLI

**Files:**
- Modify: `cmd/ccvault/main.go` — register new commands
- Create: `cmd/ccvault/remote.go`
- Create: `cmd/ccvault/push.go`

**Step 1: Write minimal smoke tests**

The Cobra command wiring is boilerplate; a smoke test that runs `ccvault remote list` on a config with no remotes and asserts "no remotes configured" is enough. Add to `cmd/ccvault/main_test.go` (create if needed).

Longer term, `push` E2E is covered by Task 11's integration test. Keep the CLI code thin and delegate to the packages.

**Step 2: Implement `cmd/ccvault/remote.go`**

```go
// ABOUTME: ccvault remote add/list/remove — manages configured remote vaults
// ABOUTME: Writes to ~/.ccvault/config.toml via a small TOML shim

package main

import (
    "fmt"
    "os"
    "path/filepath"

    "github.com/2389-research/ccvault/internal/config"
    "github.com/pelletier/go-toml/v2"
    "github.com/spf13/cobra"
)

var remoteCmd = &cobra.Command{
    Use:   "remote",
    Short: "Manage configured remote vaults",
}

var remoteListCmd = &cobra.Command{
    Use:   "list",
    Short: "List configured remotes",
    RunE: func(cmd *cobra.Command, args []string) error {
        cfg, err := config.Load()
        if err != nil {
            return err
        }
        if len(cfg.Remotes) == 0 {
            fmt.Println("No remotes configured.")
            return nil
        }
        for name, r := range cfg.Remotes {
            fmt.Printf("%-20s %s\n", name, r.URL)
        }
        return nil
    },
}

var remoteAddCmd = &cobra.Command{
    Use:   "add [name] [url]",
    Short: "Add a remote vault",
    Args:  cobra.ExactArgs(2),
    RunE: func(cmd *cobra.Command, args []string) error {
        return writeRemote(args[0], args[1])
    },
}

var remoteRemoveCmd = &cobra.Command{
    Use:   "remove [name]",
    Short: "Remove a remote vault",
    Args:  cobra.ExactArgs(1),
    RunE: func(cmd *cobra.Command, args []string) error {
        return deleteRemote(args[0])
    },
}

// writeRemote merges a new [remotes.<name>] entry into the config file.
func writeRemote(name, url string) error {
    cfg, err := loadRawConfigTOML()
    if err != nil {
        return err
    }
    if cfg["remotes"] == nil {
        cfg["remotes"] = map[string]any{}
    }
    remotes := cfg["remotes"].(map[string]any)
    remotes[name] = map[string]any{"url": url}
    return writeRawConfigTOML(cfg)
}

func deleteRemote(name string) error {
    cfg, err := loadRawConfigTOML()
    if err != nil {
        return err
    }
    if remotes, ok := cfg["remotes"].(map[string]any); ok {
        delete(remotes, name)
    }
    return writeRawConfigTOML(cfg)
}

func configPath() string {
    return filepath.Join(config.DefaultDataDir(), "config.toml")
}

func loadRawConfigTOML() (map[string]any, error) {
    data, err := os.ReadFile(configPath())
    if err != nil && !os.IsNotExist(err) {
        return nil, err
    }
    out := map[string]any{}
    if len(data) > 0 {
        if err := toml.Unmarshal(data, &out); err != nil {
            return nil, err
        }
    }
    return out, nil
}

func writeRawConfigTOML(cfg map[string]any) error {
    if err := os.MkdirAll(filepath.Dir(configPath()), 0755); err != nil {
        return err
    }
    b, err := toml.Marshal(cfg)
    if err != nil {
        return err
    }
    return os.WriteFile(configPath(), b, 0644)
}
```

**Step 3: Implement `cmd/ccvault/push.go`**

```go
// ABOUTME: ccvault push [remote] — push local sessions to a configured remote
// ABOUTME: Wires config + DB + remote client together; incremental by default

package main

import (
    "fmt"

    "github.com/2389-research/ccvault/internal/config"
    "github.com/2389-research/ccvault/internal/db"
    "github.com/2389-research/ccvault/internal/remote/client"
    "github.com/spf13/cobra"
)

var pushCmd = &cobra.Command{
    Use:   "push [remote]",
    Short: "Push local sessions to a remote vault",
    Args:  cobra.MaximumNArgs(1),
    RunE: func(cmd *cobra.Command, args []string) error {
        dryRun, _ := cmd.Flags().GetBool("dry-run")

        cfg, err := config.Load()
        if err != nil {
            return err
        }

        remoteName := "origin"
        if len(args) == 1 {
            remoteName = args[0]
        }
        r, ok := cfg.Remotes[remoteName]
        if !ok {
            return fmt.Errorf("remote %q not configured; run `ccvault remote add %s <url>`", remoteName, remoteName)
        }

        cli, err := client.FromRemoteURL(r.URL)
        if err != nil {
            return fmt.Errorf("build client: %w", err)
        }

        database, err := db.Open(cfg.DataDir)
        if err != nil {
            return err
        }
        defer database.Close()

        stats, err := client.Push(cli, database, remoteName, dryRun)
        if err != nil {
            return err
        }

        if dryRun {
            fmt.Printf("[dry-run] would push %d sessions to %s\n", stats.SessionsPushed, remoteName)
        } else {
            fmt.Printf("Pushed %d sessions (%d turns) to %s\n",
                stats.SessionsPushed, stats.TurnsPushed, remoteName)
        }
        return nil
    },
}

func init() {
    pushCmd.Flags().Bool("dry-run", false, "Print what would be pushed without sending")
}
```

**Step 4: Register commands in `cmd/ccvault/main.go`**

In the `init` function, add:

```go
remoteCmd.AddCommand(remoteListCmd, remoteAddCmd, remoteRemoveCmd)
rootCmd.AddCommand(remoteCmd)
rootCmd.AddCommand(pushCmd)
```

**Step 5: Run the whole test suite and hand-verify the CLI**

```bash
go test ./...
go build ./cmd/ccvault
./ccvault remote list
./ccvault remote add origin ccvault@example.invalid
./ccvault remote list
./ccvault remote remove origin
```

Expected: All tests pass; the remote add/list/remove flow prints as expected.

**Step 6: Commit**

```bash
git add cmd/ccvault/remote.go cmd/ccvault/push.go cmd/ccvault/main.go
git commit -m "feat: ccvault remote and push commands"
```

---

## Phase 7: `ccvaultd` entry point

### Task 13: `cmd/ccvaultd/main.go`

**Files:**
- Create: `cmd/ccvaultd/main.go`

**Step 1: Implement**

```go
// ABOUTME: ccvaultd — the group-mode server binary
// ABOUTME: Long-lived SSH server that accepts pushes and serves queries

package main

import (
    "context"
    "flag"
    "fmt"
    "log"
    "net"
    "os"
    "os/signal"
    "syscall"

    "github.com/2389-research/ccvault/internal/db"
    "github.com/2389-research/ccvault/internal/remote/server"
)

const buildVersion = "0.1.0"

func main() {
    var (
        dataDir  = flag.String("data", "/var/lib/ccvaultd", "Data directory (holds ccvault.db, host key, cache)")
        addr     = flag.String("addr", ":2222", "listen address")
        authFile = flag.String("authorized-keys", "/etc/ccvaultd/authorized_keys", "SSH authorized_keys file")
        hostKey  = flag.String("host-key", "", "Path to SSH host key (default: <data>/ssh_host_ed25519_key)")
    )
    flag.Parse()

    if *hostKey == "" {
        *hostKey = fmt.Sprintf("%s/ssh_host_ed25519_key", *dataDir)
    }

    if err := os.MkdirAll(*dataDir, 0700); err != nil {
        log.Fatalf("create data dir: %v", err)
    }

    database, err := db.Open(*dataDir)
    if err != nil {
        log.Fatalf("open db: %v", err)
    }
    defer database.Close()

    authKeys, err := server.LoadAuthorizedKeys(*authFile)
    if err != nil {
        log.Fatalf("load authorized_keys: %v", err)
    }

    hostSigner, err := server.LoadOrGenerateHostKey(*hostKey)
    if err != nil {
        log.Fatalf("host key: %v", err)
    }

    ln, err := net.Listen("tcp", *addr)
    if err != nil {
        log.Fatalf("listen %s: %v", *addr, err)
    }
    log.Printf("ccvaultd %s listening on %s", buildVersion, *addr)

    srv := server.New(database, authKeys, hostSigner, buildVersion)

    // SIGHUP → reload authorized_keys
    sighup := make(chan os.Signal, 1)
    signal.Notify(sighup, syscall.SIGHUP)
    go func() {
        for range sighup {
            if err := authKeys.Reload(); err != nil {
                log.Printf("reload authorized_keys: %v", err)
            } else {
                log.Printf("reloaded authorized_keys from %s", *authFile)
            }
        }
    }()

    // SIGINT/SIGTERM → graceful shutdown
    ctx, cancel := context.WithCancel(context.Background())
    term := make(chan os.Signal, 1)
    signal.Notify(term, syscall.SIGINT, syscall.SIGTERM)
    go func() {
        <-term
        log.Println("shutdown requested")
        cancel()
        _ = ln.Close()
    }()

    if err := srv.Serve(ctx, ln); err != nil {
        log.Fatalf("serve: %v", err)
    }
}
```

**Step 2: Build and smoke-test**

```bash
go build ./cmd/ccvaultd
./ccvaultd --help
```

Expected: Flags print, no runtime errors.

**Step 3: End-to-end manual test (optional, valuable)**

In one terminal:

```bash
mkdir -p /tmp/ccvaultd-test
ssh-keygen -t ed25519 -f /tmp/ccvaultd-test/client_key -N ""
echo "$(cat /tmp/ccvaultd-test/client_key.pub) manual-tester" > /tmp/ccvaultd-test/authorized_keys
./ccvaultd --data /tmp/ccvaultd-test/data \
           --addr :12222 \
           --authorized-keys /tmp/ccvaultd-test/authorized_keys \
           --host-key /tmp/ccvaultd-test/host_key
```

In another:

```bash
ssh -i /tmp/ccvaultd-test/client_key -o StrictHostKeyChecking=no -p 12222 ccvault@localhost version
```

Expected: JSON version response.

**Step 4: Update Makefile**

Modify `Makefile` — add a build target:

```makefile
build-server:
	go build -o bin/ccvaultd ./cmd/ccvaultd

build-all: build build-server
```

**Step 5: Commit**

```bash
git add cmd/ccvaultd/main.go Makefile
git commit -m "feat: cmd/ccvaultd server binary with SIGHUP reload"
```

---

## Phase 8: Query commands on server

### Task 14: `search` handler

**Files:**
- Create: `internal/remote/server/query.go`
- Modify: `internal/remote/server/dispatch.go`
- Test: extend `internal/remote/server/server_test.go`

**Step 1: Write failing test**

Add to `server_test.go`:

```go
func TestServerSearchCommand(t *testing.T) {
    srv, addr, signer, cleanup := StartTestServer(t)
    defer cleanup()

    // Push a session with searchable content
    // ... use the ingest helper from earlier tests to seed "the quick brown fox"

    conn, _ := ssh.Dial("tcp", addr, &ssh.ClientConfig{
        User: "ccvault", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
        HostKeyCallback: ssh.InsecureIgnoreHostKey(),
    })
    defer conn.Close()
    sess, _ := conn.NewSession()
    defer sess.Close()
    stdout, _ := sess.StdoutPipe()
    _ = sess.Start(`search q=fox`)
    // Read ndjson lines, assert one result whose Turn.Content contains "fox".
    // Left as an exercise once handler is implemented.
    _ = srv
    _ = stdout
}
```

**Step 2: Implement `internal/remote/server/query.go`**

```go
// ABOUTME: Read-side handlers: search, sessions, show, stats
// ABOUTME: Reuse internal/search and internal/db as-is; stream results as ndjson

package server

import (
    "encoding/json"
    "fmt"
    "strconv"
    "strings"

    "github.com/2389-research/ccvault/internal/search"
)

func handleSearch(ctx HandlerCtx) int {
    args := parseKV(ctx.Args)
    q := args["q"]
    limit := 20
    if s, ok := args["limit"]; ok {
        if n, err := strconv.Atoi(s); err == nil {
            limit = n
        }
    }
    query := search.Parse(q)
    // Optionally merge additional args (project, model, etc.) into the query — v1 just passes q.
    _ = args // silence unused if we add filters later

    s := search.New(ctx.Server.db.DB)
    results, err := s.Search(query, limit)
    if err != nil {
        fmt.Fprintf(ctx.Stderr, "search: %v\n", err)
        return 1
    }
    enc := json.NewEncoder(ctx.Stdout)
    for _, r := range results {
        if err := enc.Encode(r); err != nil {
            return 1
        }
    }
    return 0
}

func handleSessions(ctx HandlerCtx) int {
    args := parseKV(ctx.Args)
    limit := 50
    if s, ok := args["limit"]; ok {
        if n, err := strconv.Atoi(s); err == nil {
            limit = n
        }
    }
    var projectID int64 = 0
    // TODO: project filter — join through projects table if args["project"] is set
    sessions, err := ctx.Server.db.GetSessions(projectID, limit)
    if err != nil {
        fmt.Fprintf(ctx.Stderr, "sessions: %v\n", err)
        return 1
    }
    enc := json.NewEncoder(ctx.Stdout)
    for _, s := range sessions {
        if err := enc.Encode(s); err != nil {
            return 1
        }
    }
    return 0
}

func handleShow(ctx HandlerCtx) int {
    args := parseKV(ctx.Args)
    id := args["id"]
    if id == "" {
        fmt.Fprintln(ctx.Stderr, "show: id=<session-id> required")
        return 2
    }
    session, err := ctx.Server.db.GetSession(id)
    if err != nil || session == nil {
        fmt.Fprintf(ctx.Stderr, "session not found: %s\n", id)
        return 2
    }
    turns, err := ctx.Server.db.GetTurns(id)
    if err != nil {
        fmt.Fprintf(ctx.Stderr, "get turns: %v\n", err)
        return 1
    }
    resp := map[string]any{"session": session, "turns": turns}
    return writeJSON(ctx, resp)
}

func handleStats(ctx HandlerCtx) int {
    d := ctx.Server.db
    projects, projectTokens, _ := d.GetProjectStats()
    sessions, turns, tokens, _ := d.GetSessionStats()
    first, last, _ := d.GetFirstAndLastActivity()
    tools, _ := d.GetToolUsageStats(10)
    models, _ := d.GetTokensByModel()

    resp := map[string]any{
        "projects":        projects,
        "project_tokens":  projectTokens,
        "sessions":        sessions,
        "turns":           turns,
        "tokens":          tokens,
        "first_activity":  first,
        "last_activity":   last,
        "tool_usage":      tools,
        "tokens_by_model": models,
    }
    return writeJSON(ctx, resp)
}

func writeJSON(ctx HandlerCtx, v any) int {
    if err := json.NewEncoder(ctx.Stdout).Encode(v); err != nil {
        fmt.Fprintln(ctx.Stderr, err)
        return 1
    }
    return 0
}

func parseKV(args string) map[string]string {
    out := map[string]string{}
    for _, part := range strings.Fields(args) {
        if i := strings.Index(part, "="); i >= 0 {
            key := part[:i]
            val := strings.Trim(part[i+1:], `"`)
            out[key] = val
        }
    }
    return out
}
```

**Step 3: Wire handlers into dispatcher**

In `internal/remote/server/dispatch.go`, replace the "not implemented yet" branch:

```go
case "search":
    return handleSearch(ctx)
case "sessions":
    return handleSessions(ctx)
case "show":
    return handleShow(ctx)
case "stats":
    return handleStats(ctx)
```

**Step 4: Run tests**

Run: `go test ./internal/remote/server/... -v`
Expected: PASS (both existing tests and the new search test after fleshing it out).

**Step 5: Commit**

```bash
git add internal/remote/server/query.go internal/remote/server/dispatch.go internal/remote/server/server_test.go
git commit -m "feat: search/sessions/show/stats handlers on ccvaultd"
```

### Task 15: `--remote` flag on client read commands

**Files:**
- Modify: `cmd/ccvault/main.go` — thread a `--remote` flag through `search`, `list-sessions`, `show`, `stats`

**Step 1: Add helper**

Create `cmd/ccvault/remote_dispatch.go`:

```go
// ABOUTME: Small helpers to route a command over SSH to a remote when --remote is set
// ABOUTME: Keeps the per-command Cobra code short and consistent

package main

import (
    "bufio"
    "fmt"
    "io"
    "os"

    "github.com/2389-research/ccvault/internal/config"
    "github.com/2389-research/ccvault/internal/remote/client"
    "github.com/spf13/cobra"
)

// runOnRemote dials the named remote from config, executes the command,
// and copies its stdout to os.Stdout. Returns (ranRemote, error).
// If ranRemote is false, the caller should fall through to local execution.
func runOnRemote(cmd *cobra.Command, command string) (bool, error) {
    name, _ := cmd.Flags().GetString("remote")
    if name == "" {
        return false, nil
    }
    cfg, err := config.Load()
    if err != nil {
        return true, err
    }
    r, ok := cfg.Remotes[name]
    if !ok {
        return true, fmt.Errorf("remote %q not configured", name)
    }
    cli, err := client.FromRemoteURL(r.URL)
    if err != nil {
        return true, err
    }
    reader, _, err := cli.Run(command, nil)
    if err != nil {
        return true, err
    }
    defer reader.Close()
    br := bufio.NewReader(reader)
    _, err = io.Copy(os.Stdout, br)
    return true, err
}
```

**Step 2: Wire into each read command**

For `searchCmd`, add at the top of `RunE`:

```go
if ran, err := runOnRemote(cmd, "search q="+strconv.Quote(strings.Join(args, " "))); ran {
    return err
}
```

And near existing flag setup:

```go
searchCmd.Flags().String("remote", "", "Query a configured remote instead of the local vault")
```

Repeat for `statsCmd`, `listSessionsCmd`, `showCmd` with their respective command strings (e.g. `sessions`, `show id=<id>`, `stats`). Note: the local pretty-printers are bypassed when routed to the remote; the client just streams whatever the server sent. This is acceptable for v1 — output is JSON.

**Step 3: Manual smoke test**

Start `ccvaultd` as in Task 13, push some sessions, then:

```bash
./ccvault --remote origin search "fox"
./ccvault --remote origin stats
```

Expected: JSON output from server.

**Step 4: Commit**

```bash
git add cmd/ccvault/main.go cmd/ccvault/remote_dispatch.go
git commit -m "feat: --remote flag on search/stats/list-sessions/show"
```

---

## Phase 9: Docs

### Task 16: README section for group mode

**Files:**
- Modify: `README.md`

**Step 1: Add a "Group mode" section**

Insert between "MCP Server" and "Claude Code Skill":

```markdown
## Group Mode (Team Vault)

ccvault supports pushing your local vault to a shared team server so a whole
team can search and get analytics on their combined conversation history.

### Server

Run `ccvaultd` on any box the team can reach (SSH — no HTTP or TLS setup required):

    ccvaultd serve --data /var/lib/ccvaultd \
                   --addr :2222 \
                   --authorized-keys /etc/ccvaultd/authorized_keys

Populate `authorized_keys` with one line per authorized developer,
gitolite-style — the trailing comment becomes their attribution identity:

    ssh-ed25519 AAAAC3Nz... alice@company.com
    ssh-ed25519 AAAAC3Nz... bob@company.com

Send SIGHUP after editing to reload.

### Client

    ccvault remote add origin ccvault@vault.company.com
    ccvault push                          # incremental, default remote=origin
    ccvault push --dry-run
    ccvault search "the payments bug" --remote origin
    ccvault stats --remote origin

Uses your ssh-agent (or `~/.ssh/id_ed25519`, `~/.ssh/id_rsa`). No tokens.
```

**Step 2: Commit**

```bash
git add README.md
git commit -m "docs: document group-mode setup in README"
```

---

## Roll-out / verification

After all tasks are complete:

1. Run the full test suite:
   ```bash
   go test ./...
   ```
2. Build both binaries:
   ```bash
   go build ./cmd/ccvault ./cmd/ccvaultd
   ```
3. Run the manual end-to-end smoke test from Task 13.
4. Open a PR from `feature/group-mode` → `main`, using the design doc + implementation plan as the PR description.

---

## Notes for the executor

- The DB layer's `WithTx`, `UpsertProjectTx`, `UpsertSessionTx`, `DeleteTurnsForSessionTx`, `InsertTurnsTx`, `InsertToolUsesTx`, `GetSession`, `GetTurns`, `GetSessions` methods already exist and are exercised by `internal/sync/`. Reuse them — do not duplicate.
- `internal/search` already parses the Gmail-style query grammar and executes against SQLite FTS5. Reuse it in the server-side `search` handler.
- The dispatcher in `internal/remote/server/dispatch.go` deliberately uses a small hand-rolled `key=value` parser instead of pulling in `cobra`. Keep the server surface minimal.
- `tool_uses` push is intentionally deferred in Task 11's MVP push. When adding it, add a `GetToolUsesForSession(id)` DB helper mirroring `GetTurns`, then extend `push.go` to iterate that.
- Known-hosts verification (client side) is stubbed with `InsecureIgnoreHostKey`. When productionizing, wire up `knownhosts.New(~/.ssh/known_hosts)` from `golang.org/x/crypto/ssh/knownhosts`.
