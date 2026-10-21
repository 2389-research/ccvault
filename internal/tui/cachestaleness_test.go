// ABOUTME: Tests that the Analytics tab rebuilds a Parquet cache whose schema predates the code.
// ABOUTME: Covers #149 — the staleness check used to see one column name and nothing else.

package tui

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/2389-research/ccvault/internal/analytics"
	"github.com/xitongsys/parquet-go-source/local"
	"github.com/xitongsys/parquet-go/writer"
)

// preParentSessionRecord is the exported session shape between PR #6 and #147:
// it has `source` and not `parent_session_id`. Declared here rather than
// borrowed from internal/analytics because the point of the test is that the
// TUI must handle a file the *current* exporter can no longer produce.
type preParentSessionRecord struct {
	ID              string `parquet:"name=id, type=BYTE_ARRAY, convertedtype=UTF8"`
	ProjectID       int64  `parquet:"name=project_id, type=INT64"`
	ProjectPath     string `parquet:"name=project_path, type=BYTE_ARRAY, convertedtype=UTF8"`
	StartedAt       int64  `parquet:"name=started_at, type=INT64, convertedtype=TIMESTAMP_MILLIS"`
	EndedAt         int64  `parquet:"name=ended_at, type=INT64, convertedtype=TIMESTAMP_MILLIS"`
	Model           string `parquet:"name=model, type=BYTE_ARRAY, convertedtype=UTF8"`
	TurnCount       int32  `parquet:"name=turn_count, type=INT32"`
	InputTokens     int64  `parquet:"name=input_tokens, type=INT64"`
	OutputTokens    int64  `parquet:"name=output_tokens, type=INT64"`
	CacheReadTokens int64  `parquet:"name=cache_read_tokens, type=INT64"`
	TotalTokens     int64  `parquet:"name=total_tokens, type=INT64"`
	Source          string `parquet:"name=source, type=BYTE_ARRAY, convertedtype=UTF8"`
}

func writeStaleCache(t *testing.T, cacheDir string) {
	t.Helper()
	fw, err := local.NewLocalFileWriter(filepath.Join(cacheDir, "sessions.parquet"))
	if err != nil {
		t.Fatalf("open parquet writer file: %v", err)
	}
	pw, err := writer.NewParquetWriter(fw, new(preParentSessionRecord), 4)
	if err != nil {
		_ = fw.Close()
		t.Fatalf("new parquet writer: %v", err)
	}
	now := time.Now().UnixMilli()
	if err := pw.Write(preParentSessionRecord{
		ID: "stale-1", ProjectPath: "/tmp/stale", StartedAt: now, EndedAt: now,
		Model: "claude-opus-4", TurnCount: 1, InputTokens: 10, OutputTokens: 5,
		TotalTokens: 15, Source: "claude-code",
	}); err != nil {
		t.Fatalf("write record: %v", err)
	}
	if err := pw.WriteStop(); err != nil {
		t.Fatalf("write stop: %v", err)
	}
	if err := fw.Close(); err != nil {
		t.Fatalf("close parquet writer file: %v", err)
	}
}

// preCacheWriteRecord is the exported session shape between #147 and #167: it
// has `parent_session_id` and not `cache_write_tokens`. Every cache built
// before #167 looks like this, so this is the file a real user's machine has
// on disk at the moment the new column lands.
type preCacheWriteRecord struct {
	ID              string `parquet:"name=id, type=BYTE_ARRAY, convertedtype=UTF8"`
	ProjectID       int64  `parquet:"name=project_id, type=INT64"`
	ProjectPath     string `parquet:"name=project_path, type=BYTE_ARRAY, convertedtype=UTF8"`
	StartedAt       int64  `parquet:"name=started_at, type=INT64, convertedtype=TIMESTAMP_MILLIS"`
	EndedAt         int64  `parquet:"name=ended_at, type=INT64, convertedtype=TIMESTAMP_MILLIS"`
	Model           string `parquet:"name=model, type=BYTE_ARRAY, convertedtype=UTF8"`
	TurnCount       int32  `parquet:"name=turn_count, type=INT32"`
	InputTokens     int64  `parquet:"name=input_tokens, type=INT64"`
	OutputTokens    int64  `parquet:"name=output_tokens, type=INT64"`
	CacheReadTokens int64  `parquet:"name=cache_read_tokens, type=INT64"`
	TotalTokens     int64  `parquet:"name=total_tokens, type=INT64"`
	Source          string `parquet:"name=source, type=BYTE_ARRAY, convertedtype=UTF8"`
	ParentSessionID string `parquet:"name=parent_session_id, type=BYTE_ARRAY, convertedtype=UTF8"`
}

