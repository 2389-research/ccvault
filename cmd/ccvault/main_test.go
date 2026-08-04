// ABOUTME: Tests for CLI helpers in package main
// ABOUTME: Verifies orient gathers database state and reports failures as warnings

package main

import (
	"testing"

	"github.com/2389-research/ccvault/internal/db"
)

func TestGatherOrientation_HealthyDBHasNoWarnings(t *testing.T) {
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = database.Close() }()

	o := gatherOrientation(database)
	if len(o.Warnings) != 0 {
		t.Errorf("healthy db should produce no warnings, got %v", o.Warnings)
	}
}

func TestGatherOrientation_CollectsWarningsOnFailure(t *testing.T) {
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	_ = database.Close() // force every stats query to fail

	o := gatherOrientation(database)
	if len(o.Warnings) != 6 {
		t.Errorf("closed db should produce 6 warnings (one per query), got %d: %v", len(o.Warnings), o.Warnings)
	}
}
