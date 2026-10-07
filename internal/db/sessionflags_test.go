// ABOUTME: Tests that session read paths return the stored has_error and has_subagent flags
// ABOUTME: Covers the two columns the listing SELECT never asked for (issue #73)

package db

import (
	"testing"
	"time"

	"github.com/2389-research/ccvault/pkg/models"
)

// TestSessionReadPathsReturnStoredFlags is #73: has_error and has_subagent
// were written correctly and never selected, so every session read back with
// the Go zero value and `list-sessions --json` reported `false` for all of
// them. A field whose presence implies it was computed is worse than no field
// at all — a script filtering on has_error found nothing, ever, and could not
// tell that from an archive with no errors in it.
func TestSessionReadPathsReturnStoredFlags(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	p := &models.Project{Path: "/test/flags", DisplayName: "flags"}
	if err := db.UpsertProject(p); err != nil {
		t.Fatalf("upsert project: %v", err)
	}

	// All four combinations, so a read path that returned a constant — or that
	// swapped the two columns, which a positional scan makes easy — is caught
	// rather than half-passing.
	want := []struct {
		id          string
		hasError    bool
		hasSubagent bool
	}{
		{"flags-neither", false, false},
		{"flags-error", true, false},
		{"flags-subagent", false, true},
		{"flags-both", true, true},
	}

	for i, w := range want {
		s := &models.Session{
			ID:          w.id,
			ProjectID:   p.ID,
			StartedAt:   time.Now().Add(time.Duration(-i) * time.Hour),
			SourceFile:  "/test/flags/" + w.id + ".jsonl",
			HasError:    w.hasError,
			HasSubagent: w.hasSubagent,
		}
		if err := db.UpsertSession(s); err != nil {
			t.Fatalf("upsert session %s: %v", w.id, err)
		}
	}

	page, err := db.GetSessionsPage(p.ID, 0, 0)
	if err != nil {
		t.Fatalf("GetSessionsPage: %v", err)
	}
	if len(page) != len(want) {
		t.Fatalf("GetSessionsPage returned %d sessions, want %d", len(page), len(want))
	}
	listed := map[string]models.Session{}
	for _, s := range page {
		listed[s.ID] = s
	}

	for _, w := range want {
		got, ok := listed[w.id]
		if !ok {
			t.Errorf("GetSessionsPage: session %s missing", w.id)
			continue
		}
		if got.HasError != w.hasError {
			t.Errorf("GetSessionsPage %s: HasError = %v, want %v", w.id, got.HasError, w.hasError)
		}
		if got.HasSubagent != w.hasSubagent {
			t.Errorf("GetSessionsPage %s: HasSubagent = %v, want %v", w.id, got.HasSubagent, w.hasSubagent)
		}

		// `show-session --json` serialises GetSession's result straight out of
		// models.Session, so the same omission reached the same consumers by a
		// second route.
		single, err := db.GetSession(w.id)
		if err != nil {
			t.Fatalf("GetSession %s: %v", w.id, err)
		}
		if single == nil {
			t.Errorf("GetSession %s: no row", w.id)
			continue
		}
		if single.HasError != w.hasError {
			t.Errorf("GetSession %s: HasError = %v, want %v", w.id, single.HasError, w.hasError)
		}
		if single.HasSubagent != w.hasSubagent {
			t.Errorf("GetSession %s: HasSubagent = %v, want %v", w.id, single.HasSubagent, w.hasSubagent)
		}
	}
}

// The two columns are nullable with a DEFAULT 0, which applies to an omitted
// column and not to an explicit NULL. Selecting them for the first time is
// also the first chance to scan a NULL out of them, so they get #64's
// treatment in the same change rather than becoming the next instance of it.
func TestSessionFlagsTolerateNull(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	if _, err := db.Exec(
		`INSERT INTO sessions (id, project_id, started_at, source_file, source,
			has_error, has_subagent)
		 VALUES ('null-flags', NULL, ?, ?, 'claude-code', NULL, NULL)`,
		time.Now(), "/test/null-flags.jsonl",
	); err != nil {
		t.Fatalf("insert session with NULL flags: %v", err)
	}

	page, err := db.GetSessionsPage(0, 0, 0)
	if err != nil {
		t.Fatalf("GetSessionsPage with NULL flags: %v", err)
	}
	if len(page) != 1 {
		t.Fatalf("GetSessionsPage returned %d sessions, want 1", len(page))
	}
	if page[0].HasError || page[0].HasSubagent {
		t.Errorf("NULL flags read as (%v, %v), want (false, false)", page[0].HasError, page[0].HasSubagent)
	}

	s, err := db.GetSession("null-flags")
	if err != nil {
		t.Fatalf("GetSession with NULL flags: %v", err)
	}
	if s == nil {
		t.Fatal("GetSession returned no row")
	}
	if s.HasError || s.HasSubagent {
		t.Errorf("NULL flags read as (%v, %v), want (false, false)", s.HasError, s.HasSubagent)
	}
}

// has_subagent and subagent_count are not the same question, which is why #73
// keeps both rather than dropping the flag now that the count exists.
//
// has_subagent says this transcript dispatched work — the adapters set it from
// a Task tool use in the session's own turns. subagent_count says how many
// session rows in *this archive* name this one as their parent. They come
// apart in both directions, and on the author's 46,061-session archive they
// do: 9 sessions are flagged with no child rows (the subagent transcript was
// never ingested, or predates migration 007's linking), and 9 have child rows
// without the flag (the parent transcript was pruned, or the dispatch was not
// a Task call the adapter recognised).
//
// So `subagent_count > 0` is not a drop-in for the flag: it would stop
// reporting a dispatch whose transcript the archive does not hold, which is
// precisely the case a reader most needs told.
func TestHasSubagentAndSubagentCountAreIndependent(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	p := &models.Project{Path: "/test/indep", DisplayName: "indep"}
	if err := db.UpsertProject(p); err != nil {
		t.Fatalf("upsert project: %v", err)
	}

	// Flagged, but its subagent's transcript is not in the archive.
	flaggedOnly := &models.Session{
		ID: "dispatched-elsewhere", ProjectID: p.ID, StartedAt: time.Now(),
		SourceFile: "/test/indep/a.jsonl", HasSubagent: true,
	}
	if err := db.UpsertSession(flaggedOnly); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Not flagged, but a child row names it as parent.
	countedOnly := &models.Session{
		ID: "parent-unflagged", ProjectID: p.ID, StartedAt: time.Now(),
		SourceFile: "/test/indep/b.jsonl",
	}
	if err := db.UpsertSession(countedOnly); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	child := &models.Session{
		ID: "parent-unflagged:child", ProjectID: p.ID, StartedAt: time.Now(),
		SourceFile: "/test/indep/b-child.jsonl", ParentSessionID: "parent-unflagged",
	}
	if err := db.UpsertSession(child); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	flagged, err := db.GetSession("dispatched-elsewhere")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if !flagged.HasSubagent {
		t.Error("dispatched-elsewhere: HasSubagent = false, want true")
	}
	if flagged.SubagentCount != 0 {
		t.Errorf("dispatched-elsewhere: SubagentCount = %d, want 0", flagged.SubagentCount)
	}

	counted, err := db.GetSession("parent-unflagged")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if counted.HasSubagent {
		t.Error("parent-unflagged: HasSubagent = true, want false")
	}
	if counted.SubagentCount != 1 {
		t.Errorf("parent-unflagged: SubagentCount = %d, want 1", counted.SubagentCount)
	}
}
