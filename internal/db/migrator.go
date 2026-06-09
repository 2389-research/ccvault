// ABOUTME: Versioned database migration system using embedded SQL files
// ABOUTME: Manages schema evolution by tracking and applying migrations in order

package db

import (
	"database/sql"
	"embed"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// RunMigrations applies all pending migrations to the database in order.
// It creates a schema_version table to track which migrations have been applied.
// If the database has existing tables but no schema_version, it detects the
// existing state and bootstraps version records accordingly.
func RunMigrations(db *sql.DB) error {
	// Create schema_version table if it doesn't exist
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (
		version INTEGER NOT NULL,
		applied_at TEXT NOT NULL DEFAULT (datetime('now'))
	)`)
	if err != nil {
		return fmt.Errorf("create schema_version table: %w", err)
	}

	// Get current version
	currentVersion, err := getCurrentVersion(db)
	if err != nil {
		return fmt.Errorf("get current version: %w", err)
	}

	// If schema_version is empty but tables exist, bootstrap from existing state
	if currentVersion == 0 {
		bootstrapVersion, err := detectExistingState(db)
		if err != nil {
			return fmt.Errorf("detect existing state: %w", err)
		}
		if bootstrapVersion > 0 {
			for v := 1; v <= bootstrapVersion; v++ {
				if _, err := db.Exec("INSERT INTO schema_version (version) VALUES (?)", v); err != nil {
					return fmt.Errorf("bootstrap version %d: %w", v, err)
				}
			}
			currentVersion = bootstrapVersion
		}
	}

	// Load and sort migration files
	migrations, err := loadMigrations()
	if err != nil {
		return fmt.Errorf("load migrations: %w", err)
	}

	// Apply pending migrations
	for _, m := range migrations {
		if m.version <= currentVersion {
			continue
		}
		if err := applyMigration(db, m); err != nil {
			return fmt.Errorf("apply migration %03d: %w", m.version, err)
		}
	}

	return nil
}

// getCurrentVersion returns the highest applied migration version, or 0 if none.
func getCurrentVersion(db *sql.DB) (int, error) {
	var version sql.NullInt64
	err := db.QueryRow("SELECT MAX(version) FROM schema_version").Scan(&version)
	if err != nil {
		return 0, err
	}
	if !version.Valid {
		return 0, nil
	}
	return int(version.Int64), nil
}

// detectExistingState checks if the database already has tables from before the
// migration system was introduced, and returns the version to bootstrap to.
func detectExistingState(db *sql.DB) (int, error) {
	// Check if the projects table exists (indicator of migration 001)
	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='projects'").Scan(&count)
	if err != nil {
		return 0, err
	}
	if count == 0 {
		return 0, nil // Fresh database
	}

	// Projects table exists, so at least migration 001 was applied.
	// Check if has_error column exists on sessions (indicator of migration 002)
	rows, err := db.Query("PRAGMA table_info(sessions)")
	if err != nil {
		return 1, nil // Has tables but can't check columns, assume just 001
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var cid int
		var name, colType string
		var notNull int
		var dfltValue sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk); err != nil {
			continue
		}
		if name == "has_error" {
			return 2, nil // Both migrations already applied
		}
	}

	return 1, nil // Only initial schema
}

type migration struct {
	version  int
	filename string
	sql      string
}

// loadMigrations reads all embedded SQL migration files and returns them sorted by version.
func loadMigrations() ([]migration, error) {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}

	var migrations []migration
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".sql" {
			continue
		}

		// Parse version from filename (e.g., "001_initial_schema.sql" -> 1)
		var version int
		if _, err := fmt.Sscanf(entry.Name(), "%03d_", &version); err != nil {
			continue // Skip files that don't match the naming convention
		}

		content, err := migrationFS.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", entry.Name(), err)
		}

		migrations = append(migrations, migration{
			version:  version,
			filename: entry.Name(),
			sql:      string(content),
		})
	}

	sort.Slice(migrations, func(i, j int) bool {
		return migrations[i].version < migrations[j].version
	})

	return migrations, nil
}

// applyMigration executes a single migration within a transaction and records it.
func applyMigration(db *sql.DB, m migration) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Split SQL into individual statements, handling BEGIN...END blocks in triggers
	statements := splitStatements(m.sql)
	for _, stmt := range statements {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("exec statement in %s: %w\nstatement: %s", m.filename, err, stmt)
		}
	}

	// Record the migration
	if _, err := tx.Exec("INSERT INTO schema_version (version) VALUES (?)", m.version); err != nil {
		return fmt.Errorf("record migration version: %w", err)
	}

	return tx.Commit()
}

// splitStatements splits SQL text into individual statements, correctly handling
// CREATE TRIGGER blocks that contain semicolons inside BEGIN...END.
func splitStatements(sql string) []string {
	var statements []string
	var current strings.Builder
	inTrigger := false

	lines := strings.Split(sql, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Skip comment-only lines at statement boundaries
		if trimmed == "" && current.Len() == 0 {
			continue
		}

		// Detect trigger BEGIN
		upperTrimmed := strings.ToUpper(trimmed)
		if strings.HasPrefix(upperTrimmed, "CREATE TRIGGER") {
			inTrigger = true
		}

		current.WriteString(line)
		current.WriteString("\n")

		if inTrigger {
			// In a trigger block, look for END; to close
			if upperTrimmed == "END;" {
				statements = append(statements, strings.TrimSpace(current.String()))
				current.Reset()
				inTrigger = false
			}
		} else {
			// Outside triggers, semicolon at end of line ends a statement
			if strings.HasSuffix(trimmed, ";") {
				statements = append(statements, strings.TrimSpace(current.String()))
				current.Reset()
			}
		}
	}

	// Catch any trailing content
	remaining := strings.TrimSpace(current.String())
	if remaining != "" {
		statements = append(statements, remaining)
	}

	return statements
}
