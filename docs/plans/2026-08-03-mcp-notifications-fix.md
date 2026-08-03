# MCP Notifications Handling Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Fix the MCP server so JSON-RPC notifications (messages without an `id`) never receive a response, resolving the reported Zod-validation failure in Claude Code that leaves ccvault's MCP integration non-functional.

**Architecture:** Two-part fix in `internal/mcp/server.go`. (1) Inject an `io.Writer` into `Server` for output so tests can inspect what gets written without spawning a subprocess. (2) Add explicit no-op cases for MCP's well-known client-to-server notifications (`notifications/initialized`, `notifications/cancelled`, `notifications/roots/list_changed`), and guard the `default` branch on `req.ID == nil` so any unknown notification also produces zero output. The parse-error path stays as-is (JSON-RPC 2.0 §5 says parse errors respond with `id: null`).

**Tech Stack:** Go 1.24, `encoding/json`, stdlib `bufio`/`io`, `modernc.org/sqlite`. No new deps.

**Empirical evidence:** Reporter's exact repro against `main` at `1b70cd1` reproduces byte-for-byte — see the issue transcript. Additional probes confirmed `notifications/cancelled` and `notifications/roots/list_changed` hit the same buggy default-branch response path.

**Scope:**
- ✅ Fix notification handling for all known + unknown notifications
- ✅ Add unit tests using injected output writer
- ✅ Verify with the reporter's original repro against a fixed binary
- ❌ **Not doing:** JSON-RPC batch support (probe C — file as separate issue)
- ❌ **Not doing:** `jsonrpc` version field validation (probes D + E — permissive is acceptable)

---

## Working conventions

- Branch: `fix/mcp-notifications` (already created off main)
- Commits after each task; conventional-commit style matching the repo
- Pre-commit hooks must pass (never `--no-verify`)
- ABOUTME comments on new files (both lines starting with `// ABOUTME: `)
- Tests use real dependencies (no mocks) per the project's standing rule; a `bytes.Buffer` as `io.Writer` is not a mock, it's a real writer

---

## Task 1: Refactor Server to inject output writer

**Files:**
- Modify: `internal/mcp/server.go` — add `out io.Writer` field, use it in `send()`, default to `os.Stdout` in `NewServer`

Read the current `Server` struct first to see its exact shape. It lives near the top of the file (roughly `type Server struct { … }`).

**Step 1: Add `out io.Writer` field to `Server`**

Locate the `type Server struct` declaration. Add a new field:

```go
type Server struct {
    // ... existing fields
    out io.Writer  // stdout by default; overridable for tests
}
```

**Step 2: Default `out` in `NewServer`**

Locate `func NewServer(...)` and set `out: os.Stdout` on the returned `&Server{...}` literal. Preserve every existing field assignment.

**Step 3: Route `send()` through the field**

Locate `func (s *Server) send(v interface{})` at roughly `internal/mcp/server.go:1391`. Replace `fmt.Println(string(data))` with:

```go
_, _ = fmt.Fprintln(s.out, string(data))
```

The trailing newline stays — MCP is line-delimited JSON.

**Step 4: Verify build + existing tests still pass**

Run:
```bash
go build ./...
go test ./internal/mcp/... 2>&1 | tail -5
```

Expected: build clean, `[no test files]` (the mcp package currently has no tests). If `go build` errors on the `io.Writer` reference, ensure `io` is imported at the top of `server.go`.

**Step 5: Commit**

```bash
git add internal/mcp/server.go
git commit -m "refactor: inject io.Writer into MCP Server for testable output"
```

---

## Task 2: Fix notification handling in the dispatcher

**Files:**
- Modify: `internal/mcp/server.go:206-229` — `handleRequest` function

**Step 1: Study the current `handleRequest`**

Current shape:

```go
func (s *Server) handleRequest(req *jsonRPCRequest) {
    s.log("Handling method: %s", req.Method)

    switch req.Method {
    case "initialize":
        s.handleInitialize(req)
    case "initialized":
        // Notification, no response needed
        s.log("Client initialized")
    case "ping":
        s.sendResult(req.ID, map[string]interface{}{})
    case "tools/list":
        s.handleToolsList(req)
    case "tools/call":
        s.handleToolsCall(req)
    case "prompts/list":
        s.handlePromptsList(req)
    case "prompts/get":
        s.handlePromptsGet(req)
    default:
        s.log("Unknown method: %s", req.Method)
        s.sendError(req.ID, -32601, "Method not found", req.Method)
    }
}
```

The two bugs:
- `case "initialized":` never matches — MCP uses `"notifications/initialized"` (with prefix). Dead code.
- `default:` calls `sendError` unconditionally. For notifications (no `id`), this violates JSON-RPC 2.0.

**Step 2: Replace the function with the fixed version**

