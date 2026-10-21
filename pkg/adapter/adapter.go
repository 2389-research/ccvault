// ABOUTME: Defines the SourceAdapter interface and shared types for parsing conversation sessions.
// ABOUTME: All source-specific adapters (Claude Code, Cursor, etc.) implement SourceAdapter.

package adapter

import (
	"encoding/json"
	"io"
	"time"
)

// SessionFile represents a discovered session file on disk.
type SessionFile struct {
	Path        string
	ProjectPath string
	ModTime     time.Time
}

// ParsedSession holds the fully parsed contents of a single conversation session.
type ParsedSession struct {
	ID          string
	ProjectPath string
	DisplayName string // Human-readable project name for display in the UI

	// Turns must be in the order the source recorded them — file order for a
	// JSONL transcript. That slice order *is* the session's turn order: sync
	// derives each turn's ordinal from the index, so an adapter that returns
	// turns sorted any other way (by timestamp, say) renumbers the
	// conversation rather than merely presenting it differently. There is no
	// position field to carry instead, deliberately: one source of order
	// cannot disagree with itself.
	Turns      []ParsedTurn
	Model      string
	GitBranch  string
	StartedAt  time.Time
	EndedAt    time.Time
	SourceName string
	Metadata   map[string]any
}

// ParsedTurn represents one turn (message) within a session.
type ParsedTurn struct {
	ID           string
	ParentID     string
	Type         string // "user", "assistant", "system"
	Timestamp    time.Time
	Content      string
	RawJSON      json.RawMessage
	InputTokens  int64
	OutputTokens int64

	// CacheReadTokens and CacheWriteTokens are prompt-cache counters, summed
	// into sessions.cache_read_tokens / cache_write_tokens by the sync layer
	// the same way the input/output pair is. See models.Turn for why they are
	// not persisted per turn.
	//
	// The four counters must be disjoint, because the session row adds them
	// up. Sources do not agree on that: Claude Code and nanoclaw report
	// cache reads beside input_tokens, while codex's cached_input_tokens is
	// contained in its input_tokens — so the codex adapter subtracts the
	// cached part out before filling these, rather than reporting the source's
	// number verbatim and double-counting it. A source that records no cache
	// figures at all (hex, jeff — neither transcript format carries token
	// usage of any kind) leaves both zero.
	CacheReadTokens  int64
	CacheWriteTokens int64

	ToolUses []ParsedToolUse
	HasError bool
}

// ParsedToolUse captures a single tool invocation within a turn, including the
// payloads it carried. See models.ToolUse for what each field means and
// pkg/toolpayload for the policy that decides whether a result's content is
// stored or reduced to its length.
//
// A source that records none of a field leaves it zero — hex has no tool calls
// at all, and jeff records no usable id for 85% of its requests.
type ParsedToolUse struct {
	ToolName string
	FilePath string

	ToolUseID   string
	InputJSON   string
	InputLength int

	HasResult           bool
	ResultContent       string
	ResultLength        int
	ResultOmittedReason string

	// IsError is the tri-state failure flag — see models.ToolUse.IsError. A
	// source whose transcripts carry no error flag leaves it nil, which is why
	// it is not folded into HasResult: codex and jeff record a result and say
	// nothing about whether it failed, and false would assert success they
	// never claimed.
	IsError *bool
}

// SourceAdapter is the interface that all conversation source backends must implement.
// Discover finds session files under a root directory; Parse reads one file into a ParsedSession.
type SourceAdapter interface {
	Name() string
	Discover(root string) ([]SessionFile, error)
	Parse(path string) (*ParsedSession, error)
}

// TranscriptReparser is a SourceAdapter that can read a transcript from
// somewhere other than its original file.
//
// It exists because the source files are gone. Only a few hundred of the
// author's 49,226 archived transcripts still exist in ~/.claude, so repairing
// a counter that was never stored (#155's cache pair) cannot re-read them —
// but turns.raw_json holds the complete JSONL line of every turn, so the lines
// themselves survive inside the archive.
//
// ParseTranscript is Parse with the bytes supplied by the caller: Parse opens
// the file and hands the reader straight to it, so there is exactly one
// implementation of each source's rules and nothing can drift between a live
// sync and a re-derivation. That matters most where the rules disagree —
// codex's cached_input_tokens is contained in its input_tokens while Claude
// Code's cache counters sit beside them (see ParsedTurn.CacheReadTokens) — and
// a second implementation of that difference, in SQL or anywhere else, is two
// copies of one rule waiting to diverge.
//
// originalPath is the path the transcript came from. Adapters read the layout
// of a path for things the bytes do not carry (which session is a sidechain,
// which nanoclaw group a session belongs to), so passing the stored
// sessions.source_file keeps those answers the same as the first parse gave.
// It is not opened.
//
// Not every adapter implements it. hex stores a whole session as one JSON
// document rather than a line per turn, so its turns cannot be reassembled
// into a transcript; jeff records no token usage of any kind, so there is
// nothing about it for a re-derivation to recover. Which adapter is on which
// side of that line is pinned by TestTranscriptReparsers, so a new source that
// records usage cannot be left silently unrepairable.
type TranscriptReparser interface {
	SourceAdapter
	ParseTranscript(originalPath string, r io.Reader) (*ParsedSession, error)
}

// TokenTotals is a session's four token counters, summed over its turns.
type TokenTotals struct {
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
}

// Total is what the session cost altogether.
//
// The four counters are disjoint by contract — see ParsedTurn.CacheReadTokens
// for the per-source work that makes them so — which is what lets them be
// added rather than reconciled.
func (t TokenTotals) Total() int64 {
	return t.Input + t.Output + t.CacheRead + t.CacheWrite
}

// TokenTotals sums the session's turns into the four counters the session row
// carries.
//
// One implementation, used by both callers: the sync layer filling a session
// it just parsed, and the backfill re-deriving one from the lines the archive
// already holds. A second summation is how one of them ends up adding three
// counters out of four, which is #155 itself.
func (ps *ParsedSession) TokenTotals() TokenTotals {
	var totals TokenTotals
	for _, turn := range ps.Turns {
		totals.Input += turn.InputTokens
		totals.Output += turn.OutputTokens
		totals.CacheRead += turn.CacheReadTokens
		totals.CacheWrite += turn.CacheWriteTokens
	}
	return totals
}
