// ABOUTME: Tests the parent-vs-subagent split over the Parquet cache (#150).
// ABOUTME: The split is about per-session shape, so that is what the assertions are about.

package analytics

import (
	"testing"
	"time"

	"github.com/2389-research/ccvault/internal/db"
)

// agentRoleFixture builds a cache with deliberately different per-session
// shapes and a deliberately small subagent share, because that is what the
// real archive looks like: measured on the author's 47,409-session archive on
// 2026-10-13, subagents are 362 sessions (0.8%) and 11,513,958 of 145,661,685
// tokens (7.9%), while averaging 128.9 turns and 31,807 tokens a session
// against a top-level session's 21.5 and 2,851.
//
// Four top-level sessions of 10 turns / 1,000 tokens and one subagent of 60
// turns / 6,000 tokens reproduces that in miniature — a minority of sessions
// whose averages are six times larger.
func agentRoleFixture(t *testing.T) *Analyzer {
	t.Helper()

	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	now := time.Now().UTC()
	res, err := database.Exec(`INSERT INTO projects
		(path, display_name, first_seen_at, last_activity_at, session_count, total_tokens, source)
		VALUES ('/tmp/roles', 'roles', ?, ?, 0, 0, 'claude-code')`, now, now)
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	projectID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("project id: %v", err)
	}

	insert := func(id, parentID string, turns int, inputTok, outputTok int64) {
		if _, err := database.Exec(`INSERT INTO sessions
			(id, project_id, model, git_branch, started_at, ended_at, turn_count,
			 input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
			 source_file, source, parent_session_id)
			VALUES (?, ?, 'claude-opus-4', 'main', ?, ?, ?, ?, ?, 0, 0, ?, 'claude-code', ?)`,
			id, projectID, now, now, turns, inputTok, outputTok,
			"/tmp/roles/"+id+".jsonl", parentID); err != nil {
			t.Fatalf("insert session %s: %v", id, err)
		}
	}
	insert("parent-a", "", 10, 600, 400)
	insert("parent-b", "", 10, 600, 400)
	insert("parent-c", "", 10, 600, 400)
	insert("parent-d", "", 10, 600, 400)
	insert("claude-code:parent-a:agent-a1", "parent-a", 60, 4000, 2000)

	cacheDir := t.TempDir()
	if err := NewExporter(database, cacheDir).Export(); err != nil {
		t.Fatalf("export: %v", err)
	}
	analyzer, err := NewAnalyzer(cacheDir)
	if err != nil {
		t.Fatalf("new analyzer: %v", err)
	}
	t.Cleanup(func() { _ = analyzer.Close() })
	return analyzer
}

func TestGetStatsByAgentRole_SeparatesThePopulations(t *testing.T) {
	analyzer := agentRoleFixture(t)

	stats, err := analyzer.GetStatsByAgentRole()
	if err != nil {
		t.Fatalf("stats by agent role: %v", err)
	}

	byRole := map[string]AgentRoleStats{}
	for _, s := range stats {
		byRole[s.Role] = s
	}
	if len(byRole) != 2 {
		t.Fatalf("got roles %v, want exactly top-level and subagent", byRole)
	}

	top := byRole[AgentRoleTopLevel]
	if top.SessionCount != 4 || top.TotalTurns != 40 || top.TotalTokens != 4000 {
		t.Errorf("top-level = %+v, want 4 sessions / 40 turns / 4000 tokens", top)
	}

	sub := byRole[AgentRoleSubagent]
	if sub.SessionCount != 1 || sub.TotalTurns != 60 || sub.TotalTokens != 6000 {
		t.Errorf("subagent = %+v, want 1 session / 60 turns / 6000 tokens", sub)
	}
}

// TestAgentRoleStats_AveragesAreTheSignal: the averages are the reason the
// split exists, so they are derived from the counts rather than recomputed by
// each caller.
func TestAgentRoleStats_AveragesAreTheSignal(t *testing.T) {
	analyzer := agentRoleFixture(t)

	stats, err := analyzer.GetStatsByAgentRole()
	if err != nil {
		t.Fatalf("stats by agent role: %v", err)
	}
	byRole := map[string]AgentRoleStats{}
	for _, s := range stats {
		byRole[s.Role] = s
	}

	if got := byRole[AgentRoleTopLevel].AvgTurnsPerSession(); got != 10 {
		t.Errorf("top-level avg turns = %v, want 10", got)
	}
	if got := byRole[AgentRoleTopLevel].AvgTokensPerSession(); got != 1000 {
		t.Errorf("top-level avg tokens = %v, want 1000", got)
	}
	if got := byRole[AgentRoleSubagent].AvgTurnsPerSession(); got != 60 {
		t.Errorf("subagent avg turns = %v, want 60", got)
	}
	if got := byRole[AgentRoleSubagent].AvgTokensPerSession(); got != 6000 {
		t.Errorf("subagent avg tokens = %v, want 6000", got)
	}

	// The blended average is the number this split exists to stop anyone
	// reporting: 2,000 tokens a session describes neither population.
	var sessions, tokens int64
	for _, s := range stats {
		sessions += int64(s.SessionCount)
		tokens += s.TotalTokens
	}
	if blended := float64(tokens) / float64(sessions); blended != 2000 {
		t.Fatalf("fixture blended average = %v, want 2000 — the fixture no longer "+
			"demonstrates the problem", blended)
	}
}

// TestAgentRoleStats_ZeroSessionsDoesNotDivideByZero guards the empty-archive
// render path: a role with no sessions must report 0, not NaN.
func TestAgentRoleStats_ZeroSessionsDoesNotDivideByZero(t *testing.T) {
	empty := AgentRoleStats{Role: AgentRoleSubagent}
	if got := empty.AvgTurnsPerSession(); got != 0 {
		t.Errorf("avg turns on an empty role = %v, want 0", got)
	}
	if got := empty.AvgTokensPerSession(); got != 0 {
		t.Errorf("avg tokens on an empty role = %v, want 0", got)
	}
}

// TestGetStatsByAgentRole_RefusesAPreParentCache is the deliberate choice not
// to write this query defensively. A COALESCE around a column the file does
// not have would report every session as top-level — a wrong answer that
// looks like a right one. Erroring is what lets the TUI's staleness check
// (#149) be the thing that fixes it.
func TestGetStatsByAgentRole_RefusesAPreParentCache(t *testing.T) {
	cacheDir := t.TempDir()
	writePreParentParquet(t, cacheDir+"/sessions.parquet")

	analyzer, err := NewAnalyzer(cacheDir)
	if err != nil {
		t.Fatalf("new analyzer: %v", err)
	}
	t.Cleanup(func() { _ = analyzer.Close() })

	if _, err := analyzer.GetStatsByAgentRole(); err == nil {
		t.Fatal("a cache without parent_session_id returned stats instead of an error")
	}
}
