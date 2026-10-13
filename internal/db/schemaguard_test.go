// ABOUTME: Tests the guard that refuses an archive written by a newer ccvault
// ABOUTME: Covers Open, OpenForRead, RunMigrations and Compact, the only openers

package db

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fileDigest hashes a file so a test can assert that a refused operation left
// it byte-for-byte as it was.
func fileDigest(path string) (string, error) {
	contents, err := os.ReadFile(path) //nolint:gosec // test-controlled path
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(contents)
	return hex.EncodeToString(sum[:]), nil
}

// stampSchemaVersion records a migration version against an existing archive
// without applying anything, which is what a newer ccvault leaves behind: a
// schema_version row this binary has no migration for.
func stampSchemaVersion(t *testing.T, dataDir string, version int) {
	t.Helper()

	pool, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, dbFileName))
	if err != nil {
		t.Fatalf("open archive directly: %v", err)
	}
	defer func() { _ = pool.Close() }()

	if _, err := pool.Exec("INSERT INTO schema_version (version) VALUES (?)", version); err != nil {
		t.Fatalf("stamp schema version %d: %v", version, err)
	}
}

// archiveFromTheFuture builds a normal archive and then marks it as carrying
// one migration more than this binary ships.
func archiveFromTheFuture(t *testing.T) (dataDir string, future int) {
	t.Helper()

	dataDir = t.TempDir()
	database, err := Open(dataDir)
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close archive: %v", err)
	}

	known, err := MaxKnownSchemaVersion()
	if err != nil {
		t.Fatalf("max known schema version: %v", err)
	}
	future = known + 1
	stampSchemaVersion(t, dataDir, future)
	return dataDir, future
}

// TestMaxKnownSchemaVersion_IsTheHighestMigrationShipped keeps the number the
// guard compares against tied to the files, so adding a migration cannot leave
// the guard refusing the archive its own binary just wrote.
func TestMaxKnownSchemaVersion_IsTheHighestMigrationShipped(t *testing.T) {
	known, err := MaxKnownSchemaVersion()
	if err != nil {
		t.Fatalf("max known schema version: %v", err)
	}

	migrations, err := loadMigrations()
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	if len(migrations) == 0 {
		t.Fatal("no migrations loaded, so the guard would compare against nothing")
	}

	highest := 0
	for _, m := range migrations {
		if m.version > highest {
			highest = m.version
		}
	}
	if known != highest {
		t.Errorf("MaxKnownSchemaVersion() = %d, highest migration file is %d", known, highest)
	}

	// A fresh archive must land exactly on it, or the guard would refuse a
	// database this very binary created.
	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open fresh archive: %v", err)
	}
	defer func() { _ = database.Close() }()

	var applied int
	if err := database.QueryRow("SELECT MAX(version) FROM schema_version").Scan(&applied); err != nil {
		t.Fatalf("read applied version: %v", err)
	}
	if applied != known {
		t.Errorf("fresh archive is at schema %d, binary knows %d", applied, known)
	}
}

// TestOpen_RefusesAnArchiveWrittenByANewerBinary is the incident of #105: a
// v0.2.0 binary opened a schema-13 archive read-write without complaint, then
// deleted 35,090 turns with the key it still believed in. Open is where every
// read-write path in ccvault enters the archive, so it is where the refusal has
// to land.
func TestOpen_RefusesAnArchiveWrittenByANewerBinary(t *testing.T) {
	dataDir, future := archiveFromTheFuture(t)

	database, err := Open(dataDir)
	if err == nil {
		_ = database.Close()
		t.Fatal("Open accepted an archive written by a newer ccvault")
	}

	var tooNew *SchemaTooNewError
	if !errors.As(err, &tooNew) {
		t.Fatalf("Open error is %v, want a *SchemaTooNewError", err)
	}
	if tooNew.ArchiveVersion != future {
		t.Errorf("error reports archive version %d, want %d", tooNew.ArchiveVersion, future)
	}
	if !errors.Is(err, ErrSchemaTooNew) {
		t.Error("errors.Is(err, ErrSchemaTooNew) is false, so callers cannot match the refusal")
	}

	// The message has to tell the user what to do about it. The whole cost of
	// this bug was that nothing said anything at all.
	msg := err.Error()
	for _, want := range []string{strconv.Itoa(future), "upgrade"} {
		if !strings.Contains(strings.ToLower(msg), strings.ToLower(want)) {
			t.Errorf("refusal does not mention %q: %s", want, msg)
		}
	}
}

// TestOpen_RefusalLeavesTheArchiveUntouched proves the guard fires before
// anything writes. A refusal that had already run a migration, or bumped a
// pragma, would be a refusal after the damage.
func TestOpen_RefusalLeavesTheArchiveUntouched(t *testing.T) {
	dataDir, _ := archiveFromTheFuture(t)
	path := filepath.Join(dataDir, dbFileName)

	before, err := fileDigest(path)
	if err != nil {
		t.Fatalf("read archive before: %v", err)
	}

	if database, err := Open(dataDir); err == nil {
		_ = database.Close()
		t.Fatal("Open accepted an archive written by a newer ccvault")
	}

	after, err := fileDigest(path)
	if err != nil {
		t.Fatalf("read archive after: %v", err)
	}
	if before != after {
		t.Error("the refused Open changed the archive file")
	}
}

