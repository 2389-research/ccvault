// ABOUTME: Tests that the Parquet cache exports cache_write_tokens alongside cache_read_tokens (#167).
// ABOUTME: Without the write counter, totals cannot be recomputed from the cache's own components.

package analytics

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/2389-research/ccvault/internal/db"
)

// seedSessionWithCacheTokens inserts one project plus one session whose four
// token counters are all distinct and non-zero.
//
// seedSession in analytics_test.go pins cache_write_tokens to 0, which is the
// value the bug produces, so a test built on it would agree with the bug. The
// numbers here are deliberately far apart: a query that confuses read for
// write, or drops either, lands on a different total.
func seedSessionWithCacheTokens(t *testing.T, d *db.DB, when time.Time, projectPath, sessionID string,
	inputTok, outputTok, cacheRead, cacheWrite int64,
) {
	t.Helper()

	res, err := d.Exec(`INSERT INTO projects
		(path, display_name, first_seen_at, last_activity_at, session_count, total_tokens, source)
		VALUES (?, ?, ?, ?, 0, 0, 'claude-code')`,
		projectPath, filepath.Base(projectPath), when, when)
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	projectID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("project id: %v", err)
	}

	if _, err := d.Exec(`INSERT INTO sessions
		(id, project_id, model, git_branch, started_at, ended_at, turn_count,
		 input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, source_file, source)
		VALUES (?, ?, 'claude-opus-4', 'main', ?, ?, 2, ?, ?, ?, ?, '/tmp/x.jsonl', 'claude-code')`,
		sessionID, projectID, when, when, inputTok, outputTok, cacheRead, cacheWrite); err != nil {
		t.Fatalf("insert session: %v", err)
	}
}

// TestExportedSessionRecordCarriesCacheWriteTokens is #167's red test.
//
// The export carried cache_read_tokens and not cache_write_tokens, while
// total_tokens summed both — so the cache's own components could not reproduce
// its own total, and read spend could not be told from write spend. Cache
// reads cost roughly a tenth of cache writes, so that split is the whole
// content of a cache-efficiency number.
//
// Read back through DuckDB rather than the parquet-go reader, following #147:
// DuckDB is how this file is consumed, and a column only a Go struct can see
// would satisfy a reader-side assertion while remaining useless to every query.
func TestExportedSessionRecordCarriesCacheWriteTokens(t *testing.T) {
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	const (
		wantInput      = int64(10)
		wantOutput     = int64(5)
		wantCacheRead  = int64(1000)
		wantCacheWrite = int64(200)
	)
	seedSessionWithCacheTokens(t, database, time.Now().UTC(), "/tmp/proj-cache", "sess-cache-1",
		wantInput, wantOutput, wantCacheRead, wantCacheWrite)

	cacheDir := t.TempDir()
	if err := NewExporter(database, cacheDir).Export(); err != nil {
		t.Fatalf("export: %v", err)
	}
	analyzer, err := NewAnalyzer(cacheDir)
	if err != nil {
		t.Fatalf("new analyzer: %v", err)
	}
	t.Cleanup(func() { _ = analyzer.Close() })

	var gotInput, gotOutput, gotRead, gotWrite, gotTotal int64
	err = analyzer.db.QueryRow(
		`SELECT input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, total_tokens
		 FROM read_parquet(?)`,
		filepath.Join(cacheDir, "sessions.parquet"),
	).Scan(&gotInput, &gotOutput, &gotRead, &gotWrite, &gotTotal)
	if err != nil {
		t.Fatalf("query parquet: %v", err)
	}

	if gotWrite != wantCacheWrite {
		t.Errorf("cache_write_tokens = %d, want %d", gotWrite, wantCacheWrite)
	}
	if gotRead != wantCacheRead {
		t.Errorf("cache_read_tokens = %d, want %d", gotRead, wantCacheRead)
	}

	// The point of the issue: a total recomputed from the exported components
	// must match the exported total. With the write counter absent it came up
	// 200 short here, and ~3.27 billion tokens short across the archive.
	if sum := gotInput + gotOutput + gotRead + gotWrite; sum != gotTotal {
		t.Errorf("components sum to %d but total_tokens = %d; the cache cannot reproduce its own total",
			sum, gotTotal)
	}
}
