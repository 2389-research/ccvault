// ABOUTME: Drives the real binary through the cache-token repair: stats names the gap, repair-tokens closes it.
// ABOUTME: A repair nobody is told to run is half a fix, so the reporting is tested alongside the repair.

package integration

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/2389-research/ccvault/internal/db"
)

// TestCLI_RepairTokensClosesTheCacheTokenGap is #155's backfill end to end.
//
// The fixture's assistant turn carries all four usage counters, so a synced
// session holds 4096 cache-read and 512 cache-write tokens. Zeroing those two
// columns puts the row in the state every session ingested before the fix is
// in: input and output counted, cache pair never assigned, and the source
// transcript long gone for all but a few hundred of them.
func TestCLI_RepairTokensClosesTheCacheTokenGap(t *testing.T) {
	f := newCLIFixture(t)
	f.Run(t, "sync")

	// Put the session back into the pre-fix state, through the archive rather
	// than through the CLI: no command writes this state, which is the point —
	// it is the state the old binary left behind.
	undoTheCacheColumns(t, f.DataDir)

	// 1. stats has to name the gap. Nothing else would: zero is a plausible
	//    cache reading, so no token figure looks wrong.
	statsText := f.Run(t, "stats")
	if !strings.Contains(statsText.Stdout, "repair-tokens") {
		t.Errorf("stats does not name the repair; a command nobody knows to run is half a fix:\n%s",
			statsText.Stdout)
	}

	var before struct {
		CacheTokens struct {
			MissingWithCost int   `json:"missing_with_cost"`
			CacheTokens     int64 `json:"cache_tokens"`
			Complete        bool  `json:"complete"`
		} `json:"cache_tokens"`
	}
	f.Run(t, "stats", "--json").JSON(t, &before)
	if before.CacheTokens.MissingWithCost != 1 {
		t.Fatalf("stats reports %d session(s) missing cache tokens, want 1",
			before.CacheTokens.MissingWithCost)
	}
	if before.CacheTokens.Complete {
		t.Error("stats reports cache token coverage complete while a session reads zero")
	}

	// 2. The dry run reports the repair and writes nothing.
	var dry struct {
		Repaired      int  `json:"repaired"`
		DryRun        bool `json:"dry_run"`
		CoverageAfter struct {
			MissingWithCost int `json:"missing_with_cost"`
		} `json:"coverage_after"`
	}
	f.Run(t, "repair-tokens", "--dry-run", "--json").JSON(t, &dry)
	if dry.Repaired != 1 {
		t.Errorf("dry run Repaired = %d, want 1", dry.Repaired)
	}
	if !dry.DryRun {
		t.Error("dry run did not report itself as one")
	}
	if dry.CoverageAfter.MissingWithCost != 1 {
		t.Errorf("after the dry run %d session(s) still read zero, want 1 — a dry run must not write",
			dry.CoverageAfter.MissingWithCost)
	}

	// 3. The real run repairs it, and says so in the counters a script reads.
	var repair struct {
		Repaired             int   `json:"repaired"`
		CacheTokensRecovered int64 `json:"cache_tokens_recovered"`
		Unreadable           int   `json:"unreadable"`
		Mismatched           int   `json:"mismatched"`
		CoverageAfter        struct {
			MissingWithCost int   `json:"missing_with_cost"`
			CacheTokens     int64 `json:"cache_tokens"`
			Complete        bool  `json:"complete"`
		} `json:"coverage_after"`
	}
	f.Run(t, "repair-tokens", "--json").JSON(t, &repair)
	if repair.Repaired != 1 {
		t.Errorf("Repaired = %d, want 1", repair.Repaired)
	}
	if repair.CacheTokensRecovered != 4096+512 {
		t.Errorf("CacheTokensRecovered = %d, want %d", repair.CacheTokensRecovered, 4096+512)
	}
	if repair.Unreadable != 0 || repair.Mismatched != 0 {
		t.Errorf("unreadable %d / mismatched %d, want 0 / 0 for a complete transcript",
			repair.Unreadable, repair.Mismatched)
	}
	if !repair.CoverageAfter.Complete {
		t.Errorf("coverage still incomplete after the repair: %d session(s) read zero",
			repair.CoverageAfter.MissingWithCost)
	}

	// 4. stats stops naming the gap, and the archive's cache total is the
	//    transcript's own figure.
	var after struct {
		CacheTokens struct {
			CacheTokens int64 `json:"cache_tokens"`
			Complete    bool  `json:"complete"`
		} `json:"cache_tokens"`
		TotalTokens int64 `json:"total_tokens"`
	}
	f.Run(t, "stats", "--json").JSON(t, &after)
	if after.CacheTokens.CacheTokens != 4096+512 {
		t.Errorf("archive cache tokens = %d, want %d",
			after.CacheTokens.CacheTokens, 4096+512)
	}
	if after.TotalTokens != 11+7+4096+512 {
		t.Errorf("total tokens = %d, want %d", after.TotalTokens, 11+7+4096+512)
	}
	statsAfter := f.Run(t, "stats")
	if strings.Contains(statsAfter.Stdout, "repair-tokens") {
		t.Errorf("stats still advertises the repair after the archive is clean:\n%s", statsAfter.Stdout)
	}

	// 5. Re-running finds nothing to do.
	var again struct {
		Candidates int `json:"candidates"`
		Repaired   int `json:"repaired"`
	}
	f.Run(t, "repair-tokens", "--json").JSON(t, &again)
	if again.Candidates != 0 || again.Repaired != 0 {
		t.Errorf("second run: candidates %d, repaired %d, want 0 / 0",
			again.Candidates, again.Repaired)
	}
}

// undoTheCacheColumns zeroes the cache pair of every session in the archive at
// dataDir, leaving input and output as they were.
func undoTheCacheColumns(t *testing.T, dataDir string) {
	t.Helper()

	database, err := db.Open(dataDir)
	if err != nil {
		t.Fatalf("open archive at %s: %v", dataDir, err)
	}
	defer func() { _ = database.Close() }()

	res, err := database.Exec(`UPDATE sessions SET cache_read_tokens = 0, cache_write_tokens = 0`)
	if err != nil {
		t.Fatalf("zero the cache columns: %v", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		t.Fatalf("count the zeroed rows: %v", err)
	}
	if n == 0 {
		t.Fatalf("no sessions in %s to zero", filepath.Join(dataDir, "ccvault.db"))
	}
}