```go
func (s *Server) handleRequest(req *jsonRPCRequest) {
    s.log("Handling method: %s", req.Method)

    switch req.Method {
    case "initialize":
        s.handleInitialize(req)
    case "notifications/initialized":
        // Client → server notification per MCP spec. No response.
        s.log("Client initialized")
    case "notifications/cancelled":
        // Client cancelling an in-flight request. We don't have long-running
        // work today, so just log. No response (notification).
        s.log("Client cancelled request: %s", string(req.Params))
    case "notifications/roots/list_changed":
        // Client roots changed. We don't consume roots, so log-only. No response.
        s.log("Client roots changed")
    case "ping":
        s.sendResult(req.ID, map[string]interface{}{})
    case "tools/list":
        s.handleToolsList(req)
    case "tools/call":
        s.handleToolsCall(req)
    case "prompts/list":
        s.handlePromptsList(req)
    case "prompts/get":
        s.handlePromptsGet(req)
    default:
        // JSON-RPC 2.0: notifications (messages without an id) MUST NOT
        // receive a response, even for unknown methods. Only respond when
        // the caller supplied an id, indicating a request.
        s.log("Unknown method: %s", req.Method)
        if req.ID == nil {
            return
        }
        s.sendError(req.ID, -32601, "Method not found", req.Method)
    }
}
```

**Step 3: Verify build**

```bash
go build ./...
```

Expected: clean build.

**Step 4: Hand-run the reporter's repro against the fixed binary**

```bash
go build -o .scratch/ccvault ./cmd/ccvault
(
  echo '{"jsonrpc":"2.0","method":"initialize","id":1,"params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}}}'
  echo '{"jsonrpc":"2.0","method":"notifications/initialized"}'
) | .scratch/ccvault mcp
```

Expected: **exactly one line of output** — the `initialize` result. The notification produces no response.

Before the fix, this produced two lines; the second was an `id:null` error that broke Claude Code.

**Step 5: Commit**

```bash
git add internal/mcp/server.go
git commit -m "fix: notifications must not receive JSON-RPC responses (#7)"
```

---

## Task 3: Add unit tests for the notification behavior

**Files:**
- Create: `internal/mcp/server_test.go`

**Step 1: Write the test file**

The mcp package has zero tests today. Create `internal/mcp/server_test.go` with:

```go
// ABOUTME: Tests for the MCP JSON-RPC server — notification handling per issue #7.
// ABOUTME: Uses the injected io.Writer to inspect exactly what bytes are emitted.

package mcp

import (
    "bytes"
    "encoding/json"
    "strings"
    "testing"
)

// newTestServer builds a Server with just enough state to exercise the
// dispatch layer. The db is nil because notification handlers don't touch it.
// If a future test needs the DB, use db.Open(t.TempDir()) instead.
func newTestServer(t *testing.T) (*Server, *bytes.Buffer) {
    t.Helper()
    buf := &bytes.Buffer{}
    return &Server{out: buf}, buf
}

func TestServer_NotificationsInitialized_IsSilent(t *testing.T) {
    // Reporter's exact repro shape.
    s, buf := newTestServer(t)
    s.handleRequest(&jsonRPCRequest{
        JSONRPC: "2.0",
        Method:  "notifications/initialized",
    })
    if buf.Len() != 0 {
        t.Errorf("notification produced a response: %q", buf.String())
    }
}

func TestServer_NotificationsCancelled_IsSilent(t *testing.T) {
    s, buf := newTestServer(t)
    s.handleRequest(&jsonRPCRequest{
        JSONRPC: "2.0",
        Method:  "notifications/cancelled",
        Params:  json.RawMessage(`{"requestId":1}`),
    })
    if buf.Len() != 0 {
        t.Errorf("notification produced a response: %q", buf.String())
    }
}

func TestServer_NotificationsRootsListChanged_IsSilent(t *testing.T) {
    s, buf := newTestServer(t)
    s.handleRequest(&jsonRPCRequest{
        JSONRPC: "2.0",
        Method:  "notifications/roots/list_changed",
    })
    if buf.Len() != 0 {
        t.Errorf("notification produced a response: %q", buf.String())
    }
}

func TestServer_UnknownNotification_IsSilent(t *testing.T) {
    // Any unrecognized method with no id must produce no output, per the
    // JSON-RPC 2.0 notification rule. Guards the default-branch fix.
    s, buf := newTestServer(t)
    s.handleRequest(&jsonRPCRequest{
        JSONRPC: "2.0",
        Method:  "notifications/some/future/thing",
    })
    if buf.Len() != 0 {
        t.Errorf("unknown notification produced a response: %q", buf.String())
    }
}

func TestServer_UnknownRequest_ReturnsErrorWithID(t *testing.T) {
    // Regression guard: an unknown REQUEST (has id) must still receive
    // a proper -32601 Method not found. The fix should only suppress
    // responses when id is nil.
    s, buf := newTestServer(t)
    s.handleRequest(&jsonRPCRequest{
        JSONRPC: "2.0",
        ID:      float64(42), // json.Unmarshal turns numeric ids into float64
        Method:  "resources/list",
    })
    var resp map[string]any
    if err := json.Unmarshal(buf.Bytes(), &resp); err != nil {
        t.Fatalf("response is not valid JSON: %q", buf.String())
    }
    if resp["id"] != float64(42) {
        t.Errorf("response id = %v, want 42", resp["id"])
    }
    errObj, ok := resp["error"].(map[string]any)
    if !ok {
        t.Fatalf("no error object in response: %v", resp)
    }
    if errObj["code"] != float64(-32601) {
        t.Errorf("error code = %v, want -32601", errObj["code"])
    }
}

func TestServer_Initialize_ReturnsExpectedShape(t *testing.T) {
    // Regression guard: the happy-path handler still returns a
    // spec-shaped result. Verifies our fix didn't break the working
    // request path.
    s, buf := newTestServer(t)
    s.handleRequest(&jsonRPCRequest{
        JSONRPC: "2.0",
        ID:      float64(1),
        Method:  "initialize",
        Params:  json.RawMessage(`{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"1"}}`),
    })
    body := buf.String()
    for _, want := range []string{`"id":1`, `"result"`, `"protocolVersion":"2024-11-05"`, `"serverInfo"`} {
        if !strings.Contains(body, want) {
            t.Errorf("initialize response missing %q; got: %s", want, body)
        }
    }
}
```

