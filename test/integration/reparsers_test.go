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
	for _, tc := range []struct {
		source      string
		canReparse  bool
		carriesCost bool
	}{
		{source: "claude-code", canReparse: true, carriesCost: true},
		{source: "nanoclaw", canReparse: true, carriesCost: true},
		{source: "codex", canReparse: true, carriesCost: true},
		{source: "jeff", canReparse: false, carriesCost: false},
		{source: "hex", canReparse: false, carriesCost: false},
	} {
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
