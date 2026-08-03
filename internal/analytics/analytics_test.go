// ABOUTME: Tests for the analytics package — first coverage per issue #8.
// ABOUTME: Real SQLite fixture, real Parquet round-trip, real DuckDB queries.

package analytics

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/2389-research/ccvault/internal/db"
	"github.com/xitongsys/parquet-go-source/local"
	"github.com/xitongsys/parquet-go/parquet"
	"github.com/xitongsys/parquet-go/writer"
)

// newAnalyticsFixture creates a DB + seed data + exported parquet + open
// analyzer in one shot. Returns the analyzer plus the cache dir so tests
// can inspect the on-disk artifacts.
func newAnalyticsFixture(t *testing.T) (*Analyzer, string) {
	t.Helper()

	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	now := time.Now().UTC()
	// Two projects, one session each — different sources + models so
	// GetTopProjects / GetTokensByModel / *BySource have real content.
	seedSession(t, database, now, "/tmp/proj-a", "proj-a", "sess-a-1",
		"claude-sonnet-4", "claude-code", 100, 50, 20)
	seedSession(t, database, now.Add(-24*time.Hour), "/tmp/proj-a", "proj-a", "sess-a-2",
		"claude-sonnet-4", "claude-code", 200, 100, 0)
	seedSession(t, database, now.Add(-2*24*time.Hour), "/tmp/proj-b", "proj-b", "sess-b-1",
		"gpt-4", "codex", 80, 40, 0)

	cacheDir := t.TempDir()
	exporter := NewExporter(database, cacheDir)
	if err := exporter.Export(); err != nil {
		t.Fatalf("export: %v", err)
	}

	analyzer, err := NewAnalyzer(cacheDir)
	if err != nil {
		t.Fatalf("new analyzer: %v", err)
	}
	t.Cleanup(func() { _ = analyzer.Close() })

	return analyzer, cacheDir
}

// seedSession inserts one project (upserting on path collision) plus one
// session. Uses time.Time bindings consistently with production code so
// modernc/sqlite serializes datetimes the same way for both fixture and
// query paths.
func seedSession(t *testing.T, d *db.DB, when time.Time, projectPath, displayName, sessionID, model, source string, inputTok, outputTok, cacheTok int64) {
	t.Helper()

	// Get-or-create the project. GetProjectByPath returns nil when not found.
	proj, err := d.GetProjectByPath(projectPath)
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	var projectID int64
	if proj == nil {
		res, err := d.Exec(`INSERT INTO projects
			(path, display_name, first_seen_at, last_activity_at, session_count, total_tokens, source)
			VALUES (?, ?, ?, ?, 0, 0, ?)`,
			projectPath, displayName, when, when, source)
		if err != nil {
			t.Fatalf("insert project: %v", err)
		}
		projectID, _ = res.LastInsertId()
	} else {
		projectID = proj.ID
	}

	_, err = d.Exec(`INSERT INTO sessions
		(id, project_id, model, git_branch, started_at, ended_at, turn_count, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, source_file, source)
		VALUES (?, ?, ?, 'main', ?, ?, 2, ?, ?, ?, 0, ?, ?)`,
		sessionID, projectID, model, when, when, inputTok, outputTok, cacheTok,
		"/tmp/x.jsonl", source)
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}
}

// --- Exporter ---

// TestExporter_ExportProducesParquet verifies the exporter round-trips
// SQLite session data into a parquet file at the expected path.
func TestExporter_ExportProducesParquet(t *testing.T) {
	_, cacheDir := newAnalyticsFixture(t)

	info, err := os.Stat(filepath.Join(cacheDir, "sessions.parquet"))
	if err != nil {
		t.Fatalf("sessions.parquet missing: %v", err)
	}
	if info.Size() == 0 {
		t.Errorf("sessions.parquet is empty")
	}
}

// --- SessionsParquetHasSource ---

// TestAnalyzer_SessionsParquetHasSource_MissingFileReturnsFalse ensures a
// nonexistent parquet is reported as "no source column" without erroring
// so the TUI can distinguish "hasn't run yet" from "has legacy cache".
func TestAnalyzer_SessionsParquetHasSource_MissingFileReturnsFalse(t *testing.T) {
	cacheDir := t.TempDir()
	analyzer, err := NewAnalyzer(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = analyzer.Close() }()

	got, err := analyzer.SessionsParquetHasSource()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got {
		t.Error("expected false for missing file, got true")
	}
}

// TestAnalyzer_SessionsParquetHasSource_FreshFileReturnsTrue verifies a
// fresh export (which uses the current SessionRecord shape) reports the
// source column as present.
func TestAnalyzer_SessionsParquetHasSource_FreshFileReturnsTrue(t *testing.T) {
	analyzer, _ := newAnalyticsFixture(t)

	got, err := analyzer.SessionsParquetHasSource()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got {
		t.Error("expected true after fresh export, got false")
	}
}

