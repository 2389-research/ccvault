// ABOUTME: Refuses to write an archive whose schema a newer ccvault wrote
// ABOUTME: The guard that would have stopped the incident behind issue #105

package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
)

// ErrSchemaTooNew matches every refusal this guard produces, so callers can
// recognise it without depending on the message.
var ErrSchemaTooNew = errors.New("archive schema is newer than this ccvault understands")

// SchemaTooNewError reports that the archive carries a migration this binary
// has never heard of.
//
// What it cost to find out. On 2026-10-13 an archive migrated to schema 13 was
// opened read-write by a v0.2.0 binary still first on the author's PATH. That
// binary predates two things the archive now depends on: the (session_id, id)
// turn key of migration 012, and the connection pragmas of #47. It keyed turns
// on the uuid alone, so writing one session deleted another's rows — 35,090
// turns — and it ran without recursive_triggers, so the implicit DELETE of its
// INSERT OR REPLACE never fired the cleanup trigger and left 136,279 stranded
// documents in turns_fts. Nothing warned. The archive knows its own
// schema_version and the binary knows the highest migration it ships; the
// mismatch was detectable at the moment the file was opened.
type SchemaTooNewError struct {
	// ArchiveVersion is the highest migration recorded in the archive.
	ArchiveVersion int
	// KnownVersion is the highest migration this binary ships.
	KnownVersion int
}

func (e *SchemaTooNewError) Error() string {
	return fmt.Sprintf(
		"this archive is at schema version %d and this ccvault only knows up to %d, "+
			"so it was written by a newer ccvault than the one you are running. "+
			"Writing to it with this binary can delete data it does not understand. "+
			"Upgrade ccvault (or point this one at a different archive with --data-dir)",
		e.ArchiveVersion, e.KnownVersion)
}

// Is makes errors.Is(err, ErrSchemaTooNew) true for every refusal.
func (e *SchemaTooNewError) Is(target error) bool {
	return target == ErrSchemaTooNew
}

// Warning is the same fact phrased for a read, which is allowed to proceed.
// A newer schema is usually still readable, and blocking a read is more
// disruptive than useful — but the user should know the tool is behind the
// archive it is reporting on, because that is the state that precedes someone
// running a write with the same binary.
func (e *SchemaTooNewError) Warning() string {
	return fmt.Sprintf(
		"warning: this archive is at schema version %d and this ccvault only knows up to %d. "+
			"Reading is allowed and what you see may be incomplete; writing to it with this "+
			"binary is refused. Upgrade ccvault.",
		e.ArchiveVersion, e.KnownVersion)
}

// maxKnownSchemaVersion resolves once: the migrations are embedded, so the
// answer cannot change during a run.
var maxKnownSchemaVersion = sync.OnceValues(func() (int, error) {
	migrations, err := loadMigrations()
	if err != nil {
		return 0, err
	}
	highest := 0
	for _, m := range migrations {
		if m.version > highest {
			highest = m.version
		}
	}
	return highest, nil
})

// MaxKnownSchemaVersion is the highest migration this binary ships. Derived
// from the embedded files rather than written down, so adding a migration
// cannot leave the guard refusing the archive its own binary just wrote.
func MaxKnownSchemaVersion() (int, error) {
	return maxKnownSchemaVersion()
}

// readSchemaVersion returns the highest migration recorded in the archive, and
// 0 for a database that has no schema_version table yet — a fresh file, or one
// predating the migration system, both of which RunMigrations handles.
func readSchemaVersion(ctx context.Context, q sqlRunner) (int, error) {
	var tables int
	err := q.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='schema_version'").Scan(&tables)
	if err != nil {
		return 0, fmt.Errorf("look for schema_version: %w", err)
	}
	if tables == 0 {
		return 0, nil
	}

	var version int
	if err := q.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_version").Scan(&version); err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return version, nil
}

// inspectSchemaVersion reads an archive's schema version over a connection
// that cannot write to it, and returns the refusal if it is ahead of this
// binary.
//
// A read-only handle rather than the archive's own, because the archive's own
// DSN sets journal_mode(WAL), and setting the journal mode of a database not
// already in it rewrites the file header. The guard exists to keep this binary
// from touching a file it does not understand, so it cannot be the thing that
// touches it.
//
// A file that is not there yet is not too new: mode=ro refuses to create one,
// and a fresh archive is what Open goes on to build.
func inspectSchemaVersion(dbPath string) (*SchemaTooNewError, error) {
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			return nil, nil //nolint:nilnil // no archive, so no refusal and no failure
		}
		return nil, fmt.Errorf("stat archive: %w", err)
	}

	pool, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("open archive to read its schema version: %w", err)
	}
	defer func() { _ = pool.Close() }()
	pool.SetMaxOpenConns(1)

	return schemaTooNew(context.Background(), pool)
}

// schemaTooNew returns the refusal if the archive is ahead of this binary, and
// nil otherwise.
func schemaTooNew(ctx context.Context, q sqlRunner) (*SchemaTooNewError, error) {
	known, err := MaxKnownSchemaVersion()
	if err != nil {
		return nil, fmt.Errorf("determine the highest migration this binary ships: %w", err)
	}
	archive, err := readSchemaVersion(ctx, q)
	if err != nil {
		return nil, err
	}
	if archive > known {
		return &SchemaTooNewError{ArchiveVersion: archive, KnownVersion: known}, nil
	}
	return nil, nil //nolint:nilnil // "no refusal, no failure" is the whole result
}

// refuseIfSchemaTooNew is schemaTooNew as a plain error, for the write paths.
func refuseIfSchemaTooNew(ctx context.Context, q sqlRunner) error {
	tooNew, err := schemaTooNew(ctx, q)
	if err != nil {
		return err
	}
	if tooNew != nil {
		return tooNew
	}
	return nil
}
