// ABOUTME: Tests that get_analytics notices a Parquet cache whose schema predates the exporter, the way the TUI's Analytics tab does.
// ABOUTME: #149 taught one of the two consumers to self-heal; this is the other, and the agent-facing one.

package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/2389-research/ccvault/internal/analytics"
	"github.com/2389-research/ccvault/internal/buildinfo"
	"github.com/2389-research/ccvault/internal/config"
	"github.com/2389-research/ccvault/internal/db"
	"github.com/2389-research/ccvault/pkg/models"
	"github.com/xitongsys/parquet-go-source/local"
	"github.com/xitongsys/parquet-go/writer"
)

// preCacheWriteSessionRecord is the exported session shape between #147 and
// #167: it has `parent_session_id` and not `cache_write_tokens`. Every cache
// built before #167 looks like this, so it is the file a real user's machine
// has on disk at the moment `ccvault mcp` is next started.
//
// Declared here rather than borrowed from internal/analytics because the point
// is a file the current exporter can no longer produce.
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

// writePreCacheWriteParquet writes one session into cacheDir in the pre-#167
// shape.
func writePreCacheWriteParquet(t *testing.T, cacheDir string) {
	t.Helper()

	fw, err := local.NewLocalFileWriter(filepath.Join(cacheDir, "sessions.parquet"))
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
		ID: "stale-1", ProjectPath: "/tmp/stale", StartedAt: now, EndedAt: now,
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

// newServerWithStaleCache builds a server whose analytics cache is a pre-#167
// Parquet file, over an archive holding two sessions.
func newServerWithStaleCache(t *testing.T) *Server {
	t.Helper()

	dataDir := t.TempDir()
	database, err := db.Open(dataDir)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	p := seedProject(t, database, "/test/proj")
	for _, id := range []string{"session-1", "session-2"} {
		seedSession(t, database, id, p.ID)
		if err := database.InsertTurns([]models.Turn{{
			ID:        id + "-turn-0",
			SessionID: id,
			Type:      "user",
			Timestamp: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			Ordinal:   0,
			Content:   "hello",
		}}); err != nil {
			t.Fatalf("insert turn of %s: %v", id, err)
		}
	}

	cacheDir := filepath.Join(dataDir, "analytics")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatalf("make cache dir: %v", err)
	}
	writePreCacheWriteParquet(t, cacheDir)

	s, err := NewServer(database, &config.Config{DataDir: dataDir}, buildinfo.Report{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestGetAnalytics_RebuildsACacheOlderThanTheExporter is the fix. The MCP
// server built an Analyzer and never asked whether the cache it points at
// still matches SessionRecord, so `ccvault mcp`'s get_analytics served a
// pre-#167 cache's numbers indefinitely — and did not error, because no
// production query selects the new column. #149 taught internal/tui to
// self-heal; this is the other consumer, and the agent-facing one.
func TestGetAnalytics_RebuildsACacheOlderThanTheExporter(t *testing.T) {
	s := newServerWithStaleCache(t)

	// Red before the fix: the stale cache is served and nothing says so.
	result, err := s.getAnalytics(map[string]interface{}{})
	if err != nil {
		t.Fatalf("get_analytics: %v", err)
	}
	if result == nil {
		t.Fatal("get_analytics returned nothing")
	}

	cacheDir := filepath.Join(s.cfg.DataDir, "analytics")
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
		t.Errorf("the cache is still missing %v after get_analytics; the staleness check did not fire", missing)
	}

	// The rebuild is observable in the answer, not only on disk: the stale
	// file held one session from a project the archive does not contain, and
	// the archive holds two.
	m, ok := result.(map[string]interface{})
	if !ok {
		t.Fatalf("result is %T, want a map", result)
	}
	projects, ok := m["top_projects"].([]analytics.ProjectStats)
	if !ok {
		t.Fatalf("top_projects is %#v, want project stats", m["top_projects"])
	}
	for _, p := range projects {
		if p.ProjectPath == "/tmp/stale" {
			t.Error("get_analytics served the stale cache's project")
		}
	}
}

// TestGetAnalytics_ServesTheSummaryWhenARebuildFails keeps the self-heal from
// becoming a new way for get_analytics to fail outright. The handler already
// reports a degraded DuckDB query as a warning beside the rest of the answer,
// and a cache it could not refresh belongs in the same list: an agent that
// asked for analytics and got an error learns less than one that got the
// SQLite summary plus a sentence saying the Parquet half is stale.
func TestGetAnalytics_ServesTheSummaryWhenARebuildFails(t *testing.T) {
	s := newServerWithStaleCache(t)

	// A cache directory replaced by a file is the simplest thing the exporter
	// cannot write into, and it stands for every real reason a rebuild fails:
	// no disk space, no permission, a cache on a read-only volume.
	cacheDir := filepath.Join(s.cfg.DataDir, "analytics")
	if err := os.RemoveAll(cacheDir); err != nil {
		t.Fatalf("remove the cache directory: %v", err)
	}
	if err := os.WriteFile(cacheDir, []byte("not a directory\n"), 0o644); err != nil {
		t.Fatalf("put a file where the cache directory was: %v", err)
	}

	result, err := s.getAnalytics(map[string]interface{}{})
	if err != nil {
		t.Fatalf("a cache it could not refresh made get_analytics fail outright: %v", err)
	}

	m, ok := result.(map[string]interface{})
	if !ok {
		t.Fatalf("result is %T, want a map", result)
	}
	if m["summary"] == nil {
		t.Error("the SQLite summary was dropped along with the stale cache")
	}
	warnings, ok := m["warnings"].([]string)
	if !ok || len(warnings) == 0 {
		t.Fatalf("no warning names the stale cache: %#v", m["warnings"])
	}
	var named bool
	for _, w := range warnings {
		if strings.Contains(w, "cache") && strings.Contains(w, "build-cache") {
			named = true
		}
	}
	if !named {
		t.Errorf("no warning says the cache is stale and how to rebuild it: %v", warnings)
	}
}
