# Group Mode Design — Remote Team Vault

## Goal

Add a "group mode" to ccvault so a whole team can share a searchable vault of conversations. Each developer keeps their own local ccvault as today, and additionally pushes to one or more shared remote vaults. Team members query the remote vault for both full-text search (use case: institutional knowledge) and analytics (use case: team-level usage observability).

## Design Decisions

- **UX metaphor: git remotes.** Named remotes, one-way push, no pull, no merge. `ccvault remote add`, `ccvault push`, no branching semantics.
- **Trust model: closed team, no client-side filtering.** Same as a private git repo. If a dev's local session contains a secret, that's a people/process concern, not something ccvault will try to scrub. Simplicity beats false-safety here.
- **Direction: one-way push only.** No pull-back. Devs' local DBs remain their private view. Team queries hit the remote directly.
- **Transport: SSH.** Not HTTP+bearer. Every dev already has an SSH key; keys live in `~/.ssh` with proper permissions; ssh-agent/passphrase/yubikey story is free. No plaintext secrets in `~/.ccvault/config.toml`. Public-key identity gives us rock-solid attribution for `pushed_by`.
- **Server: new binary `ccvaultd` in the same repo.** Shares `pkg/models`, `internal/db` (schema, migrations, FTS5 triggers), `internal/search`. The server is effectively "ccvault as a daemon with an ingest command bolted on." Same schema on both sides.
- **Incremental push, tracked locally.** New table `remote_push_state` records `(remote_name, session_id, session_ended_at, pushed_at)`. `ccvault push` sends anything newer or unknown.
- **v1 excludes MCP-on-server.** CLI/TUI-against-remote is the query surface for v1. HTTP + MCP can be added later as a second listener alongside the SSH one, sharing handler layer.

## Architecture

```
[dev A laptop]  ─┐   ssh-agent + ~/.ssh/id_*
[dev B laptop]  ─┼── SSH (embedded x/crypto/ssh) ──▶  [ccvaultd]  ── SQLite+FTS5  /var/lib/ccvaultd
[dev C laptop]  ─┘                                                └─ Parquet cache (rebuilt on push)
```

- Each dev's `ccvault` binary is unchanged for local sync; grows a `remote` subcommand and a `push` subcommand, plus a `--remote` flag on read commands.
- `ccvaultd` is a new binary in `cmd/ccvaultd/`. Same repo, same module. One `docker run` deploys it.
- Server sits behind whatever network the team uses (Tailscale, VPN, direct IP, homelab). SSH provides its own transport encryption; no reverse proxy or TLS termination needed.

## UX

### Client-side (existing `ccvault` binary)

```
ccvault remote add origin ccvault@vault.company.com
ccvault remote add origin ssh://vault.company.com:2222/team
ccvault remote list
ccvault remote remove origin

ccvault push [remote]                   # incremental push; default remote=origin
ccvault push --dry-run                  # list what would be sent

ccvault search "foo" --remote origin    # query the remote
ccvault stats --remote origin
ccvault list-sessions --remote origin
ccvault show <id> --remote origin
```

Remotes live in the existing `~/.ccvault/config.toml`:

```toml
[remotes.origin]
url = "ccvault@vault.company.com"
# no token needed — SSH handles auth
```

Client uses standard ssh-agent lookup; falls back to `~/.ssh/id_ed25519`; prompts for passphrase if needed. Standard SSH client behavior throughout.

### Server-side (new `ccvaultd` binary)

```
ccvaultd serve --data /var/lib/ccvaultd --addr :2222 \
               --authorized-keys /etc/ccvaultd/authorized_keys \
               --host-key /var/lib/ccvaultd/ssh_host_ed25519_key

ccvaultd migrate       # apply pending migrations to the server DB (idempotent)
```

`authorized_keys` file, gitolite-style, one pubkey per line. The comment field is the identity string used for `pushed_by`:

```
ssh-ed25519 AAAAC3Nz... alice@2389.ai
ssh-ed25519 AAAAC3Nz... bob@2389.ai
```

Host key is auto-generated on first launch if missing. Clients get standard `known_hosts` prompts on first connect — the same friction they've experienced with every git host.

## Data Model Changes

### Local DB (new table)

```sql
CREATE TABLE remote_push_state (
    remote_name       TEXT NOT NULL,
    session_id        TEXT NOT NULL,
    session_ended_at  DATETIME NOT NULL,   -- so we know when a re-push is warranted
    pushed_at         DATETIME NOT NULL,
    PRIMARY KEY (remote_name, session_id)
);

CREATE INDEX idx_remote_push_state_pushed_at
    ON remote_push_state(remote_name, pushed_at);
```

Push algorithm: for each row in `sessions`, push if there is no matching row in `remote_push_state`, or if `sessions.ended_at > remote_push_state.session_ended_at`. Mirrors the incremental pattern used today by `source_file_mtimes` for local sync.

### Server DB (new column on `sessions`)

```sql
ALTER TABLE sessions ADD COLUMN pushed_by TEXT NOT NULL DEFAULT '';
```

`pushed_by` is filled in from the SSH connection's authenticated identity (the comment field on the matched pubkey). Enables per-user analytics: "who's pushing what," "who's spending Opus tokens," etc.

Everything else is the exact same schema as local — sessions, turns, tool_uses, projects, FTS5 tables and triggers, source_file_mtimes (unused server-side but harmless). Migrations run on server startup.

## Wire Protocol

The client opens an SSH session and issues one of a small set of commands. Body (if any) is newline-delimited JSON on stdin; response is newline-delimited JSON on stdout.

### `ingest`

