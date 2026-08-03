# Fix #11 — Add bounded reads and raw_json truncation

**Follows on from:** `docs/plans/2026-08-04-issue-11-oversized-jsonl-line.md`
**Issue:** [#11](https://github.com/2389-research/ccvault/issues/11)
**PR:** [#12](https://github.com/2389-research/ccvault/pull/12)
**Branch:** `fix/oversized-jsonl-line` (already open on origin, PR #12)

## Motivation

The already-committed fix on this branch replaced `bufio.Scanner` with `bufio.Reader.ReadBytes('\n')` (no per-line size cap). That solves the reported bug — the session no longer drops. But it leaves two shadow issues:

1. **No upper bound.** A pathological file (a corrupt "JSONL" that's a single 5 GB line, an accidental binary blob) will allocate proportional memory during parse. The original bufio.Scanner threw `ErrTooLong` at 10 MB; the current code has no ceiling at all.

2. **`turns.raw_json` bloat.** A 12 MB base64-PDF line parses fine and lands in the DB as a real turn. `FTS5` doesn't index it (image-block tool_results produce empty `Content`), but the `raw_json` column stores the full 12 MB verbatim. For a user with 100 PDF-read sessions that's ~2.4 GB of dead weight in `ccvault.db`.

Both were called out during PR review. This plan expands PR #12 with a bounded-read layer and a raw_json-truncation layer, keeping the "no config knobs" ethos.

## Thresholds

- **`maxLineBytes = 200 MB`** (hard). Above this, the line is discarded and counted as skipped. Well above real Claude Code PDF pages (~12 MB); well below "your JSONL is corrupt."
- **`maxRawJSONBytes = 1 MB`** (soft). Above this, the turn parses normally but `turn.RawJSON` is replaced with a small placeholder. Below this, `raw_json` is stored verbatim as today.

Both are hard-coded package constants. No env vars, no config file knobs. If we ever need to tune, we tune the constants and cut a release.

## What "truncated raw_json" looks like

The placeholder preserves the top-level shape consumers might look at (`uuid`, `type`, `timestamp`, `sessionId`, `parentUuid`) and adds two marker fields under a `_ccvault_` namespace so downstream tools can detect the truncation:

```json
{
  "_ccvault_stripped": true,
  "_ccvault_original_size": 12583168,
  "uuid": "abc123-...",
  "parentUuid": "def456-...",
  "sessionId": "session-uuid",
  "type": "user",
  "timestamp": "2026-08-04T10:00:01Z"
}
```

~250 bytes worst case. Existing consumers (`internal/tui/conversation.go`, `internal/mcp/server.go`, `internal/export/markdown.go`) that `json.Unmarshal` raw_json into a `RawTurn` will succeed but see no `message` field. All three already have `if content == ""` fallbacks that reach for `turn.Content`, which is unchanged. Net effect: PDF-read turns render as empty in TUI/MCP/export — same as today, since `extractContent` already returns "" for image-block tool_results.

## Approach

### Change 1: `readLineBounded`

Replace the current package-local `readLine` in `pkg/parser/parser.go` with a bounded variant:

```go
// readLineBounded reads a line up to maxBytes bytes. If the line exceeds
// maxBytes, the remainder is drained to '\n' and (nil, true, nil) is returned.
// io.EOF is returned normally when the file ends without a trailing newline.
func readLineBounded(r *bufio.Reader, maxBytes int) ([]byte, bool, error)
```

Implementation uses `bufio.Reader.ReadSlice('\n')` in a loop that accumulates into a growing buffer, checking against `maxBytes` before each append; on overrun, discards the accumulator and drains the rest of the line to `\n` via `ReadSlice` (or byte-by-byte if a chunk is > buffer size). Fast in the common case (no per-byte overhead), bounded in the pathological case.

### Change 2: Wire the cap into `ParseSessionReader`

Loop becomes:

```go
for {
    line, oversized, readErr := readLineBounded(reader, maxLineBytes)
    if oversized {
        skipped++
    } else if len(line) > 0 {
        turn, raw, parseErr := parseTurnInternal(line)
        if parseErr != nil {
            skipped++
        } else if turn != nil {
            if len(line) > maxRawJSONBytes {
                turn.RawJSON = strippedRawJSON(raw, len(line))
                truncated++
            }
            turns = append(turns, *turn)
            updateSessionMetadata(session, turn, raw)
        }
    }
    if readErr != nil {
        if errors.Is(readErr, io.EOF) { break }
        return nil, nil, skipped, truncated, fmt.Errorf(...)
    }
}
```

### Change 3: Return the truncation count

Two options:

**Option A — extend the tuple**: `ParseSessionReader` returns `(turns, session, skipped, truncated, err)`. Ugly. Fifth return.

**Option B — small result struct**: introduce `ParseStats{ SkippedLines, TurnsWithTruncatedRawJSON int }`. Return `(turns, session, stats, err)`. Refactors current 4-tuple to 4-tuple.

Go with **Option B**. Cleaner, extends without further widening.

`stats` value flows through adapters into `ParsedSession.Metadata`:

- `metadata["skipped_lines"] = stats.SkippedLines` (unchanged from current)
- `metadata["turns_with_truncated_raw_json"] = stats.TurnsWithTruncatedRawJSON` (new)

Sync accumulates:

- `Stats.TotalSkippedLines` (unchanged)
- `Stats.SessionsWithSkippedLines` (unchanged)
- `Stats.TurnsWithTruncatedRawJSON` (new — session count, not per-turn count, unless per-turn is trivial)
- `Stats.SessionsWithTruncatedTurns` (new)

Sync summary and `ccvault sync` output report the truncation counts alongside the skipped counts.

### Change 4: Injectable limits for tests

Testing a 200 MB overflow shouldn't allocate 200 MB. Provide an unexported `parseSessionReaderWithLimits(r, sourcePath, maxLine, maxRaw)` used by an internal test helper; the public `ParseSessionReader` becomes a thin wrapper passing the package constants.

## Task list (executes via TDD, one commit per task)

### T1: Introduce `readLineBounded` with unit tests

**Files:**
- `pkg/parser/parser.go` — add `readLineBounded`
- `pkg/parser/parser_test.go` — add `TestReadLineBounded_*` covering:
  - Normal line under cap → returns line, oversized=false
  - Empty line → returns empty, oversized=false
  - EOF without trailing newline → returns line, oversized=false, err=io.EOF
  - Line exceeds cap → returns nil, oversized=true, err=nil (or io.EOF if that's the drain result)
  - Multiple lines mixed: first fine, second oversized, third fine — verify reader position after each

Commit: `feat: bounded line reader for parser (defense against pathological input)`

### T2: Introduce placeholder marshaling with unit tests

**Files:**
- `pkg/parser/parser.go` — add `strippedRawJSON(raw *models.RawTurn, origSize int) []byte`
- `pkg/parser/parser_test.go` — table test asserting the shape:
  - `_ccvault_stripped: true`
  - `_ccvault_original_size` matches input
  - `uuid`, `type`, `sessionId`, `timestamp` preserved
  - `parentUuid` present only when non-empty
  - Result is valid JSON (round-trip through `json.Unmarshal`)
  - Result is small (assert `len(result) < 500`)

Commit: `feat: raw_json placeholder for oversized parsed turns`

### T3: Refactor return signature to `ParseStats`

**Files:**
- `pkg/parser/parser.go` — introduce `ParseStats`, change `ParseSession`/`ParseSessionReader` returns
- `pkg/parser/parser_test.go` — update all existing calls to the new 4-tuple
- `pkg/adapter/claudecode/claudecode.go` — update call, still populate `metadata["skipped_lines"]` from `stats.SkippedLines`
- `pkg/adapter/nanoclaw/nanoclaw.go` — same, both call sites

Existing tests should all still pass. This is a pure signature refactor.

Commit: `refactor: return ParseStats from parser instead of bare int`

### T4: Enforce line cap + `raw_json` truncation

**Files:**
- `pkg/parser/parser.go` —
  - Add package constants `maxLineBytes = 200 << 20`, `maxRawJSONBytes = 1 << 20`
  - Add `parseSessionReaderWithLimits(r, sourcePath, maxLine, maxRaw)` doing the real work
  - `ParseSessionReader` becomes a wrapper that passes the constants
  - Loop uses `readLineBounded`; increments `stats.SkippedLines` on oversized; truncates and increments `stats.TurnsWithTruncatedRawJSON` when `len(line) > maxRaw`
- `pkg/parser/parser_test.go` —
  - `TestParseSessionReaderWithLimits_LineExceedsMaxIsSkipped`: use tiny `maxLine` (say 4 KB), feed a 5 KB line, assert skipped=1, no turns
  - `TestParseSessionReaderWithLimits_TruncatesLargeRawJSON`: use tiny `maxRaw` (say 4 KB), valid line >4 KB, assert turn exists AND `turn.RawJSON` is the placeholder AND `stats.TurnsWithTruncatedRawJSON == 1`
  - Update `TestParseSessionReader_OversizedLineDoesNotDropSession` to also assert:
    - `stats.SkippedLines == 0` (still, because 12 MB < 200 MB)
    - `stats.TurnsWithTruncatedRawJSON == 1` (because 12 MB > 1 MB)
    - The middle turn's `RawJSON` is now the placeholder shape
  - Update `TestParseSessionReader_MalformedLinesAreSkippedAndCounted` for the new signature — still asserts skipped == 2

Commit: `feat: cap line reads at 200 MB and truncate raw_json above 1 MB`

### T5: Surface truncation in sync stats and CLI summary

**Files:**
- `internal/sync/sync.go` —
  - Add `Stats.SessionsWithTruncatedTurns int` and `Stats.TurnsWithTruncatedRawJSON int`
  - In `processSession`, if metadata has `turns_with_truncated_raw_json`, accumulate
  - Sync progress log: report truncation count alongside skipped-line count
- `cmd/ccvault/main.go` — print truncation counts in the `sync` command's summary

Commit: `feat: surface raw_json truncation counts in sync stats`

### T6: Integration test — full cycle with a huge line

**Files:**
- `test/integration/oversized_line_test.go` — extend `TestSync_OversizedJSONLLine_IndexesAndRecordsMtime` or add a companion test:
  - Assert `stats.TurnsWithTruncatedRawJSON == 1` after first sync
  - Assert `stats.SessionsWithTruncatedTurns == 1`
  - Load the persisted turn from the DB, unmarshal its `raw_json`, verify `_ccvault_stripped == true`

Commit: `test: verify raw_json truncation persists end-to-end`

### T7: Full suite

Run `go test ./... -race -count=1`. Expected: green.

### T8: Update PR #12

The branch already has an upstream tracker. Just `git push` — PR #12 auto-refreshes. Optionally amend the PR body to describe the new commits (bounded reads + truncation).

Commit is not needed for the PR body update; use `gh pr edit 12 --body-file <path>` after pushing.

## Explicitly NOT doing

- No env vars, no `CCVAULT_MAX_LINE_BYTES`, no `[parser]` config section. Hard-coded constants only.
- No compression of `raw_json` (e.g., gzip). Truncation is enough for the reported concern; compression is a much bigger change with rebuild-index implications.
- No structural payload-stripping (walk JSON, replace inner base64 blobs). Complex, format-fragile, out of scope.
- No spilling large payloads to sidecar files. Over-engineered for the storage volumes we're seeing.
- No changes to `internal/sync`'s error-branch mtime handling. The parser doesn't return errors for the reported cases anymore, so the branch is unreachable — leave it.
- No new dependencies.

## Verification

After the PR is updated:

1. Manual: run `ccvault sync --full` against a Claude Code home that contains at least one PDF-read session. Confirm:
   - Session indexes.
   - Sync summary reports non-zero `Turns with truncated raw_json`.
   - `sqlite3 ~/.ccvault/ccvault.db 'SELECT LENGTH(raw_json) FROM turns ORDER BY LENGTH(raw_json) DESC LIMIT 5;'` shows no rows above ~500 bytes for the truncated turns (vs. the pre-fix pattern of 12+ MB rows).

2. Automated: `go test ./... -race -count=1` clean.
