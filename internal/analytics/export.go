// ABOUTME: Parquet export functionality for analytics
// ABOUTME: Exports session data to Parquet format for DuckDB queries

package analytics

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/2389-research/ccvault/internal/db"
	"github.com/xitongsys/parquet-go-source/local"
	"github.com/xitongsys/parquet-go/parquet"
	"github.com/xitongsys/parquet-go/writer"
)

// SessionRecord is a Parquet-compatible session record. It is the only record
// type this package declares, and sessions.parquet the only file Export
// writes. Two sibling types once sat beside it describing files that were
// never produced — TurnRecord (deleted in #77) and ToolRecord (#139) — so a
// record type added here without a writer should be read as the same mistake
// a third time.
type SessionRecord struct {
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

	// ParentSessionID is the id of the transcript that dispatched this one,
	// empty for a top-level session — the same "" means top-level that
	// models.Session uses, rather than a nullable parquet column that every
	// reader would have to COALESCE. Appended rather than placed next to ID
	// so the existing column order is undisturbed.
	//
	// Without it the cache counts subagent sessions but cannot separate
	// them (#74), and a parquet file copied away from the archive has no
	// join back to SQLite to recover the split.
	ParentSessionID string `parquet:"name=parent_session_id, type=BYTE_ARRAY, convertedtype=UTF8"`
}

// Exporter exports data to Parquet format
type Exporter struct {
	db         *db.DB
	outputDir  string
	onProgress func(string)
}

// NewExporter creates a new Parquet exporter
func NewExporter(database *db.DB, outputDir string) *Exporter {
	return &Exporter{
		db:         database,
		outputDir:  outputDir,
		onProgress: func(string) {},
	}
}

// SetProgressCallback sets the progress callback
func (e *Exporter) SetProgressCallback(fn func(string)) {
	e.onProgress = fn
}

// Export exports all data to Parquet files
func (e *Exporter) Export() error {
	// Ensure output directory exists
	if err := os.MkdirAll(e.outputDir, 0755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}

	// Export sessions
	e.onProgress("Exporting sessions...")
	if err := e.exportSessions(); err != nil {
		return fmt.Errorf("export sessions: %w", err)
	}

	e.onProgress("Analytics cache built successfully")
	return nil
}

func (e *Exporter) exportSessions() error {
	path := filepath.Join(e.outputDir, "sessions.parquet")

	fw, err := local.NewLocalFileWriter(path)
	if err != nil {
		return fmt.Errorf("create file writer: %w", err)
	}
	defer func() { _ = fw.Close() }()

	pw, err := writer.NewParquetWriter(fw, new(SessionRecord), 4)
	if err != nil {
		return fmt.Errorf("create parquet writer: %w", err)
	}
	pw.CompressionType = parquet.CompressionCodec_SNAPPY

	// Get all projects for path lookup
	projects, err := e.db.GetProjects("", 0, 0)
	if err != nil {
		return fmt.Errorf("get projects: %w", err)
	}
	projectPaths := make(map[int64]string)
	for _, p := range projects {
		projectPaths[p.ID] = p.Path
	}

	// Get all sessions
	sessions, err := e.db.GetSessions(0, 0, 0)
	if err != nil {
		return fmt.Errorf("get sessions: %w", err)
	}

	for _, s := range sessions {
		record := SessionRecord{
			ID:              s.ID,
			ProjectID:       s.ProjectID,
			ProjectPath:     projectPaths[s.ProjectID],
			StartedAt:       s.StartedAt.UnixMilli(),
			EndedAt:         s.EndedAt.UnixMilli(),
			Model:           s.Model,
			TurnCount:       int32(s.TurnCount),
			InputTokens:     s.InputTokens,
			OutputTokens:    s.OutputTokens,
			CacheReadTokens: s.CacheReadTokens,
			TotalTokens:     s.InputTokens + s.OutputTokens + s.CacheReadTokens + s.CacheWriteTokens,
			Source:          s.Source,
			ParentSessionID: s.ParentSessionID,
		}
		if err := pw.Write(record); err != nil {
			return fmt.Errorf("write record: %w", err)
		}
	}

	if err := pw.WriteStop(); err != nil {
		return fmt.Errorf("write stop: %w", err)
	}

	e.onProgress(fmt.Sprintf("Exported %d sessions to %s", len(sessions), path))
	return nil
}