// legacySessionRecord mirrors the pre-PR-#6 SessionRecord shape — same
// fields as the current SessionRecord minus the Source column. We write
// one directly via parquet-go's writer to simulate a stale cache from
// before multi-source support landed.
type legacySessionRecord struct {
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
}

// writeLegacyParquet writes a parquet file at path with the pre-#6 schema
// (no `source` column). Used to exercise the schema-compat detection and
// the COALESCE(source, 'claude-code') fallback in *BySource queries.
func writeLegacyParquet(t *testing.T, path string, rows []legacySessionRecord) {
	t.Helper()
	fw, err := local.NewLocalFileWriter(path)
	if err != nil {
		t.Fatalf("open parquet writer file: %v", err)
	}
	pw, err := writer.NewParquetWriter(fw, new(legacySessionRecord), 4)
	if err != nil {
		_ = fw.Close()
		t.Fatalf("new parquet writer: %v", err)
	}
	pw.CompressionType = parquet.CompressionCodec_SNAPPY

	for _, r := range rows {
		if err := pw.Write(r); err != nil {
			t.Fatalf("write parquet row: %v", err)
		}
	}
	if err := pw.WriteStop(); err != nil {
		t.Fatalf("write stop: %v", err)
	}
	if err := fw.Close(); err != nil {
		t.Fatalf("close parquet writer: %v", err)
	}
}

