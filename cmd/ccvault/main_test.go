// ABOUTME: Tests for CLI helpers in package main
// ABOUTME: Verifies orient/prepareFullSync gather DB state, report failures, and apply --full safety.

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/2389-research/ccvault/internal/db"
)

func TestGatherOrientation_HealthyDBHasNoWarnings(t *testing.T) {
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = database.Close() }()

	o := gatherOrientation(database)
	if len(o.Warnings) != 0 {
		t.Errorf("healthy db should produce no warnings, got %v", o.Warnings)
	}
}

func TestGatherOrientation_CollectsWarningsOnFailure(t *testing.T) {
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	_ = database.Close() // force every stats query to fail

	o := gatherOrientation(database)
	if len(o.Warnings) != 6 {
		t.Errorf("closed db should produce 6 warnings (one per query), got %d: %v", len(o.Warnings), o.Warnings)
	}
}

// TestPrepareFullSync_WritesBackupBeforeSubsequentWipe verifies the full
// end-to-end contract: with --yes + backup enabled, prepareFullSync
// returns a backup path pointing at a real SQLite file that survives a
// downstream ResetAll — this is the whole point of the safety block.
func TestPrepareFullSync_WritesBackupBeforeSubsequentWipe(t *testing.T) {
	dir := t.TempDir()
	database, err := db.Open(dir)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = database.Close() }()

	// Seed something recognisable so we can prove the backup contains it
	// after the live DB is wiped.
	if _, err := database.Exec(`INSERT INTO projects (path, display_name, source) VALUES (?, ?, ?)`,
		"/canary/path", "canary-project", "claude-code"); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	backupPath, err := prepareFullSync(database, dir, true /*assumeYes*/, false /*noBackup*/)
	if err != nil {
		t.Fatalf("prepareFullSync: %v", err)
	}
	if backupPath == "" {
		t.Fatal("expected non-empty backupPath when noBackup=false")
	}
	if !strings.HasPrefix(backupPath, filepath.Join(dir, "backups")+string(filepath.Separator)) {
		t.Errorf("backup path %q not inside %q/backups/", backupPath, dir)
	}
	if _, err := os.Stat(backupPath); err != nil {
		t.Fatalf("backup file missing: %v", err)
	}

	// Wipe the live DB. The backup must still contain the seeded row.
	if err := database.ResetAll(); err != nil {
		t.Fatalf("reset live db: %v", err)
	}

	restored, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open scratch: %v", err)
	}
	defer func() { _ = restored.Close() }()

	// Point the "restored" verifier at the backup file directly by
	// copying it into a scratch DB path — the simplest cross-package way
	// to inspect the backup without exposing internal open helpers.
	backupBytes, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	scratch := t.TempDir()
	if err := os.WriteFile(filepath.Join(scratch, "ccvault.db"), backupBytes, 0o640); err != nil {
		t.Fatalf("write backup copy: %v", err)
	}
	verify, err := db.Open(scratch)
	if err != nil {
		t.Fatalf("open verify: %v", err)
	}
	defer func() { _ = verify.Close() }()

	var name string
	if err := verify.QueryRow(`SELECT display_name FROM projects WHERE path = ?`, "/canary/path").Scan(&name); err != nil {
		t.Fatalf("query backup for seeded row: %v", err)
	}
	if name != "canary-project" {
		t.Errorf("backup contents mismatch: got %q, want %q", name, "canary-project")
	}
}

// TestPrepareFullSync_NoBackupSkipsBackup asserts the escape hatch:
// --no-backup returns an empty path and creates no files.
func TestPrepareFullSync_NoBackupSkipsBackup(t *testing.T) {
	dir := t.TempDir()
	database, err := db.Open(dir)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = database.Close() }()

	backupPath, err := prepareFullSync(database, dir, true /*assumeYes*/, true /*noBackup*/)
	if err != nil {
		t.Fatalf("prepareFullSync: %v", err)
	}
	if backupPath != "" {
		t.Errorf("expected empty backupPath with --no-backup, got %q", backupPath)
	}
	if _, err := os.Stat(filepath.Join(dir, "backups")); !os.IsNotExist(err) {
		t.Errorf("--no-backup should not create backups/ dir, got %v", err)
	}
}

