// ABOUTME: Tests that cache invalidation covers whatever the exporter wrote, not one hard-coded filename (#168).
// ABOUTME: A second cache file used to survive invalidation and go silently stale.

package analytics

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/2389-research/ccvault/internal/db"
)

// parquetFilesIn lists the parquet basenames left in a cache dir.
func parquetFilesIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read cache dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".parquet" {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names)
	return names
}

// TestInvalidateCache_RemovesEverythingTheExporterWrote is the assertion that
// replaces a filename repeated in two places. It names no file: it asks the
// exporter to write a cache, invalidates, and requires the directory to hold
// no parquet afterwards. A second output file added later is covered by this
// test on the day it is written.
func TestInvalidateCache_RemovesEverythingTheExporterWrote(t *testing.T) {
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	seedSessionWithCacheTokens(t, database, time.Now().UTC(), "/tmp/proj-inv", "sess-inv-1", 10, 5, 100, 20)

	cacheDir := t.TempDir()
	if err := NewExporter(database, cacheDir).Export(); err != nil {
		t.Fatalf("export: %v", err)
	}
	if got := parquetFilesIn(t, cacheDir); len(got) == 0 {
		t.Fatal("export wrote no parquet; the test cannot show invalidation")
	}

	if err := InvalidateCache(cacheDir); err != nil {
		t.Fatalf("InvalidateCache: %v", err)
	}
	if got := parquetFilesIn(t, cacheDir); len(got) != 0 {
		t.Errorf("parquet files surviving invalidation: %v", got)
	}
}

// TestInvalidateCache_RemovesASecondCacheFile is #168 exactly — #149's bug one
// level up. The staleness check was pinned to one column name and went blind
// when a column was added; invalidation was pinned to one filename and would
// have gone blind the same way. Latent today because Export writes one file,
// so the bug has to be shown against a second file put there by hand.
func TestInvalidateCache_RemovesASecondCacheFile(t *testing.T) {
	cacheDir := t.TempDir()
	for _, name := range []string{"sessions.parquet", "turns.parquet"} {
		if err := os.WriteFile(filepath.Join(cacheDir, name), []byte("PAR1"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	if err := InvalidateCache(cacheDir); err != nil {
		t.Fatalf("InvalidateCache: %v", err)
	}
	if got := parquetFilesIn(t, cacheDir); len(got) != 0 {
		t.Errorf("parquet files surviving invalidation: %v — invalidation is still pinned to one name", got)
	}
}

// TestInvalidateCache_LeavesNonParquetAlone keeps the blast radius to the
// cache this package writes. The cache dir is ccvault's, but "delete every
// parquet" is a narrower promise than "empty the directory", and the narrower
// one is all the invalidator needs.
func TestInvalidateCache_LeavesNonParquetAlone(t *testing.T) {
	cacheDir := t.TempDir()
	keep := filepath.Join(cacheDir, "notes.txt")
	if err := os.WriteFile(keep, []byte("hands off"), 0o600); err != nil {
		t.Fatalf("write notes: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "sessions.parquet"), []byte("PAR1"), 0o600); err != nil {
		t.Fatalf("write parquet: %v", err)
	}

	if err := InvalidateCache(cacheDir); err != nil {
		t.Fatalf("InvalidateCache: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("notes.txt should have survived: %v", err)
	}
}

// TestInvalidateCache_MissingDirIsNotAnError: callers invalidate before they
// know whether a cache was ever built, and "there is nothing to invalidate" is
// success. os.Remove on a hard-coded name behaved this way via fs.ErrNotExist
// and the replacement must too, or `sync --full` fails on a fresh machine.
func TestInvalidateCache_MissingDirIsNotAnError(t *testing.T) {
	if err := InvalidateCache(filepath.Join(t.TempDir(), "never-built")); err != nil {
		t.Errorf("InvalidateCache on a missing dir = %v, want nil", err)
	}
}

// TestInvalidateCache_EmptyDirIsANoOp guards the other half of the caller's
// contract: an empty string cache dir means "no analytics cache configured",
// which the Syncer decides before calling. Asked directly, an empty path must
// not be read as the working directory.
func TestInvalidateCache_EmptyDirIsANoOp(t *testing.T) {
	if err := InvalidateCache(""); err != nil {
		t.Errorf("InvalidateCache(\"\") = %v, want nil", err)
	}
}