**Step 2: Run the tests**

```bash
go test ./internal/mcp/... -v
```

Expected: all six tests pass.

**Step 3: Run the tests with race detector**

```bash
go test ./internal/mcp/... -race -count=1
```

Expected: pass.

**Step 4: Commit**

```bash
git add internal/mcp/server_test.go
git commit -m "test: verify MCP server handles notifications per JSON-RPC 2.0 (#7)"
```

---

## Task 4: Full-suite regression check

**Files:**
- No files modified.

**Step 1: Run the full suite with the race detector**

```bash
go test ./... -race -count=1 2>&1 | tail -20
```

Expected: every package `ok`. If anything red, halt and diagnose — the fix is small enough that a regression elsewhere would be surprising, but worth catching before pushing.

**Step 2: Re-run the reporter's exact repro one more time**

```bash
go build -o .scratch/ccvault ./cmd/ccvault
(
  echo '{"jsonrpc":"2.0","method":"initialize","id":1,"params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}}}'
  echo '{"jsonrpc":"2.0","method":"notifications/initialized"}'
) | .scratch/ccvault mcp
```

Expected: one line of output (the initialize result). No `id:null` error line.

**Step 3: Also probe the two other broken notifications**

```bash
echo '{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}' | .scratch/ccvault mcp
echo "exit=$?"
echo '{"jsonrpc":"2.0","method":"notifications/roots/list_changed"}' | .scratch/ccvault mcp
echo "exit=$?"
```

Expected: **zero output** in both cases, exit 0.

**Step 4: No commit for this task** — it's verification only.

---

## Task 5: Push branch + open PR

**Files:**
- No file changes.

**Step 1: Push**

```bash
git push -u origin fix/mcp-notifications
```

**Step 2: Open PR referencing #7**

```bash
gh pr create --title "fix: MCP notifications must not receive JSON-RPC responses (#7)" --body "..."
```

Body should include:
- Summary linking to #7
- Verification: repro before/after
- List of the three well-known notifications now handled
- Test plan checklist
- Not-doing section: batch support + jsonrpc version validation (with recommendation to file as separate issues)

**Step 3: Queue auto-merge**

```bash
gh pr merge <PR#> --auto --merge
```

Same pattern used for PRs #12, #14, #15, #16.

---

## Explicitly NOT doing

- **JSON-RPC batch support** — spec-defined but out of scope for the reported bug. If any real client sends a batch we'll hear about it; separate issue.
- **`jsonrpc` version field validation** — being permissive with missing/wrong `jsonrpc` field is a mild deviation but robustness-preserving; no compelling reason to tighten.
- **Response formatting refactor** — `sendResult` / `sendError` still work fine, just fanned through the new `s.out` writer.
- **Public API changes** — `NewServer` signature stays the same; the `out` field is unexported.

## Verification story

After this PR is merged:
1. Reporter's original repro produces one line (initialize result) instead of two (result + id:null error).
2. Claude Code no longer drops the transport when connecting to ccvault's MCP server.
3. Tests catch regressions on any future dispatcher edit.