// TestPruneOldBackups_KeepsMostRecentN drops timestamped fixtures into
// a backups dir and asserts pruneOldBackups retains the highest-sorting
// (newest) N. Filename timestamps sort lexicographically → chronological
// order, so the pruner never needs to parse the string.
func TestPruneOldBackups_KeepsMostRecentN(t *testing.T) {
	dir := t.TempDir()
	names := []string{
		"ccvault-20250101-000000.db",
		"ccvault-20250601-120000.db",
		"ccvault-20260101-000000.db",
		"ccvault-20260615-235959.db",
		"ccvault-20260801-120000.db",
		"ccvault-20260814-101010.db",
		"ccvault-20260814-101011.db",
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("stub"), 0o640); err != nil {
			t.Fatalf("write %s: %v", n, err)
		}
	}
	// Non-matching files must never be touched.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi"), 0o640); err != nil {
		t.Fatalf("write README: %v", err)
	}

	pruned, err := pruneOldBackups(dir, 5)
	if err != nil {
		t.Fatalf("pruneOldBackups: %v", err)
	}
	if pruned != 2 {
		t.Errorf("expected 2 pruned, got %d", pruned)
	}

	// The two oldest should be gone, the five newest and the README must remain.
	remaining := map[string]bool{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		remaining[e.Name()] = true
	}
	wantGone := []string{"ccvault-20250101-000000.db", "ccvault-20250601-120000.db"}
	for _, n := range wantGone {
		if remaining[n] {
			t.Errorf("expected %s pruned, still present", n)
		}
	}
	wantKept := []string{
		"ccvault-20260101-000000.db",
		"ccvault-20260615-235959.db",
		"ccvault-20260801-120000.db",
		"ccvault-20260814-101010.db",
		"ccvault-20260814-101011.db",
		"README.md",
	}
	for _, n := range wantKept {
		if !remaining[n] {
			t.Errorf("expected %s kept, missing", n)
		}
	}
}

// TestPrepareFullSync_NonInteractiveWithoutYesRefuses asserts the
// primary safety property: on a scripted/CI/cron surface (stdin is a
// pipe, not a TTY), --full without --yes must REFUSE rather than
// silently wipe. Swap os.Stdin for a pipe so the test reflects the
// non-interactive case even when go test is launched from a terminal.
func TestPrepareFullSync_NonInteractiveWithoutYesRefuses(t *testing.T) {
	dir := t.TempDir()
	database, err := db.Open(dir)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = database.Close() }()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer func() { _ = r.Close() }()
	defer func() { _ = w.Close() }()
	origStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = origStdin }()

	backupPath, err := prepareFullSync(database, dir, false /*assumeYes*/, false /*noBackup*/)
	if err == nil {
		t.Fatal("expected refusal error, got nil")
	}
	if backupPath != "" {
		t.Errorf("expected empty backupPath on refusal, got %q", backupPath)
	}
	if !strings.Contains(err.Error(), "not a TTY") {
		t.Errorf("expected error mentioning TTY refusal, got %q", err.Error())
	}
	// No backup should exist because we refused before writing anything.
	if _, err := os.Stat(filepath.Join(dir, "backups")); !os.IsNotExist(err) {
		t.Errorf("no backups dir should exist on refusal, got %v", err)
	}
}

// TestPruneOldBackups_UnderKeepThresholdIsNoop asserts the pruner's
// idempotency guarantee: below the retention count, no files are
// removed and no error surfaces.
func TestPruneOldBackups_UnderKeepThresholdIsNoop(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ccvault-20260101-000000.db"), []byte("stub"), 0o640); err != nil {
		t.Fatalf("write: %v", err)
	}
	pruned, err := pruneOldBackups(dir, 5)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if pruned != 0 {
		t.Errorf("expected 0 pruned, got %d", pruned)
	}
}
