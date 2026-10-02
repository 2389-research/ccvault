// ABOUTME: Tests that re-applying a migration whose ADD COLUMN already landed is a no-op.
// ABOUTME: SQLite has no ADD COLUMN IF NOT EXISTS, so the migrator has to absorb that case.

package db

import (
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// TestApplyMigrationReplaysAddColumn covers the path TestMigrator_005 takes
// when it rewinds schema_version and re-runs: every migration above the rewind
// point executes a second time. CREATE INDEX and UPDATE statements already
// tolerate that; ALTER TABLE ADD COLUMN cannot be written to, because SQLite
// has no IF NOT EXISTS form for it. Without the migrator absorbing the
// duplicate-column error, any migration that adds a column makes a replay
// fail — which is exactly what 007 did when it landed.
func TestApplyMigrationReplaysAddColumn(t *testing.T) {
	db := openMemoryDB(t)
	defer func() { _ = db.Close() }()

	if _, err := db.Exec("CREATE TABLE widgets (id TEXT PRIMARY KEY)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := db.Exec("CREATE TABLE schema_version (version INTEGER NOT NULL, applied_at TEXT)"); err != nil {
		t.Fatalf("create schema_version: %v", err)
	}

	m := migration{
		version:  999,
		filename: "999_replay_probe.sql",
		// The leading comment matters: splitStatements hands each statement
		// to the executor with its comment block still attached, so a
		// prefix match on the raw text would miss every documented
		// migration in this repo.
		sql: "-- why this column exists\n" +
			"ALTER TABLE widgets ADD COLUMN colour TEXT;\n" +
			"CREATE INDEX IF NOT EXISTS idx_widgets_colour ON widgets(colour);\n",
	}

	if err := applyMigration(db, m); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if err := applyMigration(db, m); err != nil {
		t.Fatalf("replay must be a no-op, got: %v", err)
	}

	// The column is there exactly once, and only the real DDL was absorbed.
	rows, err := db.Query("PRAGMA table_info(widgets)")
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	defer func() { _ = rows.Close() }()
	count := 0
	for rows.Next() {
		var cid, notNull, pk int
		var name, colType string
		var dflt interface{}
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if name == "colour" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("colour column present %d times, want 1", count)
	}
}

// TestApplyMigrationStillFailsOnRealErrors guards the blast radius of the
// tolerance above: only a duplicate column on an ADD COLUMN is absorbed.
func TestApplyMigrationStillFailsOnRealErrors(t *testing.T) {
	db := openMemoryDB(t)
	defer func() { _ = db.Close() }()

	if _, err := db.Exec("CREATE TABLE schema_version (version INTEGER NOT NULL, applied_at TEXT)"); err != nil {
		t.Fatalf("create schema_version: %v", err)
	}

	cases := []struct {
		name string
		sql  string
		want string
	}{
		{
			name: "add column to a table that does not exist",
			sql:  "ALTER TABLE nonexistent ADD COLUMN x TEXT;\n",
			want: "nonexistent",
		},
		{
			name: "syntax error",
			sql:  "ALTER TABLE ADD COLUMN;\n",
			want: "syntax error",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := applyMigration(db, migration{version: 998, filename: "998_probe.sql", sql: tc.sql})
			if err == nil {
				t.Fatal("want an error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}
