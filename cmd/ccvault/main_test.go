// ABOUTME: Tests for CLI helpers in package main
// ABOUTME: Verifies orient/prepareRebuild gather DB state, report failures, and apply --rebuild safety.

package main

import (
	"io"
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
	if len(o.Warnings) != 7 {
		t.Errorf("closed db should produce 7 warnings (one per query), got %d: %v", len(o.Warnings), o.Warnings)
	}
}

// TestSessionsWithoutSourceFiles counts the rows a rebuild would destroy for
// good — the ones whose source file the upstream tool has already pruned.
// That number is what makes the --rebuild prompt honest.
func TestSessionsWithoutSourceFiles(t *testing.T) {
	dir := t.TempDir()
	database, err := db.Open(dir)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = database.Close() }()

	present := filepath.Join(dir, "still-here.jsonl")
	if err := os.WriteFile(present, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write present file: %v", err)
	}
	gone := filepath.Join(dir, "pruned-upstream.jsonl")

	// Two sessions share the pruned file; both are unrecoverable.
	rows := []struct {
		id   string
		file string
	}{
		{"session-present", present},
		{"session-gone-1", gone},
		{"session-gone-2", gone},
	}
	for _, r := range rows {
		_, err := database.Exec(
			`INSERT INTO sessions (id, started_at, source_file, source) VALUES (?, datetime('now'), ?, 'claude-code')`,
			r.id, r.file)
		if err != nil {
			t.Fatalf("seed session %s: %v", r.id, err)
		}
	}

	missing, err := sessionsWithoutSourceFiles(database)
	if err != nil {
		t.Fatalf("sessionsWithoutSourceFiles: %v", err)
	}
	if missing != 2 {
		t.Errorf("missing = %d, want 2", missing)
	}
}

