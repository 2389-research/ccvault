// ABOUTME: Tests the list-sessions subagent flags and the SUBS column rendering.
// ABOUTME: The default hides subagent rows; the count is what keeps that honest.

package main

import (
	"strings"
	"testing"
	"time"

	"github.com/2389-research/ccvault/internal/db"
	"github.com/2389-research/ccvault/pkg/models"
)

func TestSubagentScopeFromFlags(t *testing.T) {
	cases := []struct {
		name      string
		include   bool
		of        string
		wantScope db.SubagentScope
		wantID    string
	}{
		{name: "default hides them", wantScope: db.SubagentsHidden},
		{name: "include flattens", include: true, wantScope: db.SubagentsIncluded},
		{name: "subagents-of narrows", of: "parent-a", wantScope: db.SubagentsOf, wantID: "parent-a"},
		{
			// Asking for one parent's children is more specific than asking
			// for everything, so it wins.
			name:      "subagents-of wins over include",
			include:   true,
			of:        "parent-a",
			wantScope: db.SubagentsOf,
			wantID:    "parent-a",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scope, id := subagentScopeFromFlags(tc.include, tc.of)
			if scope != tc.wantScope {
				t.Errorf("scope = %v, want %v", scope, tc.wantScope)
			}
			if id != tc.wantID {
				t.Errorf("parent id = %q, want %q", id, tc.wantID)
			}
		})
	}
}

// The SUBS cell's rendering is tested in internal/compact, which owns the
// single spelling shared by this table and the TUI session list.

const (
	testParentID   = "04fb5717-c508-4503-ac85-dc11787cafaa"
	testSubagentID = "claude-code:04fb5717-c508-4503-ac85-dc11787cafaa:agent-a97db8caa54fd6f76"
)

// TestSessionIDColumnWidth: the column is sized from the ids actually being
// rendered. A composite subagent id is 72 characters against a uuid's 36, and
// a fixed %-38s pads without truncating — so one subagent row used to shove
// every later column 34 places right while the header stayed put.
func TestSessionIDColumnWidth(t *testing.T) {
	cases := []struct {
		name string
		ids  []string
		want int
	}{
		{name: "no rows keeps the default", want: sessionsIDMinWidth},
		{name: "uuids only keep the default", ids: []string{testParentID}, want: sessionsIDMinWidth},
		{
			// 72 + 2 of gutter, the same slack a 36-char uuid gets at 38.
			name: "one composite id widens the column",
			ids:  []string{testSubagentID},
			want: len(testSubagentID) + 2,
		},
		{
			name: "mixed widths size to the longest",
			ids:  []string{testParentID, testSubagentID},
			want: len(testSubagentID) + 2,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sessions := make([]models.Session, len(tc.ids))
			for i, id := range tc.ids {
				sessions[i] = models.Session{ID: id}
			}
			if got := sessionIDColumnWidth(sessions); got != tc.want {
				t.Errorf("sessionIDColumnWidth = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestRenderSessionsTableAligns is the real guard: the header has to keep
// meaning something. It checks that the first column after SESSION ID, and
// the last column, begin at the same offset on the header line and on every
// row — including a mixed list where a 36-char uuid and a 72-char composite
// id share the table — and that the separator spans the whole table.
func TestRenderSessionsTableAligns(t *testing.T) {
	started := time.Date(2026, 10, 2, 21, 58, 0, 0, time.UTC)
	sessions := []models.Session{
		{ID: testParentID, ProjectPath: "/Users/x/work/gitwatcher", StartedAt: started, TurnCount: 227, Model: "claude-opus-5", SubagentCount: 12},
		{ID: testSubagentID, ProjectPath: "/Users/x/work/gitwatcher", StartedAt: started, TurnCount: 568, Model: "claude-opus-5", ParentSessionID: testParentID},
	}

	for _, showProject := range []bool{true, false} {
		name := "with PROJECT"
		if !showProject {
			name = "without PROJECT"
		}
		t.Run(name, func(t *testing.T) {
			out := renderSessionsTable(sessions, showProject, nil)
			lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
			if len(lines) != 4 {
				t.Fatalf("got %d lines, want header + separator + 2 rows:\n%s", len(lines), out)
			}
			header, separator, rows := lines[0], lines[1], lines[2:]

			idWidth := sessionIDColumnWidth(sessions)
			if idWidth <= sessionsIDMinWidth {
				t.Fatalf("fixture should widen the id column, got %d", idWidth)
			}

			// Every id must be rendered whole — it is what the user copies
			// into `show` / `export`.
			for i, s := range sessions {
				if !strings.HasPrefix(rows[i], s.ID) {
					t.Errorf("row %d does not start with the full id %q:\n%s", i, s.ID, rows[i])
				}
			}

			// The cell immediately after SESSION ID occupies the same slice
			// on every line, header included. Sliced rather than
			// prefix-matched because PROJECT is left-aligned and STARTED is
			// right-aligned — the column's extent is what has to agree.
			secondHeader, secondWidth := "STARTED", sessionsStartedWidth
			if showProject {
				secondHeader, secondWidth = "PROJECT", sessionsProjectWidth
			}
			cell := func(line string) string {
				if len(line) < idWidth+1+secondWidth {
					t.Fatalf("line is too short to hold the second column:\n%s", line)
				}
				return strings.TrimSpace(line[idWidth+1 : idWidth+1+secondWidth])
			}
			if got := cell(header); got != secondHeader {
				t.Errorf("header's second column is %q, want %q:\n%s", got, secondHeader, header)
			}
			wantSecond := "2026-10-02 21:58"
			if showProject {
				wantSecond = "gitwatcher"
			}
			for i, row := range rows {
				if got := cell(row); got != wantSecond {
					t.Errorf("row %d's second column is %q, want %q:\n%s\n%s", i, got, wantSecond, header, row)
				}
			}

			// MODEL is last, so a matching offset there means no column in
			// between drifted.
			modelOffset := strings.Index(header, "MODEL")
			for i, row := range rows {
				if got := strings.Index(row, "claude-opus-5"); got != modelOffset {
					t.Errorf("row %d's MODEL starts at %d, header's at %d:\n%s\n%s",
						i, got, modelOffset, header, row)
				}
			}

			if len(separator) != sessionsTableWidth(idWidth, showProject) {
				t.Errorf("separator is %d chars, table is %d", len(separator), sessionsTableWidth(idWidth, showProject))
			}
			if len(separator) < modelOffset+len("claude-opus-5") {
				t.Errorf("separator (%d) is shorter than the widest row content (%d)",
					len(separator), modelOffset+len("claude-opus-5"))
			}
		})
	}
}

// TestRenderSessionsTableDefaultWidthUnchanged pins that a uuid-only listing
// renders exactly as it did before the column became dynamic.
func TestRenderSessionsTableDefaultWidthUnchanged(t *testing.T) {
	sessions := []models.Session{{
		ID:          testParentID,
		ProjectPath: "/Users/x/work/gitwatcher",
		StartedAt:   time.Date(2026, 10, 2, 21, 58, 0, 0, time.UTC),
		TurnCount:   227,
		Model:       "claude-opus-5",
	}}

	out := renderSessionsTable(sessions, true, nil)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if got := strings.Index(lines[0], "PROJECT"); got != sessionsIDMinWidth+1 {
		t.Errorf("PROJECT starts at %d, want %d (the historical layout)", got, sessionsIDMinWidth+1)
	}
	if !strings.HasPrefix(lines[2], testParentID+"   ") {
		t.Errorf("uuid row lost its historical 3-space gutter:\n%s", lines[2])
	}
}
