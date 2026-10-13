// ABOUTME: Tests that a bare before:/after: date names the caller's local day, not a wall-clock day
// ABOUTME: Issue #90 — the predicate compared text and never reached the offset

package search

import (
	"testing"
	"time"

	"github.com/2389-research/ccvault/internal/db"
	"github.com/2389-research/ccvault/pkg/models"
)

// zoneFixture holds one turn, at one instant, so a filter either admits it or
// does not and there is nothing else for the result to be about.
//
// 02:00 UTC on 1 October is chosen because it falls on a different calendar
// day depending on which side of the dateline you read it from: 14:00 on
// 30 September at UTC-12, and 16:00 on the 1st at UTC+14.
var zoneFixtureInstant = time.Date(2026, 10, 1, 2, 0, 0, 0, time.UTC)

func setupZoneFixture(t *testing.T) *db.DB {
	t.Helper()

	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	p := &models.Project{Path: "/test/zone", DisplayName: "zone"}
	if err := database.UpsertProject(p); err != nil {
		t.Fatalf("upsert project: %v", err)
	}
	s := &models.Session{ID: "session-z", ProjectID: p.ID, StartedAt: zoneFixtureInstant, SourceFile: "/z.jsonl"}
	if err := database.UpsertSession(s); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	if err := database.InsertTurns([]models.Turn{{
		ID: "turn-z", SessionID: "session-z", Type: "assistant", Ordinal: 0,
		Timestamp: zoneFixtureInstant, Content: "zoneneedle at the dateline",
	}}); err != nil {
		t.Fatalf("insert turn: %v", err)
	}
	return database
}

// TestSearch_BareDateMeansTheCallersDay is issue #90.
//
// Before the fix the bound and the stored value were both rendered
// wall-clock-first, so `t.timestamp > $1` resolved on the digits and stopped
// before the offset: local midnight on 1 October renders as
// "2026-10-01 00:00:00" in every zone on earth, and the filter admitted the
// same rows at UTC-12 as at UTC+14. Measured in the issue, and reproduced
// here.
func TestSearch_BareDateMeansTheCallersDay(t *testing.T) {
	tests := []struct {
		name  string
		zone  *time.Location
		query string
		want  bool
	}{
		{
			// Local midnight on the 1st is 12:00 UTC on the 1st, which is
			// after the turn. Local time of the turn is 14:00 on 30 September.
			name:  "after: the caller's day has not begun yet",
			zone:  time.FixedZone("UTC-12", -12*60*60),
			query: "zoneneedle after:2026-10-01",
			want:  false,
		},
		{
			// Local midnight on the 1st is 10:00 UTC on 30 September, before
			// the turn. Local time of the turn is 16:00 on the 1st.
			name:  "after: the caller's day began yesterday in UTC",
			zone:  time.FixedZone("UTC+14", 14*60*60),
			query: "zoneneedle after:2026-10-01",
			want:  true,
		},
		{
			name:  "before: the turn is still in the caller's past",
			zone:  time.FixedZone("UTC-12", -12*60*60),
			query: "zoneneedle before:2026-10-01",
			want:  true,
		},
		{
			name:  "before: the turn has not happened in the caller's day yet",
			zone:  time.FixedZone("UTC+14", 14*60*60),
			query: "zoneneedle before:2026-10-01",
			want:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pinLocalZone(t, tc.zone)
			database := setupZoneFixture(t)
			searcher := New(database.DB)

			results, err := searcher.Search(Parse(tc.query), 50)
			if err != nil {
				t.Fatalf("search %q: %v", tc.query, err)
			}
			got := len(results) > 0
			if got != tc.want {
				t.Errorf("%q at %s returned %d rows (want match=%v); the turn is at %s, local %s",
					tc.query, tc.zone, len(results), tc.want,
					zoneFixtureInstant.Format(time.RFC3339),
					zoneFixtureInstant.In(tc.zone).Format(time.RFC3339))
			}
		})
	}
}

// TestSearch_BareDateAgreesWithoutTextToPrune is the same four cases with no
// free text, so no MATCH is built and nothing prunes.
//
// The period terms are allowed to over-admit — the outer WHERE trims them —
// but never to under-admit. Running the same window both ways and demanding
// the same answer is what would catch a token set that has drifted away from
// the predicate it is supposed to shadow.
func TestSearch_BareDateAgreesWithoutTextToPrune(t *testing.T) {
	for _, zone := range []*time.Location{
		time.FixedZone("UTC-12", -12*60*60),
		time.FixedZone("UTC+14", 14*60*60),
	} {
		for _, filter := range []string{"after:2026-10-01", "before:2026-10-01"} {
			t.Run(zone.String()+" "+filter, func(t *testing.T) {
				pinLocalZone(t, zone)
				database := setupZoneFixture(t)
				searcher := New(database.DB)

				withText, err := searcher.Search(Parse("zoneneedle "+filter), 50)
				if err != nil {
					t.Fatalf("search with text: %v", err)
				}
				withoutText, err := searcher.Search(Parse(filter), 50)
				if err != nil {
					t.Fatalf("search without text: %v", err)
				}
				if len(withText) != len(withoutText) {
					t.Errorf("the pruned search returned %d rows and the unpruned one %d — the period terms disagree with the predicate",
						len(withText), len(withoutText))
				}
			})
		}
	}
}

// TestSearch_RelativeTokensStayZoneAware guards what #89 fixed, against the
// change that makes bare dates zone-aware. today/yesterday are computed on the
// local calendar, and converting the bound to UTC must not undo that.
func TestSearch_RelativeTokensStayZoneAware(t *testing.T) {
	zone := time.FixedZone("UTC+14", 14*60*60)
	pinLocalZone(t, zone)

	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = database.Close() }()

	p := &models.Project{Path: "/test/relative", DisplayName: "relative"}
	if err := database.UpsertProject(p); err != nil {
		t.Fatalf("upsert project: %v", err)
	}
	s := &models.Session{ID: "session-r", ProjectID: p.ID, StartedAt: time.Now(), SourceFile: "/r.jsonl"}
	if err := database.UpsertSession(s); err != nil {
		t.Fatalf("upsert session: %v", err)
	}

	// An hour into the caller's today, and an hour before it began.
	now := time.Now().In(zone)
	y, m, d := now.Date()
	localMidnight := time.Date(y, m, d, 0, 0, 0, 0, zone)
	if err := database.InsertTurns([]models.Turn{
		{ID: "turn-today", SessionID: "session-r", Type: "assistant", Ordinal: 0,
			Timestamp: localMidnight.Add(time.Hour), Content: "relativeneedle today"},
		{ID: "turn-before", SessionID: "session-r", Type: "assistant", Ordinal: 1,
			Timestamp: localMidnight.Add(-time.Hour), Content: "relativeneedle yesterday"},
	}); err != nil {
		t.Fatalf("insert turns: %v", err)
	}

	results, err := New(database.DB).Search(Parse("relativeneedle after:today"), 50)
	if err != nil {
		t.Fatalf("search after:today: %v", err)
	}
	if len(results) != 1 || results[0].Turn.ID != "turn-today" {
		t.Errorf("after:today returned %v, want just turn-today", resultIDs(results))
	}
}
