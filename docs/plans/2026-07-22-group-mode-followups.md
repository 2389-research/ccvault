# Group Mode Follow-ups

Deferred items from the fresh-eyes review of the `feature/group-mode` branch. The four Important issues (I1–I4) were fixed in commit `088982f`; everything below is safe to defer past v1.

## Borderline

### F1. Local flag propagation on `--remote` reads
`cmd/ccvault/remote_dispatch.go`. Today, `--remote` routes to the server and dumps raw ndjson to stdout. Local `--limit`, `--json`, `--project`, `--sort` flags are silently ignored, and the local pretty-printers are bypassed. Options:
- Plumb `--limit` and `--json` through to the server command string; default remote output to ndjson but respect `--json` (already the format) and pretty-print when neither is set.
- Or accept the current behavior and document it in the group-mode README.

## Minor

### F2. Dead `SearchResult` alias in protocol package
`internal/remote/protocol/messages.go:43`. The alias `type SearchResult = models.SearchResult` isn't used anywhere; the server encodes `search.Result` (from `internal/search`). Either delete the alias or wire it up.

### F3. Unbounded ingest buffer growth
`internal/remote/server/ingest.go`. A client that sends many `KindSession` messages without matching `KindSessionEnd` grows the per-connection map without limit. Closed-trust model makes this robustness, not security. Cap concurrent buffered sessions and per-session turn count.

### F4. Session ID / project path collisions across `pushed_by`
Design says "latest push wins" — that also silently overwrites `pushed_by`. Future audit column or a reject-on-collision mode would preserve provenance across teams.

### F5. Orphaned `remote_push_state` rows on `remote remove`
`cmd/ccvault/remote.go`. `deleteRemote` drops the TOML entry but leaves push-state rows behind. Re-adding a remote of the same name pointing at a different server makes the client think everything is already pushed. Fix: GC rows on `remote remove`, or clear on `remote add` when the URL changes.

### F6. No URL validation on `remote add`
`cmd/ccvault/remote.go:41`. Any string is written verbatim to config; errors surface only at push time. Cheap fix: call `client.FromRemoteURL(url)` in `writeRemote` before persisting.

### F7. `remote_dispatch` swallows exit codes
`cmd/ccvault/remote_dispatch.go:42-45`. `sess.Wait()` is only implicit via `Close()`; the return is not checked. A `show` for a nonexistent session on the server returns exit 2 with stderr, but the client prints nothing and exits 0.

### F8. `make release` doesn't build `ccvaultd`
`Makefile:41-45`. Cross-compilation targets only `./cmd/ccvault`. Ops that installs from release artifacts gets no server binary.

### F9. `bufio.NewReader(reader)` noise in remote_dispatch
`cmd/ccvault/remote_dispatch.go:43-44`. The `bufio.NewReader` wrap before `io.Copy` is a no-op. Replace with a plain `io.Copy(os.Stdout, reader)`.

### F10. `handleSessions` ignores `--project`
`internal/remote/server/query.go:48-49`. Hardcoded `projectID = 0`; server has no project filter. Note the limitation in the README until this lands.

### F11. Brittle field assertion in search e2e test
`internal/remote/server/server_test.go`. Asserts on `r["session_id"]` at the top level. If someone nests the response under `session`, the test silently starts passing without verifying anything. Decode into a typed struct or assert on multiple fields.

### F12. Non-graceful shutdown of active connections
`cmd/ccvaultd/main.go:76-84`. SIGINT/SIGTERM cancels the context and closes the listener, but active SSH connections aren't drained. In-flight ingest or search gets dropped. Fine for v1; document that graceful shutdown is best-effort or add a drain timeout.

### F13. `ccvaultd` uses `flag` package, not cobra
`cmd/ccvaultd/main.go:22-33`. Inconsistent with the rest of the codebase. `ccvaultd migrate` is mentioned in the design doc but not yet implemented; cobra will be the easier surface when that lands.

## Deferrals still deliberate (do not fix here)

These were called out in the design doc as v1-out-of-scope and are still intentional:
- `known_hosts` verification on the client (still `InsecureIgnoreHostKey`)
- MCP-on-server
- Push of `tool_uses` (server accepts them; client `push.go` doesn't send them yet)
- Retry + exponential backoff on failed pushes
- TUI support for `--remote` browsing
- Delete propagation
- Multi-tenancy in `ccvaultd`
