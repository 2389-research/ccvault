// ABOUTME: Tests the backfill that re-derives sessions.cache_read/cache_write from the stored transcript lines.
// ABOUTME: Covers the repair, the refusals (unreadable and partial stored lines), idempotence and project aggregates.

package sync

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/2389-research/ccvault/internal/config"
	"github.com/2389-research/ccvault/internal/db"

	// Register the codex adapter: the refusal this file pins is codex's, and
	// the claude-code registration the rest of the package relies on does not
	// cover it.
	_ "github.com/2389-research/ccvault/pkg/adapter/codex"
)

// zeroCacheColumns puts a session back into the pre-#155 state: the cache pair
// at zero with input/output as the old binary counted them.
//
// The old binary counted input and output correctly and dropped the cache pair
// on the floor, so that — not a wholesale zero — is the state the backfill has
// to repair.
func zeroCacheColumns(t *testing.T, database *db.DB, sessionID string) {
	t.Helper()
	res, err := database.Exec(
		`UPDATE sessions SET cache_read_tokens = 0, cache_write_tokens = 0 WHERE id = ?`,
		sessionID)
	if err != nil {
		t.Fatalf("zero cache columns: %v", err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		t.Fatalf("zero cache columns affected %d rows (err %v), want 1", n, err)
	}
}

// TestBackfillCacheTokens_RepairsFromStoredTranscript is the whole point of
// #155's second half.
//
// Only ~332 of the author's 49,226 transcripts still exist on disk, so the
// repair cannot be a re-parse from the source file. turns.raw_json holds the
// complete JSONL line, so the lines are fed back through the same adapter that
// originally read them and the counters are summed exactly as a live sync sums
// them.
//
// The assertion is on the exact transcript numbers, not merely on non-zero:
// zero is a plausible cache reading, which is how the original bug stayed
// invisible for the whole life of the archive.
func TestBackfillCacheTokens_RepairsFromStoredTranscript(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	claudeHome := t.TempDir()
	const sessionID = "bacf0000-1111-2222-3333-444455556666"
	writeTestSession(t, claudeHome, sessionID, "-Users-test-backfill")

	syncer := New(database, []config.SourceConfig{
		{Name: "claude-code", Type: "claude-code", Path: claudeHome},
	}, WithFullSync(true))
	if _, err := syncer.Run(context.Background()); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	// writeTestSession's assistant usage object, as literals so a fixture that
	// stops carrying cache counters fails here rather than agreeing with zero.
	const (
		wantInput      = int64(10)
		wantOutput     = int64(5)
		wantCacheRead  = int64(100)
		wantCacheWrite = int64(20)
	)

	// The transcript is removed before the backfill runs. This is the case the
	// archive is actually in — the session rows outlived their source files —
	// and removing it here is what proves the repair reads the stored lines
	// rather than quietly falling back to the file.
	if err := os.RemoveAll(filepath.Join(claudeHome, "projects")); err != nil {
		t.Fatalf("remove transcripts: %v", err)
	}
	zeroCacheColumns(t, database, sessionID)

	report, err := BackfillCacheTokens(context.Background(), database, CacheBackfillOptions{})
	if err != nil {
		t.Fatalf("BackfillCacheTokens: %v", err)
	}

	if report.Repaired != 1 {
		t.Errorf("Repaired = %d, want 1 (report: %+v)", report.Repaired, report)
	}
	if report.CacheTokensRecovered != wantCacheRead+wantCacheWrite {
		t.Errorf("CacheTokensRecovered = %d, want %d",
			report.CacheTokensRecovered, wantCacheRead+wantCacheWrite)
	}

	session, err := database.GetSession(sessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if session.CacheReadTokens != wantCacheRead {
		t.Errorf("CacheReadTokens = %d, want %d", session.CacheReadTokens, wantCacheRead)
	}
	if session.CacheWriteTokens != wantCacheWrite {
		t.Errorf("CacheWriteTokens = %d, want %d", session.CacheWriteTokens, wantCacheWrite)
	}
	// The counted pair must come back unchanged for a Claude Code transcript:
	// its four usage counters are disjoint, so the repair adds the two that
	// were dropped and re-derives the two that were not.
	if session.InputTokens != wantInput {
		t.Errorf("InputTokens = %d, want %d", session.InputTokens, wantInput)
	}
	if session.OutputTokens != wantOutput {
		t.Errorf("OutputTokens = %d, want %d", session.OutputTokens, wantOutput)
	}
}

// TestBackfillCacheTokens_ReconcilesProjectTotals covers the aggregate that
// would otherwise keep reporting the understated figure.
//
// projects.total_tokens is a denormalized sum of all four session counters, so
// repairing the sessions and stopping there leaves every project and every
// `list-projects` row exactly as wrong as before.
func TestBackfillCacheTokens_ReconcilesProjectTotals(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	claudeHome := t.TempDir()
	const sessionID = "bacf1111-2222-3333-4444-555566667777"
	writeTestSession(t, claudeHome, sessionID, "-Users-test-aggregates")

	syncer := New(database, []config.SourceConfig{
		{Name: "claude-code", Type: "claude-code", Path: claudeHome},
	}, WithFullSync(true))
	if _, err := syncer.Run(context.Background()); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	zeroCacheColumns(t, database, sessionID)
	if _, err := database.Exec(`UPDATE projects SET total_tokens = 15`); err != nil {
		t.Fatalf("understate project totals: %v", err)
	}

	if _, err := BackfillCacheTokens(context.Background(), database, CacheBackfillOptions{}); err != nil {
		t.Fatalf("BackfillCacheTokens: %v", err)
	}

	var total int64
	if err := database.QueryRow(`SELECT total_tokens FROM projects`).Scan(&total); err != nil {
		t.Fatalf("read project totals: %v", err)
	}
	// 10 input + 5 output + 100 cache read + 20 cache write.
	if total != 135 {
		t.Errorf("projects.total_tokens = %d, want 135", total)
	}
}

// TestBackfillCacheTokens_SkipsUnreadableTurns pins the 42 sessions whose
// raw_json was deliberately nulled.
//
// 216,978 turns across 42 sessions hold no stored line at all: the bytes were
// provably unrelated input and the transcripts are gone (#101). Those sessions
// cannot be repaired, and the backfill has to say so and move on rather than
// error out of a run of 48,000 — or, worse, write the zero it derived from
// nothing.
func TestBackfillCacheTokens_SkipsUnreadableTurns(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	claudeHome := t.TempDir()
	const sessionID = "bacf2222-3333-4444-5555-666677778888"
	writeTestSession(t, claudeHome, sessionID, "-Users-test-unreadable")

	syncer := New(database, []config.SourceConfig{
		{Name: "claude-code", Type: "claude-code", Path: claudeHome},
	}, WithFullSync(true))
	if _, err := syncer.Run(context.Background()); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	zeroCacheColumns(t, database, sessionID)
	if _, err := database.Exec(
		`UPDATE turns SET raw_json = NULL WHERE session_id = ? AND type = 'assistant'`,
		sessionID); err != nil {
		t.Fatalf("null the stored lines: %v", err)
	}

	report, err := BackfillCacheTokens(context.Background(), database, CacheBackfillOptions{})
	if err != nil {
		t.Fatalf("BackfillCacheTokens: %v", err)
	}

	if report.Unreadable != 1 {
		t.Errorf("Unreadable = %d, want 1 (report: %+v)", report.Unreadable, report)
	}
	if report.Repaired != 0 {
		t.Errorf("Repaired = %d, want 0", report.Repaired)
	}
	if len(report.Errors) != 0 {
		t.Errorf("Errors = %v, want none; an unrepairable session is a skip, not a failure", report.Errors)
	}

	session, err := database.GetSession(sessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	// Nothing written at all — not the zero the stored rows would have yielded.
	if session.InputTokens != 10 || session.OutputTokens != 5 {
		t.Errorf("input/output = %d/%d, want 10/5; a skipped session must not be rewritten",
			session.InputTokens, session.OutputTokens)
	}
}

// TestBackfillCacheTokens_RefusesAStrippedLine is the guard that keeps a
// partial re-derivation from being written as if it were the whole session.
//
// A turn whose original line exceeded the raw_json cap is stored as a
// placeholder that preserves its identity and drops its message, so its usage
// object is gone. Re-deriving such a session recovers the cache tokens of
// every *other* turn and silently loses that one — a plausible-looking number
// that is wrong, which is the failure mode #155 already is.
//
// It is deliberately not a search for placeholders. The arithmetic catches
// every way a stored line can fail to carry its usage, including the one it
// was not written for: codex reports its token counts on token_count event
// lines, which never became turns, so a legacy codex session re-derives to
// zero of everything.
func TestBackfillCacheTokens_RefusesAStrippedLine(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	claudeHome := t.TempDir()
	const sessionID = "bacf3333-4444-5555-6666-777788889999"
	// Two assistant turns, so stripping one leaves the other's cache tokens
	// behind. A session with a single assistant turn would re-derive to zero
	// cache tokens and be caught by the no-cache-usage branch instead, which
	// would let this test pass with no guard at all.
	writeTwoAssistantTurnSession(t, claudeHome, sessionID, "-Users-test-shrink")

	syncer := New(database, []config.SourceConfig{
		{Name: "claude-code", Type: "claude-code", Path: claudeHome},
	}, WithFullSync(true))
	if _, err := syncer.Run(context.Background()); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	stored, err := database.GetSession(sessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if stored.InputTokens != 30 || stored.CacheReadTokens != 300 {
		t.Fatalf("fixture synced as input %d / cache read %d, want 30 / 300",
			stored.InputTokens, stored.CacheReadTokens)
	}

	zeroCacheColumns(t, database, sessionID)
	// The placeholder an oversized line is stored as: identity preserved,
	// message — and so usage — dropped. Applied to the second assistant turn
	// only.
	if _, err := database.Exec(`
		UPDATE turns
		SET raw_json = json_object(
			'_ccvault_stripped', json('true'),
			'uuid', id, 'sessionId', session_id, 'type', type, 'timestamp', timestamp)
		WHERE session_id = ? AND ordinal = 2`, sessionID); err != nil {
		t.Fatalf("stub the stored line: %v", err)
	}

	report, err := BackfillCacheTokens(context.Background(), database, CacheBackfillOptions{})
	if err != nil {
		t.Fatalf("BackfillCacheTokens: %v", err)
	}

	if report.Mismatched != 1 {
		t.Errorf("Mismatched = %d, want 1 (report: %+v)", report.Mismatched, report)
	}
	if report.Repaired != 0 {
		t.Errorf("Repaired = %d, want 0; a partial re-derivation is not a repair", report.Repaired)
	}

	session, err := database.GetSession(sessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if session.InputTokens != 30 || session.OutputTokens != 14 {
		t.Errorf("input/output = %d/%d, want 30/14 left alone",
			session.InputTokens, session.OutputTokens)
	}
	if session.CacheReadTokens != 0 {
		t.Errorf("CacheReadTokens = %d, want 0; 200 of the 300 is worse than the honest zero",
			session.CacheReadTokens)
	}
}

// writeTwoAssistantTurnSession writes a Claude Code transcript with one user
// turn and two assistant turns, each carrying its own usage object.
//
// Totals: input 30, output 14, cache read 300, cache write 60.
func writeTwoAssistantTurnSession(t *testing.T, dir, sessionID, projectDir string) {
	t.Helper()
	projDir := filepath.Join(dir, "projects", projectDir)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	cwd := "/Users/test/shrink"
	line := func(uuid, parent, turnType, body string, offset time.Duration) string {
		return `{"uuid":"` + uuid + `","parentUuid":` + parent +
			`,"sessionId":"` + sessionID + `","type":"` + turnType +
			`","timestamp":"` + now.Add(offset).Format(time.RFC3339Nano) +
			`","cwd":"` + cwd + `","gitBranch":"main","version":"1.0.0","message":` + body + `}`
	}
	assistant := func(text string) string {
		return `{"role":"assistant","model":"claude-opus-4","content":[{"type":"text","text":"` + text +
			`"}],"usage":{"input_tokens":15,"output_tokens":7,` +
			`"cache_read_input_tokens":150,"cache_creation_input_tokens":30}}`
	}

	lines := []string{
		line(sessionID+"-turn-1", "null", "user", `{"role":"user","content":"Hello world"}`, 0),
		line(sessionID+"-turn-2", `"`+sessionID+`-turn-1"`, "assistant", assistant("first"), time.Second),
		line(sessionID+"-turn-3", `"`+sessionID+`-turn-2"`, "assistant", assistant("second"), 2*time.Second),
	}

	var buf []byte
	for _, l := range lines {
		buf = append(buf, l...)
		buf = append(buf, '\n')
	}
	if err := os.WriteFile(filepath.Join(projDir, sessionID+".jsonl"), buf, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
}

// TestBackfillCacheTokens_NamesTheSessionsWithNoCacheUsage separates the two
// readings of a zero.
//
// A short session really can spend no cache tokens, and after the backfill its
// columns look exactly like a session the backfill never reached. The run has
// to count the two apart, because "48,000 sessions still read zero" means
// something quite different from "48,000 sessions were examined and 300 of
// them genuinely spent nothing".
func TestBackfillCacheTokens_NamesTheSessionsWithNoCacheUsage(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	claudeHome := t.TempDir()
	const sessionID = "bacf4444-5555-6666-7777-88889999aaaa"
	writeTestSession(t, claudeHome, sessionID, "-Users-test-nocache")

	syncer := New(database, []config.SourceConfig{
		{Name: "claude-code", Type: "claude-code", Path: claudeHome},
	}, WithFullSync(true))
	if _, err := syncer.Run(context.Background()); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	// A transcript whose usage object carries no cache counters: the stored
	// line is complete and readable, and the honest answer is zero.
	if _, err := database.Exec(`
		UPDATE turns
		SET raw_json = json_remove(
			json_remove(raw_json, '$.message.usage.cache_read_input_tokens'),
			'$.message.usage.cache_creation_input_tokens')
		WHERE session_id = ? AND type = 'assistant'`, sessionID); err != nil {
		t.Fatalf("drop the cache counters from the stored line: %v", err)
	}
	zeroCacheColumns(t, database, sessionID)

	report, err := BackfillCacheTokens(context.Background(), database, CacheBackfillOptions{})
	if err != nil {
		t.Fatalf("BackfillCacheTokens: %v", err)
	}

	if report.NoCacheUsage != 1 {
		t.Errorf("NoCacheUsage = %d, want 1 (report: %+v)", report.NoCacheUsage, report)
	}
	if report.Repaired != 0 {
		t.Errorf("Repaired = %d, want 0", report.Repaired)
	}
	if report.Mismatched != 0 {
		t.Errorf("Mismatched = %d, want 0; input and output still add up", report.Mismatched)
	}
}

// TestBackfillCacheTokens_IsIdempotent covers the re-run.
//
// Reading 4 GB of stored lines across 48,000 sessions takes minutes, so the
// run has to be safe to repeat and to resume. The key is the cache pair itself
// being zero, which is why a repaired session is not a candidate the second
// time and a genuinely cache-free one still is.
func TestBackfillCacheTokens_IsIdempotent(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	claudeHome := t.TempDir()
	const sessionID = "bacf5555-6666-7777-8888-9999aaaabbbb"
	writeTestSession(t, claudeHome, sessionID, "-Users-test-idempotent")

	syncer := New(database, []config.SourceConfig{
		{Name: "claude-code", Type: "claude-code", Path: claudeHome},
	}, WithFullSync(true))
	if _, err := syncer.Run(context.Background()); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	zeroCacheColumns(t, database, sessionID)

	first, err := BackfillCacheTokens(context.Background(), database, CacheBackfillOptions{})
	if err != nil {
		t.Fatalf("first BackfillCacheTokens: %v", err)
	}
	if first.Repaired != 1 {
		t.Fatalf("first run Repaired = %d, want 1", first.Repaired)
	}

	before, err := database.GetSession(sessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}

	second, err := BackfillCacheTokens(context.Background(), database, CacheBackfillOptions{})
	if err != nil {
		t.Fatalf("second BackfillCacheTokens: %v", err)
	}
	if second.Candidates != 0 {
		t.Errorf("second run Candidates = %d, want 0; a repaired session is not a candidate again",
			second.Candidates)
	}
	if second.Repaired != 0 {
		t.Errorf("second run Repaired = %d, want 0", second.Repaired)
	}

	after, err := database.GetSession(sessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if after.TotalTokens() != before.TotalTokens() {
		t.Errorf("TotalTokens() moved on the second run: %d then %d",
			before.TotalTokens(), after.TotalTokens())
	}
}

// TestBackfillCacheTokens_CancelledRunIsNotAFailure covers Ctrl-C.
//
// A pass over 48,000 sessions takes minutes, so being interrupted is a normal
// way for this command to end. It has to report that it stopped early and exit
// cleanly: the sessions it already committed stay repaired, and the candidate
// condition means the next run picks up exactly the rest.
func TestBackfillCacheTokens_CancelledRunIsNotAFailure(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	claudeHome := t.TempDir()
	const sessionID = "bacf7777-8888-9999-aaaa-bbbbccccdddd"
	writeTestSession(t, claudeHome, sessionID, "-Users-test-cancel")

	syncer := New(database, []config.SourceConfig{
		{Name: "claude-code", Type: "claude-code", Path: claudeHome},
	}, WithFullSync(true))
	if _, err := syncer.Run(context.Background()); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	zeroCacheColumns(t, database, sessionID)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	report, err := BackfillCacheTokens(ctx, database, CacheBackfillOptions{})
	if err != nil {
		t.Fatalf("a cancelled run returned an error: %v", err)
	}
	if !report.Cancelled {
		t.Error("report does not say the run was cancelled")
	}
	if report.Examined != 0 {
		t.Errorf("Examined = %d, want 0; the run stopped before the first session", report.Examined)
	}
	if report.Candidates != 1 {
		t.Errorf("Candidates = %d, want 1; the work left to do is still reported", report.Candidates)
	}

	session, err := database.GetSession(sessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if session.CacheReadTokens != 0 {
		t.Errorf("CacheReadTokens = %d, want 0; nothing was examined", session.CacheReadTokens)
	}
}

// TestBackfillCacheTokens_DryRunWritesNothing covers the look-before-you-write
// pass on an archive of this size.
func TestBackfillCacheTokens_DryRunWritesNothing(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	claudeHome := t.TempDir()
	const sessionID = "bacf6666-7777-8888-9999-aaaabbbbcccc"
	writeTestSession(t, claudeHome, sessionID, "-Users-test-dryrun")

	syncer := New(database, []config.SourceConfig{
		{Name: "claude-code", Type: "claude-code", Path: claudeHome},
	}, WithFullSync(true))
	if _, err := syncer.Run(context.Background()); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	zeroCacheColumns(t, database, sessionID)

	report, err := BackfillCacheTokens(context.Background(), database, CacheBackfillOptions{DryRun: true})
	if err != nil {
		t.Fatalf("BackfillCacheTokens: %v", err)
	}
	// The dry run reports what it would repair, in the same counter, so the
	// number the user approves is the number they get.
	if report.Repaired != 1 {
		t.Errorf("Repaired = %d, want 1", report.Repaired)
	}

	session, err := database.GetSession(sessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if session.CacheReadTokens != 0 || session.CacheWriteTokens != 0 {
		t.Errorf("cache pair = %d/%d, want 0/0; a dry run must not write",
			session.CacheReadTokens, session.CacheWriteTokens)
	}
}

// TestBackfillCacheTokens_RefusesALegacyCodexSession is the case a SQL
// reimplementation of the per-source rule would have got silently wrong, in
// the direction that costs the most.
//
// codex reports its token counts on token_count event lines, which the adapter
// folds into the preceding assistant turn rather than storing as turns of
// their own. They are therefore absent from raw_json, so a legacy codex
// session re-derives to zero tokens of every kind — and a backfill that
// trusted its own re-derivation would wipe the input_tokens the session
// already had.
func TestBackfillCacheTokens_RefusesALegacyCodexSession(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	codexHome := t.TempDir()
	sessionsDir := filepath.Join(codexHome, "sessions", "2026", "03", "11")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	const codexUUID = "019cddb7-1979-7983-b176-785af4de7cbf"
	writeCodexTestSession(t, filepath.Join(sessionsDir,
		"rollout-2026-03-11T11-24-50-"+codexUUID+".jsonl"))

	syncer := New(database, []config.SourceConfig{
		{Name: "codex", Type: "codex", Path: codexHome},
	}, WithFullSync(true))
	if _, err := syncer.Run(context.Background()); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	sessionID := codexUUID
	synced, err := database.GetSession(sessionID)
	if err != nil {
		t.Fatalf("GetSession(%s): %v", sessionID, err)
	}
	if synced == nil {
		t.Fatalf("no session row for %s; the codex fixture did not sync", sessionID)
	}
	// The split #163 applies: the cached share is moved out of input, not
	// copied beside it, so the total is unchanged by it.
	if synced.CacheReadTokens != 7040 {
		t.Fatalf("CacheReadTokens = %d, want 7040 — fixture or adapter stopped splitting",
			synced.CacheReadTokens)
	}
	if synced.InputTokens != 8870-7040 {
		t.Fatalf("InputTokens = %d, want %d", synced.InputTokens, 8870-7040)
	}

	// Put the row back the way a pre-#163 binary left it: the cached share
	// still inside input_tokens, the cache pair at zero.
	if _, err := database.Exec(
		`UPDATE sessions SET input_tokens = 8870, cache_read_tokens = 0, cache_write_tokens = 0 WHERE id = ?`,
		sessionID); err != nil {
		t.Fatalf("restore the pre-fix row: %v", err)
	}

	report, err := BackfillCacheTokens(context.Background(), database, CacheBackfillOptions{})
	if err != nil {
		t.Fatalf("BackfillCacheTokens: %v", err)
	}

	if report.Mismatched != 1 {
		t.Errorf("Mismatched = %d, want 1 (report: %+v)", report.Mismatched, report)
	}
	if report.Repaired != 0 {
		t.Errorf("Repaired = %d, want 0", report.Repaired)
	}

	after, err := database.GetSession(sessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if after.InputTokens != 8870 {
		t.Errorf("InputTokens = %d, want 8870 — the refusal is what protects it", after.InputTokens)
	}
}

// writeCodexTestSession writes a codex rollout carrying one assistant message
// and the cumulative token_count event that reports its cost.
//
// The numbers are a real reading: input 8870 / cached 7040 / output 332 /
// total 9202, where 8870+332 = 9202, so cached_input_tokens is contained in
// input_tokens rather than sitting beside it.
func writeCodexTestSession(t *testing.T, path string) {
	t.Helper()
	lines := []string{
		`{"timestamp":"2026-03-11T16:25:01.231Z","type":"session_meta","payload":{"id":"019cddb7-1979-7983-b176-785af4de7cbf","cwd":"/Users/test/codexproject"}}`,
		`{"timestamp":"2026-03-11T16:25:01.232Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"an answer"}]}}`,
		`{"timestamp":"2026-03-11T16:25:08.133Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":8870,"cached_input_tokens":7040,"output_tokens":332,"total_tokens":9202}}}}`,
	}
	var buf []byte
	for _, line := range lines {
		buf = append(buf, line...)
		buf = append(buf, '\n')
	}
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatalf("write codex fixture: %v", err)
	}
	// Give the fixture an mtime the sync layer will read as current.
	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
}
