# Fix #11 — Oversized JSONL Line Drops Entire Session

**Issue:** [#11](https://github.com/2389-research/ccvault/issues/11) — sync: one oversized JSONL line discards the entire session file
**Branch:** `fix/oversized-jsonl-line`

## Summary

A single JSONL line exceeding `bufio.Scanner`'s 10 MB cap causes `ParseSession` to abort with `bufio.ErrTooLong`, discarding all turns from that file. Because `sync.processSession` never records the mtime on parse failure, subsequent syncs retry and fail forever. Real-world trigger: Claude Code stores PDF read results as base64 image blocks in a single JSONL line; an ~8 MB PDF produces two ~12 MB lines.

## Root cause (both halves)

**A. Parser uses a fixed-cap scanner** — `pkg/parser/parser.go:36-39`:

```go
scanner := bufio.NewScanner(r)
buf := make([]byte, 0, 1024*1024)
scanner.Buffer(buf, 10*1024*1024) // 10MB max
```

When a line exceeds the cap, `scanner.Scan()` returns false and `scanner.Err()` returns `bufio.ErrTooLong`. `bufio.Scanner` cannot resume by design.

**B. Sync fails closed on parse errors** — `internal/sync/sync.go:185-188`:

```go
parsed, err := adpt.Parse(sf.Path)
if err != nil {
    return fmt.Errorf("parse session: %w", err)
}
```

No mtime upsert on this path. Every subsequent sync re-attempts the same file.

Note precedent at `sync.go:190-195`: empty sessions record mtime and get skipped. We already tolerate "unusable session, don't retry" — just not the oversized-line variant.

## Approach

1. Replace `bufio.Scanner` with `bufio.Reader` + `ReadBytes('\n')` in `ParseSessionReader`. Reference implementation: `pkg/adapter/util.go:19` and its usage in `pkg/adapter/codex/codex.go`. Same shape; no cap.
2. When a line fails JSON parsing (`parseTurnInternal` returns error), skip it — same as today's behavior.
3. Count skipped lines so the sync layer can surface them.
4. Return skipped-line info alongside session/turns from `ParseSessionReader`.
5. Have `pkg/adapter/claudecode` thread that count through into `ParsedSession.Metadata["skipped_lines"]`.
6. Have `internal/sync` accumulate skipped lines into `Stats` and surface in the summary.

## Why not the alternatives

- **Raise the 10 MB cap** — pushes the same failure to a bigger file. Reporter explicitly called this out.
- **Import `pkg/adapter.ReadLine` into `pkg/parser`** — package layering: `pkg/adapter` wraps `pkg/parser`, not vice versa. Duplicating the ~5-line helper into `pkg/parser` is cleaner.
- **Fix only Bug B (record mtime on parse failure)** — silently drops the entire session forever. Worse than status quo for the reporter's case (2 bad lines out of 173).

## Task list (executes via TDD, one commit per task)

### T1: Repro test — oversized line drops session

**Files:**
- `pkg/parser/parser_test.go` — add `TestParseSessionReader_OversizedLineDoesNotDropSession`

Construct input with three lines: normal user turn, one line ~12 MB long (JSON-parseable garbage, e.g. a giant `{"type":"user","message":{"role":"user","content":"AAAA...AAAA"}}`), normal assistant turn. Call `ParseSessionReader`. Expected under the new behavior: no error, 2 turns returned, skipped-count == 1.

Under current behavior this fails with `bufio.ErrTooLong` — that's our red bar.

Commit: `test: repro #11 — oversized JSONL line should not drop session`

### T2: Rewrite ParseSessionReader on bufio.Reader

**Files:**
- `pkg/parser/parser.go` — replace scanner loop with `bufio.Reader` + local `readLine` helper
- Add a small local `readLine(*bufio.Reader) ([]byte, error)` mirroring `pkg/adapter/util.go`'s `ReadLine`. Two-line comment noting parity with the adapter helper (why we don't share: package layering).
- Update loop to skip lines whose JSON fails to parse (existing behavior) and track the skipped count. A read failure for reasons other than EOF still aborts the parse and returns an error (unchanged fail-closed behavior), but the skipped count accumulated so far is returned alongside the error.

Test from T1 should now pass. Also verify existing `TestParseSessionReader_*` tests still pass.

Commit: `fix: use bufio.Reader in parser to survive oversized lines (#11)`

### T3: Surface skipped line count

Two smaller sub-steps:

**T3a:** Extend `ParseSessionReader` signature to also return skipped count. Callers to update: `parser.ParseSession`, `pkg/adapter/claudecode/claudecode.go`. Old test lookups (`_, session, err`) become `_, session, _, err` or similar. Consider whether we want a new struct return type vs. extra return value — extra return is smaller.

**T3b:** In `pkg/adapter/claudecode/claudecode.go`, set `metadata["skipped_lines"]` when non-zero.

**T3c:** In `internal/sync/sync.go`, if `parsed.Metadata["skipped_lines"]` is present and non-zero, accumulate into `stats.SessionsWithSkippedLines` (new field) and `stats.TotalSkippedLines` (new field). Print in summary when verbose.

Commit: `feat: count and surface JSONL lines skipped during parse`

### T4: Verify sync behavior after fix

Add an integration-shaped test:
- Create a temp `.jsonl` with one oversized line surrounded by valid ones.
- Run parse via the Claude Code adapter and confirm: session ID populated, correct number of turns, skipped_lines in metadata.
- (If cheap) run through `internal/sync` and confirm mtime IS recorded now.

Commit: `test: session with oversized line syncs correctly and records mtime`

### T5: Full suite

Run `go test ./... -race -count=1`. Expected: green.

If any adapter tests break because they consume `ParseSessionReader`'s return signature, that's covered by T3a — no new work.

### T6: Push + PR

Push branch, open PR referencing #11. PR body: repro + before/after behavior + link to this design doc.

## Explicit non-goals

- Do not change `bufio.Scanner` usage elsewhere in the repo (adapter files already use `bufio.Reader`; parser is the last holdout).
- Do not add PDF-content-specific detection. The fix is generic — any oversized line survives.
- Do not add configurable caps. Not needed.
- Do not touch `internal/sync` beyond surfacing the skipped-line count. The "record mtime on parse failure" branch (Bug B in isolation) is not touched; T2 makes it moot because parse no longer fails on oversized lines.

## Test data considerations

The 12 MB test line will bloat test artifacts if we commit it. Options:
- Build the oversized line at test time via `strings.Repeat("A", 12*1024*1024)` inside a valid JSON envelope. This is the right choice — deterministic, no committed binary, adds ~12 MB to test memory (fine).
- Commit a fixture — no, wasteful.

## Verification once done

Manual: on a machine with a real trapped session, `ccvault sync` should complete without the "token too long" error, log 1+ skipped lines for that session, and record the mtime so subsequent syncs skip it.