// TestPrepareRebuild_WritesBackupBeforeSubsequentWipe verifies the full
// end-to-end contract: with --yes + backup enabled, prepareRebuild
// returns a backup path pointing at a real SQLite file that survives a
// downstream ResetAll — this is the whole point of the safety block.
func TestPrepareRebuild_WritesBackupBeforeSubsequentWipe(t *testing.T) {
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

	backupPath, err := prepareRebuild(database, dir, true /*assumeYes*/, false /*noBackup*/)
	if err != nil {
		t.Fatalf("prepareRebuild: %v", err)
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

// TestPrepareRebuild_NoBackupSkipsBackup asserts the escape hatch:
// --no-backup returns an empty path and creates no files.
func TestPrepareRebuild_NoBackupSkipsBackup(t *testing.T) {
	dir := t.TempDir()
	database, err := db.Open(dir)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = database.Close() }()

	backupPath, err := prepareRebuild(database, dir, true /*assumeYes*/, true /*noBackup*/)
	if err != nil {
		t.Fatalf("prepareRebuild: %v", err)
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

// TestPrepareRebuild_NonInteractiveWithoutYesRefuses asserts the
// primary safety property: on a scripted/CI/cron surface (stdin is a
// pipe, not a TTY), --rebuild without --yes must REFUSE rather than
// silently wipe. Swap os.Stdin for a pipe so the test reflects the
// non-interactive case even when go test is launched from a terminal.
func TestPrepareRebuild_NonInteractiveWithoutYesRefuses(t *testing.T) {
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

	backupPath, err := prepareRebuild(database, dir, false /*assumeYes*/, false /*noBackup*/)
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

// TestGatherOrientation_IncludesStorage covers the reason storage landed in
// orient at all: an agent reading the JSON should be able to see that the
// file is mostly dead space without being told to look.
func TestGatherOrientation_IncludesStorage(t *testing.T) {
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = database.Close() }()

	o := gatherOrientation(database)
	if o.Storage.PageCount <= 0 {
		t.Errorf("storage page count = %d, want > 0", o.Storage.PageCount)
	}
	if o.Storage.FileBytes <= 0 {
		t.Errorf("storage file bytes = %d, want > 0", o.Storage.FileBytes)
	}
}

func TestStorageJSON_ReportsPageAccountingAndVerdict(t *testing.T) {
	// The real archive's numbers as measured: 4.6 GB holding 1.9 GB.
	stats := db.StorageStats{
		FileBytes:     4904400896,
		PageSize:      4096,
		PageCount:     1197217,
		FreelistCount: 713257,
	}

	out := storageJSON(stats)

	wantInt := map[string]int64{
		"file_bytes":        4904400896,
		"page_size":         4096,
		"page_count":        1197217,
		"freelist_count":    713257,
		"live_bytes":        1982300160,
		"reclaimable_bytes": 2921500672,
	}
	for key, want := range wantInt {
		got, ok := out[key].(int64)
		if !ok {
			t.Errorf("%s = %#v, want an int64", key, out[key])
			continue
		}
		if got != want {
			t.Errorf("%s = %d, want %d", key, got, want)
		}
	}
	if ratio, ok := out["freelist_ratio"].(float64); !ok || ratio < 0.59 || ratio > 0.60 {
		t.Errorf("freelist_ratio = %#v, want ~0.596", out["freelist_ratio"])
	}
	if worth, ok := out["worth_reclaiming"].(bool); !ok || !worth {
		t.Errorf("worth_reclaiming = %#v, want true", out["worth_reclaiming"])
	}
}

func TestReclaimHint(t *testing.T) {
	tests := []struct {
		name     string
		stats    db.StorageStats
		wantHint bool
	}{
		{
			name:     "a mostly dead archive names the command that fixes it",
			stats:    db.StorageStats{FileBytes: 4904400896, PageSize: 4096, PageCount: 1197217, FreelistCount: 713257},
			wantHint: true,
		},
		{
			name:     "a healthy archive stays quiet",
			stats:    db.StorageStats{FileBytes: 2000000000, PageSize: 4096, PageCount: 488281, FreelistCount: 100},
			wantHint: false,
		},
		{
			name:     "a tiny archive stays quiet even when mostly empty",
			stats:    db.StorageStats{FileBytes: 409600, PageSize: 4096, PageCount: 100, FreelistCount: 90},
			wantHint: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hint := reclaimHint(tt.stats)
			if tt.wantHint == (hint == "") {
				t.Fatalf("reclaimHint() = %q, wantHint = %v", hint, tt.wantHint)
			}
			if tt.wantHint && !strings.Contains(hint, "ccvault vacuum") {
				t.Errorf("hint %q does not name 'ccvault vacuum'", hint)
			}
		})
	}
}

func TestIntegrityJSON_ReportsDriftAndVerdict(t *testing.T) {
	// Three index entries whose turn row is gone, out of an otherwise
	// fully-indexed archive.
	out := integrityJSON(db.FTSIntegrity{Turns: 1_200_000, Indexed: 1_200_003, Orphaned: 3})

	wantInt := map[string]int64{
		"turns":     1_200_000,
		"indexed":   1_200_003,
		"orphaned":  3,
		"unindexed": 0,
	}
	for key, want := range wantInt {
		got, ok := out[key].(int64)
		if !ok {
			t.Errorf("%s = %#v, want an int64", key, out[key])
			continue
		}
		if got != want {
			t.Errorf("%s = %d, want %d", key, got, want)
		}
	}
	if consistent, ok := out["consistent"].(bool); !ok || consistent {
		t.Errorf("consistent = %#v, want false", out["consistent"])
	}

	clean := integrityJSON(db.FTSIntegrity{Turns: 1_200_000, Indexed: 1_200_000})
	if consistent, ok := clean["consistent"].(bool); !ok || !consistent {
		t.Errorf("consistent = %#v for an index in step, want true", clean["consistent"])
	}
}

// captureStdout runs fn with os.Stdout redirected and returns what it wrote.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w

	done := make(chan string, 1)
	go func() {
		var sb strings.Builder
		_, _ = io.Copy(&sb, r)
		done <- sb.String()
	}()

	fn()

	os.Stdout = orig
	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	out := <-done
	if err := r.Close(); err != nil {
		t.Fatalf("close pipe reader: %v", err)
	}
	return out
}

// The search-index section is the only thing that makes FTS drift visible, so
// what it prints is the feature.
func TestPrintIntegritySection(t *testing.T) {
	tests := []struct {
		name      string
		integrity db.FTSIntegrity
		wantLines []string
		omitLines []string
	}{
		{
			name:      "a healthy index reports its size and nothing else",
			integrity: db.FTSIntegrity{Turns: 4200, Indexed: 4200},
			wantLines: []string{"Search index:", "4200 of 4200 turns"},
			omitLines: []string{"Orphaned", "Unindexed", "sync --full"},
		},
		{
			name:      "ghost entries are named and the repair is spelled out",
			integrity: db.FTSIntegrity{Turns: 4200, Indexed: 4203, Orphaned: 3},
			wantLines: []string{"Orphaned:", "3 entries", "ccvault sync --full"},
			omitLines: []string{"Unindexed"},
		},
		{
			name:      "turns the index never got are named too",
			integrity: db.FTSIntegrity{Turns: 4200, Indexed: 4190},
			wantLines: []string{"Unindexed:", "10 turns", "ccvault sync --full"},
			omitLines: []string{"Orphaned"},
		},
		{
			name:      "an empty archive prints no section at all",
			integrity: db.FTSIntegrity{},
			omitLines: []string{"Search index"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := captureStdout(t, func() { printIntegritySection(tt.integrity) })
			for _, want := range tt.wantLines {
				if !strings.Contains(out, want) {
					t.Errorf("output does not contain %q:\n%s", want, out)
				}
			}
			for _, omit := range tt.omitLines {
				if strings.Contains(out, omit) {
					t.Errorf("output unexpectedly contains %q:\n%s", omit, out)
				}
			}
		})
	}
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		bytes int64
		want  string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{4096, "4.0 KB"},
		{2921500672, "2.7 GB"},
		{4904400896, "4.6 GB"},
	}
	for _, tt := range tests {
		if got := formatBytes(tt.bytes); got != tt.want {
			t.Errorf("formatBytes(%d) = %q, want %q", tt.bytes, got, tt.want)
		}
	}
}
