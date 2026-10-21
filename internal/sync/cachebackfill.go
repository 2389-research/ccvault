// ABOUTME: Re-derives the prompt-cache token counters of already-ingested sessions from the lines the archive stored.
// ABOUTME: Feeds turns.raw_json back through the adapter that first read it, so the per-source token rule has one implementation.

package sync

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/2389-research/ccvault/internal/db"
	"github.com/2389-research/ccvault/pkg/adapter"
)

// cacheBackfillBatch is how many repaired sessions are written per
// transaction. Small enough that an interrupted run keeps nearly all of its
// progress, large enough that 48,000 sessions are not 48,000 commits.
const cacheBackfillBatch = 500

// cacheBackfillCommit bounds each commit, which runs on a context the run's
// cancellation cannot reach. Long enough to outwait a sync holding the writer
// (busyTimeoutMS is 5s), short enough that Ctrl-C still ends the command
// promptly.
const cacheBackfillCommit = 30 * time.Second

// maxReportedBackfillErrors caps the errors carried back from a run. A
// systematic fault would otherwise return one error per session and bury the
// counts underneath them; the count of failures is kept whole regardless.
const maxReportedBackfillErrors = 10

// CacheBackfillOptions configures a cache-token backfill run.
type CacheBackfillOptions struct {
	// DryRun examines every candidate and writes nothing. The counts it
	// reports are the counts a real run would produce.
	DryRun bool

	// Progress, if set, is called with a line of progress every few hundred
	// sessions. Reading 4 GB of stored lines takes minutes, and a command that
	// says nothing for minutes looks hung.
	Progress func(format string, args ...any)

	// BatchSize overrides how many repairs are committed at once. Zero means
	// cacheBackfillBatch.
	BatchSize int

	// afterSession is called once per examined candidate, with how many have
	// been examined so far. A test seam, unexported so only this package can
	// set one: it is how a test cancels a run mid-pass, which is the only way
	// to reach the commit of a partly-filled batch. Same reason
	// parseSessionReaderWithLimits takes its limits as arguments.
	afterSession func(examined int)
}

// CacheBackfillReport is what one run did.
//
// Every candidate lands in exactly one of Repaired, NoCacheUsage, Unreadable,
// Mismatched, Unsupported or Failed, which is what makes the report readable
// as an account of the whole archive rather than a success count. Two fields
// qualify those buckets rather than joining them: Refused is a subtraction
// from Repaired, for a session the archive declined to write after the run had
// decided to repair it, and StrippedPayloads is a subset of Repaired whose
// totals carry a caveat.
type CacheBackfillReport struct {
	// Candidates is how many sessions still read zero on both cache columns.
	Candidates int

	// Repaired is how many sessions were given back the cache tokens their
	// stored transcript records. On a dry run, how many would have been.
	Repaired int

	// NoCacheUsage is sessions whose stored transcript is complete and
	// readable and records no cache tokens. Their zero is the right answer,
	// which is the reading a bare "still zero" count cannot distinguish.
	NoCacheUsage int

	// Unreadable is sessions holding a turn with no stored line at all, so
	// nothing can be re-derived from them (#101).
	Unreadable int

	// Mismatched is sessions whose re-derived input and output tokens do not
	// equal the input and output the row already counts, so the stored lines
	// do not account for what the session is known to have cost. Refused
	// rather than written — see the guard in BackfillCacheTokens.
	//
	// Broken down by source because the causes are not alike and one number
	// cannot say which applies. A claude-code session here is usually missing
	// turns its row still counts, which is the drift `stats` reports
	// separately; every legacy codex session is here by construction, because
	// codex records its token counts on event lines that never became turns.
	Mismatched         int
	MismatchedBySource map[string]int

	// StrippedPayloads is how many of the repaired sessions hold a line
	// ccvault replaced with a placeholder because the original exceeded the
	// raw_json cap. They are counted inside Repaired, not instead of it, and
	// reported because their totals are the only ones carrying a caveat — see
	// db.StoredTranscript.StrippedLines.
	StrippedPayloads int

	// Unsupported is sessions from a source that cannot be re-derived at all,
	// counted per source so the reason is nameable rather than a mystery.
	Unsupported         int
	UnsupportedBySource map[string]int

	// Failed is sessions whose re-derivation errored. Errors holds the first
	// few verbatim.
	Failed int
	Errors []error

	// Refused is sessions the run derived a repair for and the archive then
	// declined to write, because the row had stopped matching what the repair
	// was derived against — another run, or a sync that re-parsed it. They
	// are counted here and taken back out of Repaired, so the headline figure
	// is what the archive accepted rather than what this run intended.
	Refused int

	// CacheTokensRecovered is the cache total the repaired sessions were
	// understating the archive by.
	CacheTokensRecovered int64

	// Examined is how many candidates the run got through. Below Candidates
	// when the run was cancelled.
	Examined int

	// Cancelled records that the context ended the run early. The sessions
	// already committed stay repaired; the next run picks up the rest.
	Cancelled bool

	// DryRun mirrors the option, so a report cannot be read as a record of
	// writes that never happened.
	DryRun bool

	Elapsed time.Duration
}