// TestOpen_RefusalDoesNotRewriteTheJournalMode. The archive's own DSN sets
// journal_mode(WAL), and setting the journal mode of a database not already in
// it rewrites the file header — so a guard that ran after the connection was
// established would be the very thing writing to a file it is about to refuse.
func TestOpen_RefusalDoesNotRewriteTheJournalMode(t *testing.T) {
	dataDir, _ := archiveFromTheFuture(t)
	path := filepath.Join(dataDir, dbFileName)

	// Put the archive back into rollback-journal mode, the state a WAL-mode
	// fixture would hide.
	pool, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open archive directly: %v", err)
	}
	var mode string
	if err := pool.QueryRow("PRAGMA journal_mode=DELETE").Scan(&mode); err != nil {
		t.Fatalf("set rollback-journal mode: %v", err)
	}
	if err := pool.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if strings.EqualFold(mode, "wal") {
		t.Fatalf("archive is still in WAL mode (%q), so this test proves nothing", mode)
	}

	before, err := fileDigest(path)
	if err != nil {
		t.Fatalf("read archive before: %v", err)
	}
	if database, err := Open(dataDir); err == nil {
		_ = database.Close()
		t.Fatal("Open accepted an archive written by a newer ccvault")
	}
	after, err := fileDigest(path)
	if err != nil {
		t.Fatalf("read archive after: %v", err)
	}
	if before != after {
		t.Error("the refused Open rewrote the archive's header")
	}
}

// TestOpenForRead_AllowsANewerArchiveAndSaysSo. Refusing a read is more
// disruptive than useful — a newer schema is normally still readable — but the
// user should know the tool is behind the archive it is reporting on.
func TestOpenForRead_AllowsANewerArchiveAndSaysSo(t *testing.T) {
	dataDir, future := archiveFromTheFuture(t)

	database, err := OpenForRead(dataDir)
	if err != nil {
		t.Fatalf("OpenForRead refused a newer archive: %v", err)
	}
	defer func() { _ = database.Close() }()

	warning := database.SchemaWarning()
	if warning == "" {
		t.Fatal("OpenForRead opened a newer archive with no warning at all")
	}
	if !strings.Contains(warning, strconv.Itoa(future)) {
		t.Errorf("warning does not name the archive's schema version %d: %s", future, warning)
	}

	// Still a usable handle: the point is that reads keep working.
	var n int
	if err := database.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&n); err != nil {
		t.Errorf("read from a newer archive: %v", err)
	}
}

// TestOpenForRead_IsSilentOnAnArchiveItUnderstands keeps the warning rare
// enough to mean something.
func TestOpenForRead_IsSilentOnAnArchiveItUnderstands(t *testing.T) {
	database, err := OpenForRead(t.TempDir())
	if err != nil {
		t.Fatalf("OpenForRead: %v", err)
	}
	defer func() { _ = database.Close() }()

	if w := database.SchemaWarning(); w != "" {
		t.Errorf("SchemaWarning() = %q on a current archive, want empty", w)
	}
}

// TestRunMigrations_RefusesANewerSchema is the half of the guard that cannot be
// walked around. Open is a convenience; RunMigrations is what every path that
// establishes or advances the schema must go through, so a future caller that
// opens its own *sql.DB and migrates it still meets the refusal.
func TestRunMigrations_RefusesANewerSchema(t *testing.T) {
	dataDir, _ := archiveFromTheFuture(t)

	pool, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, dbFileName))
	if err != nil {
		t.Fatalf("open archive directly: %v", err)
	}
	defer func() { _ = pool.Close() }()

	err = RunMigrations(pool)
	if err == nil {
		t.Fatal("RunMigrations accepted an archive written by a newer ccvault")
	}
	if !errors.Is(err, ErrSchemaTooNew) {
		t.Errorf("RunMigrations error is %v, want ErrSchemaTooNew", err)
	}
}

// TestCompact_RefusesAnArchiveWrittenByANewerBinary. Compaction deliberately
// opens the file without running migrations, so it does not inherit the guard
// from Open — and it is the one operation whose bad outcome cannot be undone by
// re-running it, because it swaps and deletes.
func TestCompact_RefusesAnArchiveWrittenByANewerBinary(t *testing.T) {
	dataDir, _ := archiveFromTheFuture(t)

	if _, err := Compact(dataDir); err == nil {
		t.Fatal("Compact accepted an archive written by a newer ccvault")
	} else if !errors.Is(err, ErrSchemaTooNew) {
		t.Errorf("Compact error is %v, want ErrSchemaTooNew", err)
	}
}
