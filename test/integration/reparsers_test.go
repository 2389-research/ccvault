// ABOUTME: Pins which registered adapters can re-derive a session from stored transcript lines.
// ABOUTME: A source that records token usage but cannot be reparsed is a session the #155 backfill can never repair.

package integration

import (
	"testing"

	"github.com/2389-research/ccvault/pkg/adapter"

	_ "github.com/2389-research/ccvault/pkg/adapter/claudecode"
	_ "github.com/2389-research/ccvault/pkg/adapter/codex"
	_ "github.com/2389-research/ccvault/pkg/adapter/hex"
	_ "github.com/2389-research/ccvault/pkg/adapter/jeff"
	_ "github.com/2389-research/ccvault/pkg/adapter/nanoclaw"
	_ "github.com/2389-research/ccvault/pkg/adapter/pi"
)

// TestTranscriptReparsers is the table adapter.TranscriptReparser's
// documentation refers to.
//
// The cache-token backfill can only re-derive a session whose adapter
// implements the interface, and it reports every other source as unsupported
// rather than guessing. That reporting is only honest while this list is
// right: a source that records token usage and does not implement the
// interface is a source whose sessions stay understated forever, and nothing
// else in the build would notice.
//
// The reason for each "no" is the reason it is allowed to stay a "no":
//
//   - jeff carries no token usage of any kind in its transcript format, so a
//     re-derivation would recover nothing. Confirmed against the structs and
//     against 3 real sessions, which contain no "token" substring (#155).
//   - hex stores a whole session as one JSON document and ccvault marshals
//     each message into raw_json separately, so the stored rows cannot be
//     reassembled into the transcript the adapter reads. It also records no
//     token usage, which is why this is a note rather than a defect.
//
// pi is the first source added after this table grew a completeness guard, and
// it is a "yes" on both counts: every pi assistant message carries its own
// usage block, and the spend pi records without a message — cache warming, a
// compaction's summarisation — is stored as a turn of its own rather than
// folded into the turn before it, specifically so those numbers are in
// raw_json where a re-derivation can find them. That is the codex trap below,
// avoided on purpose.
//
// What implementing the interface does NOT promise is that a stored session
// can be repaired, and codex is the standing proof. Its adapter reparses a
// transcript correctly — splitting cached_input_tokens out of input_tokens,
// which is the rule a SQL reimplementation would have got backwards — but it
// folds codex's token_count *event* lines into the preceding assistant turn
// instead of storing them as turns, so those counts were never in raw_json to
// begin with. Every legacy codex session therefore re-derives to zero and is
// refused by the backfill's checksum, permanently.
//
// Both halves of that are pinned elsewhere, and they are where to look if this
// comment ever stops being true: codex.TestParseTranscript_AppliesTheSubsetSplit
// for the adapter applying the split when the token_count lines are there, and
// sync.TestBackfillCacheTokens_RefusesALegacyCodexSession for a stored session
// without them being refused with its input_tokens intact. The per-source
// breakdown in the repair report is what tells a user which case their archive
// is in.
func TestTranscriptReparsers(t *testing.T) {
	cases := []struct {
		source      string
		canReparse  bool
		carriesCost bool
	}{
		{source: "claude-code", canReparse: true, carriesCost: true},
		{source: "nanoclaw", canReparse: true, carriesCost: true},
		{source: "codex", canReparse: true, carriesCost: true},
		{source: "jeff", canReparse: false, carriesCost: false},
		{source: "hex", canReparse: false, carriesCost: false},
		{source: "pi", canReparse: true, carriesCost: true},
	}

	// Every registered source has to be in the table, or the table says
	// nothing about the source that is missing from it. Being absent used to
	// mean being unchecked, which is the same silence the comment above warns
	// about one level up: a new source that records token usage and cannot be
	// reparsed is a source whose sessions are understated forever, and leaving
	// it off this list is how that would ship unnoticed.
	listed := make(map[string]bool, len(cases))
	for _, tc := range cases {
		listed[tc.source] = true
	}
	for _, name := range adapter.RegisteredNames() {
		if !listed[name] {
			t.Errorf("source %q is registered but not in this table, so nothing checks whether it can be reparsed", name)
		}
	}

	for _, tc := range cases {
		adpt, err := adapter.Get(tc.source)
		if err != nil {
			t.Fatalf("Get(%q): %v", tc.source, err)
		}
		_, ok := adpt.(adapter.TranscriptReparser)
		if ok != tc.canReparse {
			t.Errorf("%s implements TranscriptReparser = %v, want %v", tc.source, ok, tc.canReparse)
		}
		if tc.carriesCost && !ok {
			t.Errorf("%s records token usage but cannot be reparsed, so its sessions can never be repaired",
				tc.source)
		}
	}
}