// BackfillCacheTokens repairs sessions.cache_read_tokens and
// cache_write_tokens for sessions ingested before anything assigned them.
//
// This is the second half of #155. The first half fixed the sync path, so new
// sessions carry the counters; every session already in the archive still
// reads zero, and on the author's archive that is 48,064 of 49,226 sessions
// understating the token total by roughly two orders of magnitude.
//
// It cannot be a re-parse from disk: a few hundred of those transcripts still
// exist under ~/.claude and the rest are gone, so the trick migration 012 uses
// — forget the source file's mtime and let the next sync re-read it — has
// nothing to re-read. It cannot be a SQL migration either, because extracting
// the counters in SQL means restating the per-source rule, and that rule is
// not uniform: codex's cached_input_tokens is contained in its input_tokens
// while Claude Code's cache counters sit beside them. Two statements of one
// rule is #143, and the SQL copy is the one that would have inflated every
// codex session.
//
// So the route is the archive's own stored lines. turns.raw_json holds the
// complete JSONL line of every turn, so the lines are streamed back through
// the adapter that originally read them and summed by the same
// ParsedSession.TokenTotals the sync path uses. No second implementation of
// anything: the only new rule is when to refuse a result.
//
// Three refusals, and they are what make the run trustworthy:
//
//   - a session holding a turn with no stored line is skipped, not errored
//     over. 42 sessions are in that state on purpose (#101) and a run of
//     48,000 cannot stop for them.
//   - a session whose re-derived input and output tokens do not match the ones
//     the row already counts is refused. That equality is the proof the stored
//     lines are the whole session: input and output were counted correctly
//     from the start, so re-deriving them is a checksum over the bytes the
//     repair is about to trust. It is what catches a turn whose oversized line
//     was stored as a placeholder that dropped its usage object, and it is
//     what catches codex, whose token counts arrive on event lines that never
//     became turns — so a legacy codex session re-derives to zero of
//     everything, and writing that would wipe the input tokens the row had.
//     A derivation *above* the stored figures is refused too: that is the row
//     and the lines disagreeing, which is a different defect and not this
//     command's to decide.
//   - a session whose source has no reparser is counted and named rather than
//     silently left out.
//
// One thing it deliberately does not refuse: a session holding a line ccvault
// replaced with a placeholder because the original was too large to store.
// Those are repaired and counted separately as StrippedPayloads, because
// refusing them leaves a whole session at zero to avoid being short by one
// turn — and zero is the wrong number this command exists to remove. The
// measurement behind that is at the branch itself.
//
// Re-running is safe and resumes: the candidate condition is the cache pair
// still being zero, and the UPDATE repeats it along with the counted pair the
// repair was derived against.
func BackfillCacheTokens(ctx context.Context, database *db.DB, opts CacheBackfillOptions) (CacheBackfillReport, error) {
	started := time.Now()
	report := CacheBackfillReport{
		DryRun:              opts.DryRun,
		UnsupportedBySource: make(map[string]int),
		MismatchedBySource:  make(map[string]int),
	}

	batchSize := opts.BatchSize
	if batchSize <= 0 {
		batchSize = cacheBackfillBatch
	}

	candidates, err := database.CacheBackfillCandidates()
	if err != nil {
		return report, err
	}
	report.Candidates = len(candidates)
	progress(opts, "Examining %d session(s) whose cache token counters read zero", len(candidates))

	// One adapter instance per source, reused across sessions. The adapters are
	// stateless; constructing one per session would just be 48,000 allocations.
	reparsers := make(map[string]adapter.TranscriptReparser)

	pending := make([]db.SessionTokenCounters, 0, batchSize)
	cacheTokensOf := func(sessionID string) int64 {
		for _, c := range pending {
			if c.SessionID == sessionID {
				return c.CacheRead + c.CacheWrite
			}
		}
		return 0
	}
	// Every commit runs on a context the run's cancellation cannot reach.
	// Those repairs are already derived — reading 4 GB of stored lines is the
	// expensive half, not the UPDATE — so a cancel arriving mid-commit should
	// not throw a batch of them away and turn Ctrl-C into a failed command.
	// Bounded, so a cancelled run cannot hang on a busy writer either.
	flush := func() error {
		if opts.DryRun || len(pending) == 0 {
			pending = pending[:0]
			return nil
		}
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cacheBackfillCommit)
		defer cancel()

		_, refused, err := database.WriteSessionTokenCounters(writeCtx, pending)
		if err != nil {
			return err
		}
		// A row the archive declined comes back out of the counts. The
		// candidate list was read before the parse, so a row can be repaired
		// by another run — or by a sync that re-read its transcript — in
		// between, and reporting it as repaired here would be this run taking
		// credit for a number it did not write.
		for _, sessionID := range refused {
			report.Refused++
			report.Repaired--
			report.CacheTokensRecovered -= cacheTokensOf(sessionID)
			report.Errors = appendCapped(report.Errors, fmt.Errorf(
				"session %s was no longer the row this repair was derived from, so it was left alone "+
					"(another run, or a sync that re-read its transcript?)", sessionID))
		}
		pending = pending[:0]
		return nil
	}

	for i, candidate := range candidates {
		if ctx.Err() != nil {
			report.Cancelled = true
			break
		}
		report.Examined++

		reparser, ok := reparsers[candidate.Source]
		if !ok {
			reparser = lookupReparser(candidate.Source)
			reparsers[candidate.Source] = reparser
		}
		if reparser == nil {
			report.Unsupported++
			report.UnsupportedBySource[candidate.Source]++
			continue
		}

		totals, stripped, err := rederiveTokenTotals(database, reparser, candidate)
		switch {
		case errors.Is(err, db.ErrNoStoredLine):
			report.Unreadable++
			continue
		case err != nil:
			report.Failed++
			report.Errors = appendCapped(report.Errors,
				fmt.Errorf("re-derive session %s: %w", candidate.ID, err))
			continue
		}

		// Column by column rather than on the sum of the two: a sum agrees
		// with itself when input and output have swapped places, and writing
		// the swap back is not something this command should be able to do.
		if totals.Input != candidate.StoredInput || totals.Output != candidate.StoredOutput {
			report.Mismatched++
			report.MismatchedBySource[candidate.Source]++
			continue
		}
		if totals.CacheRead == 0 && totals.CacheWrite == 0 {
			report.NoCacheUsage++
			continue
		}

		report.Repaired++
		report.CacheTokensRecovered += totals.CacheRead + totals.CacheWrite
		// Repaired, and flagged. A placeholder line carries no usage, so if
		// the turn behind it had any, this total is short by it — but the
		// checksum just proved those turns reported no input and no output,
		// which for a Claude Code transcript means either no usage object at
		// all (an oversized *user* message, which is what the cap exists for)
		// or a fully-cached prompt. Measured on the author's archive: 7
		// placeholder turns across 6 sessions out of 13,375 turns, holding
		// 1.29 billion cache tokens between them. Refusing those sessions
		// would leave 100% of that uncounted to avoid being short by at most
		// seven turns — the zero is the worse wrong number, and it is the one
		// indistinguishable from "spent no cache tokens".
		if stripped > 0 {
			report.StrippedPayloads++
		}
		pending = append(pending, db.SessionTokenCounters{
			SessionID:  candidate.ID,
			Input:      totals.Input,
			Output:     totals.Output,
			CacheRead:  totals.CacheRead,
			CacheWrite: totals.CacheWrite,
		})

		if len(pending) >= batchSize {
			if err := flush(); err != nil {
				report.Elapsed = time.Since(started)
				return report, err
			}
		}

		if (i+1)%2000 == 0 {
			progress(opts, "  %d/%d examined, %d repaired, %d recovered so far",
				i+1, len(candidates), report.Repaired, report.CacheTokensRecovered)
		}
		if opts.afterSession != nil {
			opts.afterSession(report.Examined)
		}
	}

	// The last batch is committed even when the run was cancelled — see flush.
	if err := flush(); err != nil {
		report.Elapsed = time.Since(started)
		return report, err
	}

	// projects.total_tokens is a denormalized sum of all four session
	// counters, so leaving it alone would mean `list-projects`, the TUI and
	// every per-project figure still reported the understated total the repair
	// just corrected.
	if report.Repaired > 0 && !opts.DryRun {
		if err := database.ReconcileProjectAggregates(nil); err != nil {
			report.Elapsed = time.Since(started)
			return report, fmt.Errorf("reconcile project aggregates: %w", err)
		}
	}

	report.Elapsed = time.Since(started)
	return report, nil
}

