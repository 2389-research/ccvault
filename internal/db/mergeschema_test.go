// ABOUTME: Tests the schema-version guard on the archive an import reads FROM
// ABOUTME: A fixture source stamped past this binary's highest migration, refused and overridden

package db

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seedSourceArchive builds a second archive with one session in it, and
// returns the path to its database file.
func seedSourceArchive(t *testing.T, sessionID string) string {
	t.Helper()

	dir := t.TempDir()
	source, err := Open(dir)
	if err != nil {
		t.Fatalf("open source archive: %v", err)
	}
	seedSession(t, source, "/proj/source", sessionID, "imported content", time.Now())
	if err := source.Close(); err != nil {
		t.Fatalf("close source archive: %v", err)
	}
	return filepath.Join(dir, dbFileName)
}

// TestMergeFrom_RefusesASourceFromANewerCCVault is #136. #105 guards the
// archive an import writes TO, and verifyMergeSchema checks the columns the
// two share — but nothing looked at the source's schema version.
//
// The exposure is narrower than #105's and worth stating precisely: the source
// is only ever read, so this cannot delete data. The risk is reading columns
// whose meaning changed. Migration 012 is the precedent: it re-keyed turns
// from id to (session_id, id) without renaming a column, so a binary that
// predated it would read both columns happily and draw the wrong conclusion
// about row identity — exactly what a column-name check cannot catch.
func TestMergeFrom_RefusesASourceFromANewerCCVault(t *testing.T) {
	known, err := MaxKnownSchemaVersion()
	if err != nil {
		t.Fatalf("read the highest migration this binary ships: %v", err)
	}

	sourcePath := seedSourceArchive(t, "newer-session")
	stampSchemaVersion(t, filepath.Dir(sourcePath), known+1)

	dest, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open destination archive: %v", err)
	}
	defer func() { _ = dest.Close() }()

	_, err = dest.MergeFrom(sourcePath)
	if err == nil {
		t.Fatal("import from an archive written by a newer ccvault succeeded unchecked")
	}

	var tooNew *MergeSourceTooNewError
	if !errors.As(err, &tooNew) {
		t.Fatalf("refusal is not a MergeSourceTooNewError: %v", err)
	}
	if tooNew.SourceVersion != known+1 || tooNew.KnownVersion != known {
		t.Errorf("refusal reports source %d against known %d, want %d against %d",
			tooNew.SourceVersion, tooNew.KnownVersion, known+1, known)
	}
	if !errors.Is(err, ErrSchemaTooNew) {
		t.Error("the refusal does not match ErrSchemaTooNew, so a caller cannot recognise it by sentinel")
	}

	// The message has to name the source, because a user with two archives
	// open needs to know which one was refused.
	if !strings.Contains(err.Error(), sourcePath) {
		t.Errorf("refusal does not name the source archive: %v", err)
	}
	// And it has to name the way through, because importing from a newer
	// archive is legitimate — it is how this archive was recovered.
	if !strings.Contains(err.Error(), "--allow-newer-schema") {
		t.Errorf("refusal does not name the override: %v", err)
	}

	// Nothing may have landed.
	var sessions int
	if err := dest.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&sessions); err != nil {
		t.Fatalf("count destination sessions: %v", err)
	}
	if sessions != 0 {
		t.Errorf("the refused import left %d sessions in the destination", sessions)
	}
}

// TestMergeFrom_AllowNewerSchemaImportsAnyway. Importing from a newer archive
// is a legitimate thing to want to do, and is in fact how the author's archive
// was recovered in September. The guard is a refusal with a way through, not a
// wall.
func TestMergeFrom_AllowNewerSchemaImportsAnyway(t *testing.T) {
	known, err := MaxKnownSchemaVersion()
	if err != nil {
		t.Fatalf("read the highest migration this binary ships: %v", err)
	}

	sourcePath := seedSourceArchive(t, "newer-session")
	stampSchemaVersion(t, filepath.Dir(sourcePath), known+1)

	dest, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open destination archive: %v", err)
	}
	defer func() { _ = dest.Close() }()

	stats, err := dest.MergeFromWithOptions(sourcePath, MergeOptions{AllowNewerSchema: true})
	if err != nil {
		t.Fatalf("import with the override: %v", err)
	}
	if stats.SessionsInserted != 1 {
		t.Errorf("imported %d sessions, want 1", stats.SessionsInserted)
	}

	var turns int
	if err := dest.QueryRow("SELECT COUNT(*) FROM turns WHERE session_id = 'newer-session'").Scan(&turns); err != nil {
		t.Fatalf("count imported turns: %v", err)
	}
	if turns != 1 {
		t.Errorf("imported %d turns, want 1", turns)
	}
}

// TestMergeFrom_AcceptsASourceAtOrBelowThisSchema. The ordinary case, and the
// one a guard with an off-by-one would break: an archive at exactly this
// binary's schema is not newer than it.
func TestMergeFrom_AcceptsASourceAtOrBelowThisSchema(t *testing.T) {
	sourcePath := seedSourceArchive(t, "current-session")

	dest, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open destination archive: %v", err)
	}
	defer func() { _ = dest.Close() }()

	stats, err := dest.MergeFrom(sourcePath)
	if err != nil {
		t.Fatalf("import from an archive at this binary's own schema: %v", err)
	}
	if stats.SessionsInserted != 1 {
		t.Errorf("imported %d sessions, want 1", stats.SessionsInserted)
	}
}

// TestMergeFrom_AcceptsASourcePredatingTheMigrationSystem. An archive with no
// schema_version table at all is older, not newer, and is the main thing
// `ccvault import` is pointed at: a copy taken off another machine, or a
// backup predating a column.
func TestMergeFrom_AcceptsASourcePredatingTheMigrationSystem(t *testing.T) {
	sourcePath := seedSourceArchive(t, "ancient-session")

	source, err := OpenForRepair(filepath.Dir(sourcePath))
	if err != nil {
		t.Fatalf("reopen the source archive: %v", err)
	}
	if _, err := source.Exec("DROP TABLE schema_version"); err != nil {
		t.Fatalf("drop the source's schema_version table: %v", err)
	}
	if err := source.Close(); err != nil {
		t.Fatalf("close the source archive: %v", err)
	}

	dest, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open destination archive: %v", err)
	}
	defer func() { _ = dest.Close() }()

	if _, err := dest.MergeFrom(sourcePath); err != nil {
		t.Fatalf("import from an archive with no schema_version table: %v", err)
	}
}
