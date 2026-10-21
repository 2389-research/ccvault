// ABOUTME: Tests that the Parquet cache staleness check follows SessionRecord's schema.
// ABOUTME: A check pinned to one column name goes blind the next time a column is added.

package analytics

import (
	"slices"
	"testing"
	"time"

	"github.com/xitongsys/parquet-go-source/local"
	"github.com/xitongsys/parquet-go/writer"
)

// preParentSessionRecord is the SessionRecord shape as of PR #6 through #147:
// it carries `source`, so the old name-pinned check called it fresh, but it
// predates `parent_session_id`. This is the exact file #149 describes — a
// cache the TUI kept forever because the one column it knew to look for was
// already there.
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

// writePreParentParquet writes a sessions.parquet in the pre-#147 shape.
func writePreParentParquet(t *testing.T, path string) {
	t.Helper()
	fw, err := local.NewLocalFileWriter(path)
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
		ID: "pre-parent-1", ProjectPath: "/tmp/pre-parent", StartedAt: now, EndedAt: now,
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

// TestSessionRecordColumnsTracksTheStruct is the whole point of deriving the
// expected column set by reflection: the list cannot drift from the struct,
// because it *is* the struct. A field added to SessionRecord shows up here
// with no edit, which is what stops #149 from recurring.
func TestSessionRecordColumnsTracksTheStruct(t *testing.T) {
	cols := sessionRecordColumns()

	// Spot-check the two columns whose additions each caused a staleness bug,
	// plus the one added after the check started deriving itself (#167) — the
	// first column to be covered without anyone editing a list.
	for _, want := range []string{"source", "parent_session_id", "cache_write_tokens"} {
		if !slices.Contains(cols, want) {
			t.Errorf("sessionRecordColumns() = %v, missing %q", cols, want)
		}
	}
	// Every parquet-tagged field must be represented, or the check would
	// under-report staleness for whichever field got skipped.
	if len(cols) != 14 {
		t.Errorf("sessionRecordColumns() returned %d columns (%v); SessionRecord declares 14 — "+
			"if you added a field, this number moves and nothing else has to", len(cols), cols)
	}
}

// TestSessionsParquetMissingColumns_FreshExportIsComplete: the check must not
// cry stale on a cache the current exporter just wrote, or the TUI rebuilds on
// every load.
func TestSessionsParquetMissingColumns_FreshExportIsComplete(t *testing.T) {
	analyzer, _ := newAnalyticsFixture(t)

	missing, err := analyzer.SessionsParquetMissingColumns()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("fresh export reports missing columns %v, want none", missing)
	}
}

// TestSessionsParquetMissingColumns_SeesParentSessionID is the red test for
// #149. The old check asked "does this file have `source`" — this file does,
// and yet it is stale.
func TestSessionsParquetMissingColumns_SeesParentSessionID(t *testing.T) {
	cacheDir := t.TempDir()
	writePreParentParquet(t, cacheDir+"/sessions.parquet")

	analyzer, err := NewAnalyzer(cacheDir)
	if err != nil {
		t.Fatalf("new analyzer: %v", err)
	}
	t.Cleanup(func() { _ = analyzer.Close() })

	missing, err := analyzer.SessionsParquetMissingColumns()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !slices.Contains(missing, "parent_session_id") {
		t.Errorf("missing columns = %v, want parent_session_id among them", missing)
	}
	if slices.Contains(missing, "source") {
		t.Errorf("missing columns = %v; this file has `source` and must not be "+
			"reported as lacking it", missing)
	}
}