func writePreCacheWriteCache(t *testing.T, cacheDir string) {
	t.Helper()
	fw, err := local.NewLocalFileWriter(filepath.Join(cacheDir, "sessions.parquet"))
	if err != nil {
		t.Fatalf("open parquet writer file: %v", err)
	}
	pw, err := writer.NewParquetWriter(fw, new(preCacheWriteRecord), 4)
	if err != nil {
		_ = fw.Close()
		t.Fatalf("new parquet writer: %v", err)
	}
	now := time.Now().UnixMilli()
	if err := pw.Write(preCacheWriteRecord{
		ID: "stale-cachewrite-1", ProjectPath: "/tmp/stale", StartedAt: now, EndedAt: now,
		Model: "claude-opus-4", TurnCount: 1, InputTokens: 10, OutputTokens: 5,
		CacheReadTokens: 100, TotalTokens: 115, Source: "claude-code",
	}); err != nil {
		t.Fatalf("write record: %v", err)
	}
	if err := pw.WriteStop(); err != nil {
		t.Fatalf("write stop: %v", err)
	}
	if err := fw.Close(); err != nil {
		t.Fatalf("close parquet writer file: %v", err)
	}
}

// TestAnalyticsModel_RebuildsCacheMissingCacheWriteTokens is #167 collecting on
// #149. Adding `cache_write_tokens` to SessionRecord edited no check, and this
// is the behaviour that was claimed to follow for free: a cache built by the
// previous release is recognised as stale on the first Analytics load and
// replaced, without the user being told to run `build-cache`.
func TestAnalyticsModel_RebuildsCacheMissingCacheWriteTokens(t *testing.T) {
	database := openTUITestDB(t)
	_, sessionIDs := seedProjectAndSessions(t, database, []string{"claude-code", "codex"})

	cacheDir := t.TempDir()
	writePreCacheWriteCache(t, cacheDir)

	m := NewAnalyticsModel(database, cacheDir)
	loaded, ok := m.loadAnalytics().(analyticsLoadedMsg)
	if !ok {
		t.Fatal("loadAnalytics did not return analyticsLoadedMsg")
	}
	if loaded.err != nil {
		t.Fatalf("loadAnalytics err: %v", loaded.err)
	}

	analyzer, err := analytics.NewAnalyzer(cacheDir)
	if err != nil {
		t.Fatalf("new analyzer: %v", err)
	}
	t.Cleanup(func() { _ = analyzer.Close() })

	missing, err := analyzer.SessionsParquetMissingColumns()
	if err != nil {
		t.Fatalf("missing columns: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("cache still missing %v after load; the staleness check did not fire", missing)
	}

	// The stale file held one row from a project that is not in the archive.
	// If the rebuild ran, the summary describes the seeded archive instead.
	if loaded.summary == nil {
		t.Fatal("summary is nil")
	}
	if loaded.summary.TotalSessions != len(sessionIDs) {
		t.Errorf("summary TotalSessions = %d, want %d — the stale cache was served",
			loaded.summary.TotalSessions, len(sessionIDs))
	}
}

// TestAnalyticsModel_RebuildsCacheMissingNewerColumn is #149's red test. A
// cache written before #147 has `source`, so the old name-pinned check called
// it current and the TUI kept reading it. The rebuild is observable two ways:
// the cache gains the column, and the stale row written above is gone in
// favour of the seeded sessions.
func TestAnalyticsModel_RebuildsCacheMissingNewerColumn(t *testing.T) {
	database := openTUITestDB(t)
	_, sessionIDs := seedProjectAndSessions(t, database, []string{"claude-code", "codex"})

	cacheDir := t.TempDir()
	writeStaleCache(t, cacheDir)

	m := NewAnalyticsModel(database, cacheDir)
	loaded, ok := m.loadAnalytics().(analyticsLoadedMsg)
	if !ok {
		t.Fatal("loadAnalytics did not return analyticsLoadedMsg")
	}
	if loaded.err != nil {
		t.Fatalf("loadAnalytics err: %v", loaded.err)
	}

	analyzer, err := analytics.NewAnalyzer(cacheDir)
	if err != nil {
		t.Fatalf("new analyzer: %v", err)
	}
	t.Cleanup(func() { _ = analyzer.Close() })

	missing, err := analyzer.SessionsParquetMissingColumns()
	if err != nil {
		t.Fatalf("missing columns: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("cache still missing %v after load; the staleness check did not fire", missing)
	}

	// The stale file held one row. If the rebuild ran, the summary describes
	// the seeded archive instead.
	if loaded.summary == nil {
		t.Fatal("summary is nil")
	}
	if loaded.summary.TotalSessions != len(sessionIDs) {
		t.Errorf("summary TotalSessions = %d, want %d — the stale cache was served",
			loaded.summary.TotalSessions, len(sessionIDs))
	}
}