// rederiveTokenTotals streams one session's stored lines through its adapter
// and sums the result the way the sync path sums a live parse.
//
// The stored source_file path is handed back to the adapter unopened. It is
// usually a path to a file that no longer exists, and that is fine: adapters
// read the *shape* of a path for things the lines do not carry — whether a
// transcript is a sidechain, which nanoclaw group it belongs to — and those
// answers have to come out the same as they did the first time or the
// re-derivation is of some other session.
// The cursor is closed before this returns, and so before the caller writes
// anything: the connection pool is one handle wide, so a write issued while a
// transcript is still open would wait on a connection the read is holding.
// The second return value is how many of the lines read were ccvault's
// placeholder for an oversized original, which the caller refuses on.
func rederiveTokenTotals(database *db.DB, reparser adapter.TranscriptReparser, candidate db.CacheBackfillCandidate) (adapter.TokenTotals, int, error) {
	transcript, err := database.StoredTranscript(candidate.ID)
	if err != nil {
		return adapter.TokenTotals{}, 0, err
	}
	defer func() { _ = transcript.Close() }()

	parsed, err := reparser.ParseTranscript(candidate.SourceFile, transcript)
	if err != nil {
		return adapter.TokenTotals{}, transcript.StrippedLines(), err
	}
	return parsed.TokenTotals(), transcript.StrippedLines(), nil
}

// lookupReparser returns the adapter for a source if it can re-derive a
// session from stored lines, and nil otherwise.
//
// nil rather than an error: a source with no reparser is a reportable fact
// about that source (jeff records no token usage at all; hex does not store a
// line per turn), not a fault in the run.
func lookupReparser(source string) adapter.TranscriptReparser {
	adpt, err := adapter.Get(source)
	if err != nil {
		return nil
	}
	reparser, ok := adpt.(adapter.TranscriptReparser)
	if !ok {
		return nil
	}
	return reparser
}

// appendCapped adds an error to a slice that has stopped growing at
// maxReportedBackfillErrors.
func appendCapped(errs []error, err error) []error {
	if len(errs) >= maxReportedBackfillErrors {
		return errs
	}
	return append(errs, err)
}

// progress reports a line if the caller asked for progress.
func progress(opts CacheBackfillOptions, format string, args ...any) {
	if opts.Progress == nil {
		return
	}
	opts.Progress(format, args...)
}