// preCacheWriteSessionRecord is the SessionRecord shape between #147 and #167:
// it has `parent_session_id`, so #149's check had nothing to object to, but it
// predates `cache_write_tokens`.
//
// This is the file every existing cache on disk is, and the reason #167 did
// not have to touch the staleness check at all: the expected column set is
// read off the struct, so declaring the field is what makes this file stale.
type preCacheWriteSessionRecord struct {
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

// writePreCacheWriteParquet writes a sessions.parquet in the pre-#167 shape.
func writePreCacheWriteParquet(t *testing.T, path string) {
	t.Helper()
	fw, err := local.NewLocalFileWriter(path)
	if err != nil {
		t.Fatalf("open parquet writer file: %v", err)
	}
	pw, err := writer.NewParquetWriter(fw, new(preCacheWriteSessionRecord), 4)
	if err != nil {
		_ = fw.Close()
		t.Fatalf("new parquet writer: %v", err)
	}
	now := time.Now().UnixMilli()
	if err := pw.Write(preCacheWriteSessionRecord{
		ID: "pre-cache-write-1", ProjectPath: "/tmp/pre-cache-write", StartedAt: now, EndedAt: now,
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

// TestSessionsParquetMissingColumns_SeesCacheWriteTokens is the check earning
// its keep a second time. #167 added a column and changed nothing here; this
// test asserts the claim #149 made — that the next column is covered on
// declaration — against the next column, rather than taking it on faith.
func TestSessionsParquetMissingColumns_SeesCacheWriteTokens(t *testing.T) {
	cacheDir := t.TempDir()
	writePreCacheWriteParquet(t, cacheDir+"/sessions.parquet")

	analyzer, err := NewAnalyzer(cacheDir)
	if err != nil {
		t.Fatalf("new analyzer: %v", err)
	}
	t.Cleanup(func() { _ = analyzer.Close() })

	missing, err := analyzer.SessionsParquetMissingColumns()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !slices.Contains(missing, "cache_write_tokens") {
		t.Errorf("missing columns = %v, want cache_write_tokens among them", missing)
	}
	for _, present := range []string{"source", "parent_session_id", "cache_read_tokens"} {
		if slices.Contains(missing, present) {
			t.Errorf("missing columns = %v; this file has %q and must not be reported as lacking it",
				missing, present)
		}
	}
}

// TestSessionsParquetMissingColumns_LegacyFileMissesBoth: the pre-#6 shape is
// stale for two reasons now, and the check should name both rather than stop
// at the first.
func TestSessionsParquetMissingColumns_LegacyFileMissesBoth(t *testing.T) {
	cacheDir := t.TempDir()
	now := time.Now().UnixMilli()
	writeLegacyParquet(t, cacheDir+"/sessions.parquet", []legacySessionRecord{
		{ID: "legacy-1", ProjectPath: "/tmp/legacy", StartedAt: now, EndedAt: now,
			Model: "claude-2", TurnCount: 1, InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
	})

	analyzer, err := NewAnalyzer(cacheDir)
	if err != nil {
		t.Fatalf("new analyzer: %v", err)
	}
	t.Cleanup(func() { _ = analyzer.Close() })

	missing, err := analyzer.SessionsParquetMissingColumns()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{"source", "parent_session_id"} {
		if !slices.Contains(missing, want) {
			t.Errorf("missing columns = %v, want %q among them", missing, want)
		}
	}
}

// TestSessionsParquetMissingColumns_NoFileNeedsEverything lets one call answer
// "should I rebuild": no cache at all carries none of the expected columns, so
// the caller does not need a separate existence check to reach the same answer.
func TestSessionsParquetMissingColumns_NoFileNeedsEverything(t *testing.T) {
	analyzer, err := NewAnalyzer(t.TempDir())
	if err != nil {
		t.Fatalf("new analyzer: %v", err)
	}
	t.Cleanup(func() { _ = analyzer.Close() })

	missing, err := analyzer.SessionsParquetMissingColumns()
	if err != nil {
		t.Fatalf("missing file should not be an error: %v", err)
	}
	if len(missing) != len(sessionRecordColumns()) {
		t.Errorf("missing columns = %v, want all %d expected columns",
			missing, len(sessionRecordColumns()))
	}
}
