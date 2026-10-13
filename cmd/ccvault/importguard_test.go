// ABOUTME: Tests the CLI surface of the import source's schema-version guard
// ABOUTME: Covers the refusal, the binary-naming decoration, and the override flag

package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/2389-research/ccvault/internal/db"
)

// TestNameTheBinary_DecoratesTheImportSourceRefusalToo is the detail that made
// 2026-10-13's incident diagnosable: three ccvault installs were ahead of the
// repo on the author's PATH, and "which binary is this" is the one fact the
// error cannot work out for itself. #136's refusal is the same kind of refusal
// and needs the same decoration.
func TestNameTheBinary_DecoratesTheImportSourceRefusalToo(t *testing.T) {
	refusal := nameTheBinary(&db.MergeSourceTooNewError{
		Path:          "/backups/ccvault-20260905-140418.db",
		SourceVersion: 15,
		KnownVersion:  14,
	})

	if !errors.Is(refusal, db.ErrSchemaTooNew) {
		t.Errorf("decorating lost the error identity: %v", refusal)
	}
	if !strings.Contains(refusal.Error(), "running ccvault") {
		t.Errorf("the refusal does not name the running binary: %v", refusal)
	}
	if !strings.Contains(refusal.Error(), "/backups/ccvault-20260905-140418.db") {
		t.Errorf("the refusal does not name the source archive: %v", refusal)
	}
	if !strings.Contains(refusal.Error(), "--allow-newer-schema") {
		t.Errorf("the refusal does not name the override: %v", refusal)
	}
}

// TestImportCmd_HasTheOverrideFlag. The refusal names `--allow-newer-schema`,
// so the flag has to exist under that spelling — a message naming a flag the
// binary does not accept is worse than no message.
func TestImportCmd_HasTheOverrideFlag(t *testing.T) {
	flag := importCmd.Flags().Lookup("allow-newer-schema")
	if flag == nil {
		t.Fatal("import has no --allow-newer-schema flag, but the refusal tells users to pass one")
	}
	if flag.Value.Type() != "bool" {
		t.Errorf("--allow-newer-schema is a %s, want a bool", flag.Value.Type())
	}
	if flag.DefValue != "false" {
		t.Errorf("--allow-newer-schema defaults to %q; the guard has to be on by default", flag.DefValue)
	}
}