// TestAnalyzer_SessionsParquetHasSource_LegacyFileReturnsFalse writes a
// synthetic parquet with the pre-PR-#6 schema (no source column) and
// verifies the schema-compat check reports it as legacy.
func TestAnalyzer_SessionsParquetHasSource_LegacyFileReturnsFalse(t *testing.T) {
	cacheDir := t.TempDir()
	now := time.Now().UnixMilli()
	writeLegacyParquet(t, filepath.Join(cacheDir, "sessions.parquet"), []legacySessionRecord{
		{ID: "legacy-1", ProjectPath: "/tmp/legacy", StartedAt: now, EndedAt: now,
			Model: "claude-2", TurnCount: 1, InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
	})

	analyzer, err := NewAnalyzer(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = analyzer.Close() }()

	got, err := analyzer.SessionsParquetHasSource()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got {
		t.Error("expected false for legacy parquet without source column, got true")
	}
}

// --- Query methods ---

func TestAnalyzer_GetSummary(t *testing.T) {
	analyzer, _ := newAnalyticsFixture(t)

	s, err := analyzer.GetSummary()
	if err != nil {
		t.Fatalf("GetSummary: %v", err)
	}
	if s.TotalSessions != 3 {
		t.Errorf("TotalSessions = %d, want 3", s.TotalSessions)
	}
	// Fixture: 100+50+20 + 200+100 + 80+40 = 590
	if s.TotalTokens != 590 {
		t.Errorf("TotalTokens = %d, want 590", s.TotalTokens)
	}
	if s.UniqueModels != 2 {
		t.Errorf("UniqueModels = %d, want 2 (claude-sonnet-4, gpt-4)", s.UniqueModels)
	}
}

func TestAnalyzer_GetTokensByDay(t *testing.T) {
	analyzer, _ := newAnalyticsFixture(t)

	daily, err := analyzer.GetTokensByDay(30)
	if err != nil {
		t.Fatalf("GetTokensByDay: %v", err)
	}
	if len(daily) == 0 {
		t.Fatal("expected at least one day of results")
	}
	// Total across all days should equal the summary total.
	var total int64
	for _, d := range daily {
		total += d.TotalTokens
	}
	if total != 590 {
		t.Errorf("sum(TotalTokens) across days = %d, want 590", total)
	}
}

func TestAnalyzer_GetTopProjects(t *testing.T) {
	analyzer, _ := newAnalyticsFixture(t)

	projects, err := analyzer.GetTopProjects(10)
	if err != nil {
		t.Fatalf("GetTopProjects: %v", err)
	}
	if len(projects) != 2 {
		t.Fatalf("expected 2 projects, got %d", len(projects))
	}
	// proj-a has 100+50+20 + 200+100 = 470 tokens; proj-b has 80+40 = 120.
	// Ordering must be by tokens DESC.
	if projects[0].ProjectPath != "/tmp/proj-a" {
		t.Errorf("top project = %q, want /tmp/proj-a", projects[0].ProjectPath)
	}
	if projects[0].TotalTokens != 470 {
		t.Errorf("proj-a tokens = %d, want 470", projects[0].TotalTokens)
	}
	if projects[1].ProjectPath != "/tmp/proj-b" {
		t.Errorf("second project = %q, want /tmp/proj-b", projects[1].ProjectPath)
	}
}

func TestAnalyzer_GetTokensByModel(t *testing.T) {
	analyzer, _ := newAnalyticsFixture(t)

	models, err := analyzer.GetTokensByModel()
	if err != nil {
		t.Fatalf("GetTokensByModel: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(models))
	}
	// claude-sonnet-4: two sessions, 100+50+20+200+100 = 470 tokens
	// gpt-4: one session, 80+40 = 120 tokens
	found := map[string]ModelStats{}
	for _, m := range models {
		found[m.Model] = m
	}
	if found["claude-sonnet-4"].TotalTokens != 470 {
		t.Errorf("claude-sonnet-4 tokens = %d, want 470", found["claude-sonnet-4"].TotalTokens)
	}
	if found["gpt-4"].TotalTokens != 120 {
		t.Errorf("gpt-4 tokens = %d, want 120", found["gpt-4"].TotalTokens)
	}
}

func TestAnalyzer_GetTokensBySource(t *testing.T) {
	analyzer, _ := newAnalyticsFixture(t)

	bySource, err := analyzer.GetTokensBySource()
	if err != nil {
		t.Fatalf("GetTokensBySource: %v", err)
	}
	if len(bySource) != 2 {
		t.Fatalf("expected 2 sources, got %d", len(bySource))
	}
	found := map[string]int64{}
	for _, s := range bySource {
		found[s.Source] = s.TotalTokens
	}
	if found["claude-code"] != 470 {
		t.Errorf("claude-code tokens = %d, want 470", found["claude-code"])
	}
	if found["codex"] != 120 {
		t.Errorf("codex tokens = %d, want 120", found["codex"])
	}
}

func TestAnalyzer_GetSessionsBySource(t *testing.T) {
	analyzer, _ := newAnalyticsFixture(t)

	bySource, err := analyzer.GetSessionsBySource()
	if err != nil {
		t.Fatalf("GetSessionsBySource: %v", err)
	}
	found := map[string]int{}
	for _, s := range bySource {
		found[s.Source] = s.SessionCount
	}
	if found["claude-code"] != 2 {
		t.Errorf("claude-code sessions = %d, want 2", found["claude-code"])
	}
	if found["codex"] != 1 {
		t.Errorf("codex sessions = %d, want 1", found["codex"])
	}
}

// TestAnalyzer_LegacyParquetErrorsCleanlyOnSourceQueries documents that
// source-aware queries fail with a clear "column not found" error against
// a legacy parquet, rather than silently returning misleading data. The
// COALESCE(source, 'claude-code') clause in the query is aspirational
// defensive coding — DuckDB errors at bind-time when the column is
// missing, so the fallback never fires. The real safety net is that
// callers (TUI) call SessionsParquetHasSource first and rebuild the
// cache when it returns false; this test guards the "the caller didn't
// rebuild" behavior so a future regression that silently returns wrong
// data would be caught.
func TestAnalyzer_LegacyParquetErrorsCleanlyOnSourceQueries(t *testing.T) {
	cacheDir := t.TempDir()
	now := time.Now().UnixMilli()
	writeLegacyParquet(t, filepath.Join(cacheDir, "sessions.parquet"), []legacySessionRecord{
		{ID: "l1", ProjectPath: "/tmp/legacy", StartedAt: now, EndedAt: now,
			Model: "claude-2", TurnCount: 1, InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
	})

	analyzer, err := NewAnalyzer(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = analyzer.Close() }()

	// Source queries on legacy parquets should surface a clear error, not
	// return misleading rows.
	if _, err := analyzer.GetTokensBySource(); err == nil {
		t.Error("GetTokensBySource on legacy parquet: expected error, got nil")
	}
	if _, err := analyzer.GetSessionsBySource(); err == nil {
		t.Error("GetSessionsBySource on legacy parquet: expected error, got nil")
	}

	// Non-source queries should still work fine — they don't touch the
	// missing column.
	if _, err := analyzer.GetSummary(); err != nil {
		t.Errorf("GetSummary on legacy parquet should still work: %v", err)
	}
	if _, err := analyzer.GetTokensByModel(); err != nil {
		t.Errorf("GetTokensByModel on legacy parquet should still work: %v", err)
	}
}

// TestAnalyzer_MissingCacheErrors verifies that each query returns a
// user-friendly error when the parquet file is missing, rather than
// panicking or returning empty results.
func TestAnalyzer_MissingCacheErrors(t *testing.T) {
	cacheDir := t.TempDir()
	analyzer, err := NewAnalyzer(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = analyzer.Close() }()

	if _, err := analyzer.GetSummary(); err == nil {
		t.Error("GetSummary: expected error on missing cache")
	}
	if _, err := analyzer.GetTokensByDay(30); err == nil {
		t.Error("GetTokensByDay: expected error on missing cache")
	}
	if _, err := analyzer.GetTopProjects(10); err == nil {
		t.Error("GetTopProjects: expected error on missing cache")
	}
	if _, err := analyzer.GetTokensByModel(); err == nil {
		t.Error("GetTokensByModel: expected error on missing cache")
	}
	if _, err := analyzer.GetTokensBySource(); err == nil {
		t.Error("GetTokensBySource: expected error on missing cache")
	}
	if _, err := analyzer.GetSessionsBySource(); err == nil {
		t.Error("GetSessionsBySource: expected error on missing cache")
	}
}
