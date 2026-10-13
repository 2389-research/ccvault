// ABOUTME: Reports turns whose transcript timestamp an adapter's layout could not read, as session metadata.
// ABOUTME: The visibility half of #146 — the layout lists stay per-source, the counting does not.

package adapter

// UnreadableTimestampsKey and UnreadableTimestampSampleKey are the
// ParsedSession.Metadata keys the sync layer aggregates and reports. Named
// constants because the producer and the consumer sit in different packages
// and a typo in one of them is a diagnostic that silently reads clean.
const (
	UnreadableTimestampsKey      = "turns_with_unparsable_timestamp"
	UnreadableTimestampSampleKey = "unparsable_timestamp_sample"
)

// ReportUnreadableTimestamps counts the turns left at the zero time — the
// ones whose transcript timestamp matched none of the adapter's layouts — and
// records the count, plus the sample string the caller captured, in meta.
//
// Nothing is written when every turn parsed, so the key's presence is itself
// the signal and a clean archive stays quiet.
//
// Why a count rather than one shared layout list (#146): pkg/parser,
// pkg/adapter/codex and pkg/adapter/jeff each read a different tool's
// transcript format. Those producers change independently of each other and
// of us, and a single list would couple five of them together — besides
// pointing pkg/ at internal/db, the wrong direction for this tree. The thing
// the three lists actually lacked was not unification but a signal: a turn at
// the zero time sorts to the very start of the archive and falls outside
// every date filter, while being indistinguishable from a turn someone
// genuinely recorded at the epoch.
//
// The zero time is the marker because it is what every adapter here already
// produces for an unreadable value, and because no real transcript carries
// January 1 of year 1.
func ReportUnreadableTimestamps(meta map[string]any, turns []ParsedTurn, sample string) {
	unreadable := 0
	for _, turn := range turns {
		if turn.Timestamp.IsZero() {
			unreadable++
		}
	}
	if unreadable == 0 {
		return
	}
	meta[UnreadableTimestampsKey] = unreadable
	if sample != "" {
		meta[UnreadableTimestampSampleKey] = sample
	}
}
