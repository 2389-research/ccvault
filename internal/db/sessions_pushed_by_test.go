// ABOUTME: Verifies pushed_by round-trips through UpsertSession / GetSession
// ABOUTME: Exercises the column added by migration 006 for remote-vault attribution

package db

import (
	"testing"
	"time"

	"github.com/2389-research/ccvault/pkg/models"
)

func TestSessionPushedByRoundTrip(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	p := &models.Project{
		Path:           "/Users/test/project",
		DisplayName:    "test/project",
		FirstSeenAt:    time.Now(),
		LastActivityAt: time.Now(),
	}
	if err := db.UpsertProject(p); err != nil {
		t.Fatalf("upsert project: %v", err)
	}

	s := &models.Session{
		ID:         "sess-pushed-by",
		ProjectID:  p.ID,
		StartedAt:  time.Now(),
		EndedAt:    time.Now().Add(time.Hour),
		SourceFile: "/test/session.jsonl",
		Source:     "claude-code",
		PushedBy:   "alice@laptop",
	}
	if err := db.UpsertSession(s); err != nil {
		t.Fatalf("upsert session: %v", err)
	}

	got, err := db.GetSession(s.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got == nil {
		t.Fatal("session not found")
	}
	if got.PushedBy != "alice@laptop" {
		t.Errorf("pushed_by = %q, want alice@laptop", got.PushedBy)
	}
}

func TestSessionPushedByDefaultsEmpty(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	p := &models.Project{
		Path:           "/Users/test/project",
		DisplayName:    "test/project",
		FirstSeenAt:    time.Now(),
		LastActivityAt: time.Now(),
	}
	if err := db.UpsertProject(p); err != nil {
		t.Fatalf("upsert project: %v", err)
	}

	// PushedBy left as zero value — local-vault case
	s := &models.Session{
		ID:         "sess-local",
		ProjectID:  p.ID,
		StartedAt:  time.Now(),
		SourceFile: "/test/local.jsonl",
	}
	if err := db.UpsertSession(s); err != nil {
		t.Fatalf("upsert session: %v", err)
	}

	got, err := db.GetSession(s.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got == nil {
		t.Fatal("session not found")
	}
	if got.PushedBy != "" {
		t.Errorf("pushed_by = %q, want empty string", got.PushedBy)
	}
}
