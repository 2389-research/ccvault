// ABOUTME: Benchmarks the per-page reads behind get_turns, session-wide against page-bounded (#141, #157).
// ABOUTME: Exists so the claims in those fixes are measured numbers rather than arguments about indexes.

package db

import (
	"fmt"
	"testing"
	"time"

	"github.com/2389-research/ccvault/pkg/models"
)

// buildToolHeavySession seeds one session with turns assistant turns and
// calls tool_uses rows spread evenly over them, which is the shape that makes
// the session-wide read expensive: many rows attached to many turns, of which
// a page wants fifty.
func buildToolHeavySession(tb testing.TB, database *DB, sessionID string, turns, calls int) []string {
	tb.Helper()

	p := &models.Project{Path: "/bench/proj", DisplayName: "bench"}
	if err := database.UpsertProject(p); err != nil {
		tb.Fatalf("upsert project: %v", err)
	}
	if err := database.UpsertSession(&models.Session{
		ID:         sessionID,
		ProjectID:  p.ID,
		StartedAt:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		SourceFile: "/tmp/" + sessionID + ".jsonl",
	}); err != nil {
		tb.Fatalf("upsert session: %v", err)
	}

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ids := make([]string, turns)
	batch := make([]models.Turn, 0, 2000)
	for i := 0; i < turns; i++ {
		ids[i] = fmt.Sprintf("%s-turn-%06d", sessionID, i)
		batch = append(batch, models.Turn{
			ID:        ids[i],
			SessionID: sessionID,
			Type:      "assistant",
			Timestamp: base.Add(time.Duration(i) * time.Second),
			Ordinal:   i,
			Content:   "assistant",
			RawJSON:   []byte(`{"type":"assistant"}`),
		})
		if len(batch) == cap(batch) {
			if err := database.InsertTurns(batch); err != nil {
				tb.Fatalf("insert turns: %v", err)
			}
			batch = batch[:0]
		}
	}
	if len(batch) > 0 {
		if err := database.InsertTurns(batch); err != nil {
			tb.Fatalf("insert turns: %v", err)
		}
	}

	// InsertToolUsesTx numbers a turn's calls by their position in the batch
	// and replaces whatever already holds that position, so a turn whose
	// calls straddle two batches overwrites its own rows. Each turn's calls
	// go in whole, and a batch only flushes on a turn boundary.
	uses := make([]models.ToolUse, 0, 4096)
	for i := 0; i < turns; i++ {
		perTurn := calls / turns
		if i < calls%turns {
			perTurn++
		}
		for c := 0; c < perTurn; c++ {
			uses = append(uses, models.ToolUse{
				TurnID:    ids[i],
				SessionID: sessionID,
				ToolName:  "mcp__nanoclaw__ssh_localhost",
				Timestamp: base.Add(time.Duration(i) * time.Second),
			})
		}
		if len(uses) >= 4096 {
			if err := database.InsertToolUses(uses); err != nil {
				tb.Fatalf("insert tool uses: %v", err)
			}
			uses = uses[:0]
		}
	}
	if len(uses) > 0 {
		if err := database.InsertToolUses(uses); err != nil {
			tb.Fatalf("insert tool uses: %v", err)
		}
	}

	var stored int
	if err := database.QueryRow(
		`SELECT count(*) FROM tool_uses WHERE session_id = ?`, sessionID).Scan(&stored); err != nil {
		tb.Fatalf("count stored calls: %v", err)
	}
	if stored != calls {
		tb.Fatalf("fixture stored %d calls, wanted %d: the numbering collapsed", stored, calls)
	}

	return ids
}

// benchSessions are the two scales worth knowing about: the largest real
// session in the author's archive, and the scale #141 described (whose
// 133,039 turned out to be the archive-wide count of one tool name rather
// than one session's — the defect is the same shape either way).
var benchSessions = []struct {
	name  string
	turns int
	calls int
}{
	{"archive_worst_7034turns_1871calls", 7034, 1871},
	{"issue_scale_7034turns_133039calls", 7034, 133039},
}

func BenchmarkToolNameReadForOnePage(b *testing.B) {
	for _, sc := range benchSessions {
		database, err := Open(b.TempDir())
		if err != nil {
			b.Fatalf("open db: %v", err)
		}
		ids := buildToolHeavySession(b, database, "bench-session", sc.turns, sc.calls)
		page := ids[:50]

		b.Run("session_wide/"+sc.name, func(b *testing.B) {
			rows := 0
			for i := 0; i < b.N; i++ {
				byTurn, err := database.SessionToolNamesByTurn("bench-session")
				if err != nil {
					b.Fatal(err)
				}
				rows = countNames(byTurn)
			}
			b.ReportMetric(float64(rows), "rows/op")
		})
		b.Run("page_bounded/"+sc.name, func(b *testing.B) {
			rows := 0
			for i := 0; i < b.N; i++ {
				byTurn, err := database.ToolNamesForTurns("bench-session", page)
				if err != nil {
					b.Fatal(err)
				}
				rows = countNames(byTurn)
			}
			b.ReportMetric(float64(rows), "rows/op")
		})

		_ = database.Close()
	}
}

func countNames(byTurn map[string][]string) int {
	n := 0
	for _, names := range byTurn {
		n += len(names)
	}
	return n
}

// TestMeasureToolNameRead reports the rows each form reads for one 50-turn
// page. Run with -v when the numbers in #141's PR need restating.
func TestMeasureToolNameRead(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a 133,039-row session")
	}
	for _, sc := range benchSessions {
		t.Run(sc.name, func(t *testing.T) {
			database, err := Open(t.TempDir())
			if err != nil {
				t.Fatalf("open db: %v", err)
			}
			defer func() { _ = database.Close() }()

			ids := buildToolHeavySession(t, database, "bench-session", sc.turns, sc.calls)
			page := ids[:50]

			start := time.Now()
			wide, err := database.SessionToolNamesByTurn("bench-session")
			if err != nil {
				t.Fatal(err)
			}
			wideElapsed := time.Since(start)

			start = time.Now()
			scoped, err := database.ToolNamesForTurns("bench-session", page)
			if err != nil {
				t.Fatal(err)
			}
			scopedElapsed := time.Since(start)

			t.Logf("session-wide: %d rows, %d turns, %s", countNames(wide), len(wide), wideElapsed)
			t.Logf("page-bounded: %d rows, %d turns, %s", countNames(scoped), len(scoped), scopedElapsed)
		})
	}
}
