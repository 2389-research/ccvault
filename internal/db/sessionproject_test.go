// ABOUTME: Tests that upserting a session can move it to another project, and cannot orphan it onto no project.
// ABOUTME: Without the move, #156's repair re-parsed 42 transcripts and left every one of them where it was.

package db

import (
	"testing"
	"time"

	"github.com/2389-research/ccvault/pkg/models"
)

func TestUpsertSessionFollowsACorrectedProject(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	worktree := &models.Project{Path: "/repo/.claude/worktrees/agent-a1", DisplayName: "agent-a1", FirstSeenAt: now, LastActivityAt: now}
	repo := &models.Project{Path: "/repo", DisplayName: "repo", FirstSeenAt: now, LastActivityAt: now}
	for _, p := range []*models.Project{worktree, repo} {
		if err := database.UpsertProject(p); err != nil {
			t.Fatalf("UpsertProject %s: %v", p.Path, err)
		}
	}

	session := &models.Session{
		ID:         "claude-code:parent:agent-a1",
		ProjectID:  worktree.ID,
		SourceFile: "/h/agent-a1.jsonl",
		Source:     "claude-code",
		StartedAt:  now,
		EndedAt:    now,
	}
	if err := database.UpsertSession(session); err != nil {
		t.Fatalf("first UpsertSession: %v", err)
	}

	// The re-parse: same session, the project ccvault now says it belongs to.
	session.ProjectID = repo.ID
	if err := database.UpsertSession(session); err != nil {
		t.Fatalf("second UpsertSession: %v", err)
	}

	var got int64
	if err := database.QueryRow(`SELECT project_id FROM sessions WHERE id = ?`, session.ID).Scan(&got); err != nil {
		t.Fatalf("scan project_id: %v", err)
	}
	if got != repo.ID {
		t.Errorf("project_id = %d, want the repo's %d", got, repo.ID)
	}

	// An upsert that carries no project leaves the stored one alone, rather
	// than pointing the row at a project id that does not exist.
	session.ProjectID = 0
	if err := database.UpsertSession(session); err != nil {
		t.Fatalf("third UpsertSession: %v", err)
	}
	if err := database.QueryRow(`SELECT project_id FROM sessions WHERE id = ?`, session.ID).Scan(&got); err != nil {
		t.Fatalf("scan project_id: %v", err)
	}
	if got != repo.ID {
		t.Errorf("project_id = %d after a projectless upsert, want the stored %d", got, repo.ID)
	}
}
