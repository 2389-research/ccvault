// ABOUTME: Tests the list-sessions subagent flags and the SUBS column rendering.
// ABOUTME: The default hides subagent rows; the count is what keeps that honest.

package main

import (
	"testing"

	"github.com/2389-research/ccvault/internal/db"
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

func TestFormatSubagentCount(t *testing.T) {
	cases := map[int]string{0: "-", 1: "1", 72: "72"}
	for in, want := range cases {
		if got := formatSubagentCount(in); got != want {
			t.Errorf("formatSubagentCount(%d) = %q, want %q", in, got, want)
		}
	}
}
