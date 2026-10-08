// ABOUTME: Tests the stats output that makes invalid stored payloads visible (issue #101)
// ABOUTME: Covers the printed section, the --json shape, and the source-file split

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/2389-research/ccvault/internal/db"
)

// The raw-payload section is the only thing that makes a non-JSON raw_json
// visible — the rows are indexed, searchable and counted like any other — so
// what it prints is the feature.
func TestPrintRawJSONSection(t *testing.T) {
	tests := []struct {
		name          string
		integrity     db.RawJSONIntegrity
		withSource    int
		withoutSource int
		wantLines     []string
		omitLines     []string
	}{
		{
			name:      "a whole-archive scan that finds nothing says so plainly",
			integrity: db.RawJSONIntegrity{Turns: 4200, Scanned: 4200, Complete: true},
			wantLines: []string{"Raw payloads:", "all 4200 turns"},
			omitLines: []string{"Invalid JSON", "--check-raw-json", "sync --full"},
		},
		{
			name:      "a windowed scan names its window and the way to widen it",
			integrity: db.RawJSONIntegrity{Turns: 996680, Scanned: 100000},
			wantLines: []string{"newest 100000 of 996680 turns", "ccvault stats --check-raw-json"},
			omitLines: []string{"Invalid JSON"},
		},
		{
			name: "damage with a surviving transcript names the repair",
			integrity: db.RawJSONIntegrity{
				Turns: 4200, Scanned: 4200, Invalid: 68, Sessions: 2, Complete: true,
			},
			withSource: 2,
			wantLines:  []string{"Invalid JSON:", "68 of 4200 turns", "2 session(s)", "ccvault sync --full"},
			omitLines:  []string{"cannot be repaired"},
		},
		{
			name: "damage with no transcript left says it cannot be repaired",
			integrity: db.RawJSONIntegrity{
				Turns: 996680, Scanned: 996680, Invalid: 216978, Sessions: 42, Complete: true,
			},
			withoutSource: 42,
			wantLines:     []string{"216978 of 996680 turns", "42 session(s)", "cannot be repaired"},
			omitLines:     []string{"ccvault sync --full"},
		},
		{
			name: "a windowed scan that finds damage says the count is a window's worth",
			integrity: db.RawJSONIntegrity{
				Turns: 996680, Scanned: 100000, Invalid: 7, Sessions: 1,
			},
			withSource: 1,
			wantLines:  []string{"7 of the newest 100000 turns"},
		},
		{
			name:      "an empty archive prints no section at all",
			integrity: db.RawJSONIntegrity{Complete: true},
			omitLines: []string{"Raw payloads"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := captureStdout(t, func() {
				printRawJSONSection(tt.integrity, tt.withSource, tt.withoutSource)
			})
			for _, want := range tt.wantLines {
				if !strings.Contains(out, want) {
					t.Errorf("output does not contain %q:\n%s", want, out)
				}
			}
			for _, omit := range tt.omitLines {
				if strings.Contains(out, omit) {
					t.Errorf("output unexpectedly contains %q:\n%s", omit, out)
				}
			}
		})
	}
}

func TestRawJSONJSON_ReportsScopeAndVerdict(t *testing.T) {
	out := rawJSONJSON(db.RawJSONIntegrity{
		Turns: 996680, Scanned: 100000, Invalid: 7, Sessions: 1,
	}, 0, 1)

	wantInt := map[string]int64{
		"turns":    996680,
		"scanned":  100000,
		"invalid":  7,
		"sessions": 1,
	}
	for key, want := range wantInt {
		got, ok := out[key].(int64)
		if !ok {
			t.Errorf("%s = %#v, want an int64", key, out[key])
			continue
		}
		if got != want {
			t.Errorf("%s = %d, want %d", key, got, want)
		}
	}
	if got, ok := out["sessions_without_source"].(int); !ok || got != 1 {
		t.Errorf("sessions_without_source = %#v, want 1", out["sessions_without_source"])
	}
	// A windowed report is a lower bound, and a consumer that cannot tell the
	// difference would read 7 as the archive's total.
	if complete, ok := out["complete"].(bool); !ok || complete {
		t.Errorf("complete = %#v, want false for a windowed scan", out["complete"])
	}
	if consistent, ok := out["consistent"].(bool); !ok || consistent {
		t.Errorf("consistent = %#v, want false with 7 invalid payloads", out["consistent"])
	}

	clean := rawJSONJSON(db.RawJSONIntegrity{Turns: 4200, Scanned: 4200, Complete: true}, 0, 0)
	if consistent, ok := clean["consistent"].(bool); !ok || !consistent {
		t.Errorf("consistent = %#v for an archive with no damage, want true", clean["consistent"])
	}
}

// A path that exists but cannot be stat'd must not be reported as unrepairable:
// claiming a transcript is gone when it is only unreadable would talk someone
// out of a repair that would have worked.
func TestSessionSourceFileSplit(t *testing.T) {
	dir := t.TempDir()

	present := filepath.Join(dir, "present.jsonl")
	if err := os.WriteFile(present, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write fixture transcript: %v", err)
	}
	gone := filepath.Join(dir, "gone.jsonl")

	got, missing := sessionSourceFileSplit([]string{present, gone, present})
	if got != 2 || missing != 1 {
		t.Errorf("sessionSourceFileSplit() = %d present, %d missing; want 2, 1", got, missing)
	}

	if got, missing := sessionSourceFileSplit(nil); got != 0 || missing != 0 {
		t.Errorf("sessionSourceFileSplit(nil) = %d, %d; want 0, 0", got, missing)
	}
}