```
$ ssh vault.company.com ingest
{"kind":"session",     "session":{"id":"abc123", "project_path":"...", ...}}
{"kind":"turn",        "turn":{"id":"...", "session_id":"abc123", ...}}
{"kind":"turn",        "turn":{...}}
{"kind":"tool_use",    "tool_use":{...}}
{"kind":"session_end", "session_id":"abc123"}
{"kind":"session",     "session":{"id":"def456", ...}}
...
```

Server buffers each session's records until the `session_end` marker, then transactionally upserts: delete-then-insert on `(source, session_id)`, stamping `pushed_by` from the SSH identity. Session becomes visible only after `session_end`; interrupted pushes leave the server unchanged for that session. "Latest push wins" semantics fall out for free.

### `search`, `sessions`, `show`, `stats`

Query commands mirror the MCP tool surface. Each accepts key=value args after the command name and streams ndjson responses:

```
$ ssh vault.company.com search q="error handling" project="foo"
{"session_id":"abc123", "snippet":"...", "matched_at":"..."}
{"session_id":"def456", "snippet":"...", "matched_at":"..."}

$ ssh vault.company.com stats
{"total_sessions":1234, "total_turns":56789, ...}
```

## Server Binary Shape

`cmd/ccvaultd/main.go` — a small entry point that:

1. Parses flags (`--data`, `--addr`, `--authorized-keys`, `--host-key`).
2. Opens the SQLite DB, runs migrations.
3. Loads authorized keys into memory (also watches the file for changes; SIGHUP re-reads).
4. Starts an embedded SSH server using `golang.org/x/crypto/ssh`.
5. For each accepted connection, matches the pubkey against authorized keys, records the identity, dispatches on the command string to a handler.

New shared packages:

- `internal/remote/protocol/` — ndjson message types, shared between client and server.
- `internal/remote/client/` — SSH dialer, connection helpers, `Push`/`Search`/`Stats` client-side calls used by `ccvault --remote` codepaths.
- `internal/remote/server/` — SSH server, command dispatch, ingest+query handlers. Reused by `cmd/ccvaultd`.

Existing packages the server reuses unchanged:

- `pkg/models`, `pkg/adapter` (indirectly, via serialized models)
- `internal/db` (schema, migrations, all upserts, transactions, FTS5)
- `internal/search` (query grammar + execution)
- `internal/analytics` (Parquet cache; server rebuilds on push instead of on-demand)

## Error Handling & Edge Cases

- **Partial push interrupted.** Session is visible only after `session_end`. Next `ccvault push` finds the missing `remote_push_state` row and re-pushes. Idempotent.
- **Unknown pubkey.** SSH returns `Permission denied (publickey)`. Client exits nonzero, local `remote_push_state` not updated. Standard SSH error.
- **Server unreachable.** Retry with backoff (3 attempts, 1s / 5s / 30s), then fail. Local DB unchanged.
- **Schema drift.** Server exposes a `version` command; client checks compatibility on connect. Client refuses to push to a server whose schema version is older than the client's. (Server upgrades that add new columns are backward-compatible in the other direction.)
- **Session deleted locally.** No propagation. We don't have "delete" semantics in v1. Simplest thing that works.
- **Host key change.** Standard `known_hosts` warning surfaced through the SSH client library. User resolves the same way they'd resolve a warning from a git host.
- **Concurrent pushes from multiple clients.** Server serializes writes per session (delete-then-insert inside a transaction). Different sessions can commit concurrently. Latest push wins.
- **Rotating a compromised key.** Admin removes the line from `authorized_keys`, sends SIGHUP or restarts. That client's connections fail on next attempt.

## Testing Plan

- **Unit — `internal/remote/protocol/`.** Table tests for message encoding/decoding, error surfaces on malformed input.
- **Unit — `internal/remote/client/`.** Retry logic, ndjson streaming, command formatting. In-process SSH server fixture.
- **Integration — end-to-end push.** Spin up `ccvaultd` on a tempdir + random port with a test host key and a test authorized_keys file. Run `ccvault push` against it. Assert the server DB matches the client DB row-for-row (sessions, turns, tool_uses, `pushed_by` populated correctly).
- **Integration — end-to-end query.** After push, run `ccvault search --remote testremote` and assert results equal a direct search on the server DB.
- **Integration — two-client convergence.** Two independent client DBs, each with disjoint sessions, both push to the same server. Assert server has union of both, correctly attributed via `pushed_by`.
- **Integration — resumability.** Kill the client mid-push, restart, assert the interrupted session is re-pushed and reaches the server correctly.

All tests use real SQLite, real SSH, real disk. No mocks — matches the project's standing rule against mocking.

## Out of Scope (the NOT-doing line)

- No pull, no bidirectional sync, no branch/tag semantics.
- No MCP-on-server. Deferred; when it lands, an HTTP listener runs alongside the SSH one, sharing the handler layer.
- No web UI. No dashboard. No browser-facing surface.
- No multi-tenancy. One `ccvaultd` process serves one team's data.
- No client-side redaction or secret-scrubbing. Trust the team.
- No key management CLI on the server (`ccvaultd key add …`). Admin edits `authorized_keys` by hand or via config management. Matches SSH conventions.
- No delete propagation.
- No push signing beyond the SSH handshake.

## Roll-out

1. Land the `remote_push_state` migration and `ccvault remote` / `ccvault push` client commands.
2. Land `cmd/ccvaultd/` server with ingest.
3. Land query commands (`search`, `stats`, `list-sessions`, `show`) with `--remote` flag on the client, and matching server-side dispatch.
4. Add TUI affordance for browsing a remote (deferred until step 3 is proven useful).
