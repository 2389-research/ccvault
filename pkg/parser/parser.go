// ABOUTME: JSONL parser for Claude Code conversation files
// ABOUTME: Converts raw JSONL entries into structured Turn objects

package parser

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/2389-research/ccvault/pkg/models"
	"github.com/2389-research/ccvault/pkg/toolpayload"
)

// ParseStats reports counts of anomalies the parser handled while reading a
// session. Populated even when Parse returns an error (partial progress may be
// observable to callers).
type ParseStats struct {
	// SkippedLines is the number of lines the parser could not use, either
	// because they failed to JSON-parse or (see T4) because they exceeded
	// the per-line size cap and were dropped defensively.
	SkippedLines int
	// TurnsWithTruncatedRawJSON is the number of turns whose raw_json was
	// replaced with a placeholder because the original line exceeded the
	// raw_json size threshold. See strippedRawJSON.
	TurnsWithTruncatedRawJSON int

	// TurnsWithUnparsableTimestamp is the number of turns whose transcript
	// timestamp matched none of parseTimestamp's layouts (or was absent) and
	// so carries the zero time rather than a recorded one.
	//
	// The turn is kept — dropping a conversation over a clock is the wrong
	// trade, and refusing the line would make a producer changing its
	// timestamp format cost the content of every session it wrote rather than
	// their metadata: a session with no turns gets no id either, since that
	// comes from turns[0]. But the substitution used to be completely silent,
	// which is #146, so the count is what makes it answerable.
	//
	// The zero time is the fallback and time.Now() was not (#165). A
	// present-day instant is indistinguishable from a recorded one on
	// inspection, answers every date predicate, and since #142 made
	// timestamps sort lexicographically as instants it sorts to the newest end
	// of the archive — the part people look at. The zero value is impossible
	// on its face, and it is what the codex and jeff adapters already produce
	// for the same failure, so one archive does not carry two markers for it.
	// Neither value belongs in the session's bounds; see updateSessionMetadata.
	//
	// Non-zero here means a producer's format moved and this list has not.
	// It is not consolidated with the codex and jeff lists on purpose: each
	// reads a different tool's transcript, those tools change independently,
	// and a shared list would couple them (and point pkg/ at internal/db).
	TurnsWithUnparsableTimestamp int

	// UnparsableTimestampSample is the first timestamp string that failed to
	// parse, verbatim, empty when none did. A count says something is
	// unreadable; the sample says which layout is missing.
	UnparsableTimestampSample string
}

// Package-level parser limits. Hard-coded on purpose; see the design doc at
// docs/plans/2026-08-04-issue-11-bounded-and-truncated.md for rationale.
const (
	// maxLineBytes is the hard cap on the size of a single JSONL line the
	// parser will accept. Above this the line is skipped and counted. Well
	// above real-world Claude Code PDF-page lines (~12 MB) but low enough
	// to bound memory on a pathological file.
	maxLineBytes = 200 * 1024 * 1024

	// maxRawJSONBytes is the threshold above which turn.RawJSON is replaced
	// with a small placeholder (see strippedRawJSON). Real conversation
	// turns are well under 1 MB; PDF pages and other embedded blobs are the
	// only realistic overflow.
	maxRawJSONBytes = 1 * 1024 * 1024
)

// ParseSession reads a session JSONL file and returns all turns.
//
// See ParseStats for the meaning of the third return value.
func ParseSession(path string) ([]models.Turn, *models.Session, ParseStats, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, ParseStats{}, fmt.Errorf("open session file: %w", err)
	}
	defer func() { _ = f.Close() }()

	return ParseSessionReader(f, path)
}

// ParseSessionReader parses a session from an io.Reader using the package
// default limits (maxLineBytes, maxRawJSONBytes). See parseSessionReaderWithLimits
// for the underlying primitive.
func ParseSessionReader(r io.Reader, sourcePath string) ([]models.Turn, *models.Session, ParseStats, error) {
	return parseSessionReaderWithLimits(r, sourcePath, maxLineBytes, maxRawJSONBytes)
}

// parseSessionReaderWithLimits is the injectable core so tests can exercise the
// cap/truncate paths without allocating hundreds of megabytes.
//
// Uses bufio.Reader.ReadSlice via readLineBounded rather than bufio.Scanner so
// per-line size is bounded but not fixed at 10 MB. Claude Code embeds
// base64-encoded PDF pages inside single JSONL lines (see issue #11) which
// routinely exceeds any small token cap and, with a Scanner, aborts the whole
// file. Lines above maxLine are skipped defensively; turns whose original raw
// line was above maxRaw get a placeholder in place of the full blob so
// turns.raw_json doesn't bloat the DB with content nothing indexes.
func parseSessionReaderWithLimits(r io.Reader, sourcePath string, maxLine, maxRaw int) ([]models.Turn, *models.Session, ParseStats, error) {
	var turns []models.Turn
	session := &models.Session{
		SourceFile: sourcePath,
	}
	stats := ParseStats{}

	reader := bufio.NewReader(r)

	for {
		line, oversized, readErr := readLineBounded(reader, maxLine)
		if oversized {
			stats.SkippedLines++
		} else if len(line) > 0 {
			turn, raw, tsOK, parseErr := parseTurnInternal(line)
			if parseErr != nil {
				stats.SkippedLines++
			} else if turn != nil {
				if !tsOK {
					stats.TurnsWithUnparsableTimestamp++
					if stats.UnparsableTimestampSample == "" && raw.Timestamp != "" {
						stats.UnparsableTimestampSample = raw.Timestamp
					}
				}
				if len(line) > maxRaw {
					turn.RawJSON = strippedRawJSON(raw, len(line))
					stats.TurnsWithTruncatedRawJSON++
				}
				// The turn's position in the session: the count of turns
				// already kept, so the sequence is 0-based and gapless over
				// every type, and skipped lines leave no hole. Assigned here
				// rather than from the raw line number because a line the
				// parser could not use is not a turn.
				turn.Ordinal = len(turns)
				turns = append(turns, *turn)
				updateSessionMetadata(session, turn, raw)
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return nil, nil, stats, fmt.Errorf("read session file: %w", readErr)
		}
	}

	// Set session ID from first turn if available
	if len(turns) > 0 {
		session.ID = turns[0].SessionID
	}

	// StartedAt and EndedAt are maintained as running extrema inside
	// updateSessionMetadata, not taken from the ends of the file. See the note
	// there for why.

	session.TurnCount = len(turns)

	return turns, session, stats, nil
}

// readLineBounded reads the next line from r, capped at maxBytes of content
// (post-strip of CR/LF). If the line exceeds maxBytes, the excess is drained
// up to the next '\n' and (nil, true, err) is returned; err carries io.EOF
// when the drain hits EOF, so callers can break out cleanly. Uses
// bufio.Reader.ReadSlice for chunk-by-chunk reads that keep memory bounded
// even for pathological input (a multi-GB single "line" won't blow the heap).
func readLineBounded(r *bufio.Reader, maxBytes int) ([]byte, bool, error) {
	var acc []byte
	for {
		chunk, err := r.ReadSlice('\n')
		switch {
		case err == nil:
			// chunk ends with '\n'. Strip \n and optional \r.
			line := chunk
			if len(line) > 0 && line[len(line)-1] == '\n' {
				line = line[:len(line)-1]
				if len(line) > 0 && line[len(line)-1] == '\r' {
					line = line[:len(line)-1]
				}
			}
			if len(acc)+len(line) > maxBytes {
				return nil, true, nil
			}
			if len(acc) == 0 {
				// Fast path: no accumulator yet. Copy out of r's internal
				// buffer since chunk is not stable across further reads.
				out := make([]byte, len(line))
				copy(out, line)
				return out, false, nil
			}
			acc = append(acc, line...)
			return acc, false, nil
		case errors.Is(err, bufio.ErrBufferFull):
			// chunk fills the internal buffer without hitting '\n'.
			if len(acc)+len(chunk) > maxBytes {
				// Would exceed cap. Drain the rest of the line to '\n'
				// (or EOF) and report the line as oversized.
				drainErr := drainToNewline(r)
				if drainErr != nil && !errors.Is(drainErr, io.EOF) {
					return nil, true, drainErr
				}
				return nil, true, drainErr
			}
			// Copy out of r's internal buffer before the next read
			// overwrites it.
			acc = append(acc, chunk...)
		default:
			// io.EOF or another error. chunk may hold the final line if the
			// file ended without a trailing newline.
			if len(chunk) > 0 {
				if len(acc)+len(chunk) > maxBytes {
					// Cap exceeded on the terminal chunk. No newline to
					// drain to (we're at EOF), so just report.
					return nil, true, err
				}
				acc = append(acc, chunk...)
			}
			if len(acc) > 0 && acc[len(acc)-1] == '\r' {
				acc = acc[:len(acc)-1]
			}
			return acc, false, err
		}
	}
}

// drainToNewline consumes bytes from r until a '\n' is found or an error occurs.
// Used by readLineBounded to skip past the tail of an oversized line.
func drainToNewline(r *bufio.Reader) error {
	for {
		_, err := r.ReadSlice('\n')
		if err == nil {
			return nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return err
	}
}

// ParseTurn parses a single JSONL line into a Turn
func ParseTurn(data []byte) (*models.Turn, error) {
	turn, _, _, err := parseTurnInternal(data)
	return turn, err
}

// strippedRawTurn is the JSON shape written in place of an oversized raw line.
// Fields under the _ccvault_ namespace mark the shape as a ccvault-side
// synthetic replacement, distinguishable from the source-format fields
// (uuid/type/timestamp/etc.) which are preserved so downstream consumers can
// still identify the turn.
type strippedRawTurn struct {
	Stripped     bool   `json:"_ccvault_stripped"`
	OriginalSize int    `json:"_ccvault_original_size"`
	UUID         string `json:"uuid"`
	ParentUUID   string `json:"parentUuid,omitempty"`
	SessionID    string `json:"sessionId"`
	Type         string `json:"type"`
	Timestamp    string `json:"timestamp"`
}

// strippedRawJSON returns a small JSON placeholder that stands in for the raw
// line when the original exceeds maxRawJSONBytes. Callers use this to keep
// turns.raw_json bounded while preserving the turn's identity and structural
// metadata; the actual payload (which for the reported case is base64 PDF
// content that no consumer indexes anyway) is dropped.
func strippedRawJSON(raw *models.RawTurn, originalSize int) []byte {
	stub := strippedRawTurn{
		Stripped:     true,
		OriginalSize: originalSize,
		UUID:         raw.UUID,
		ParentUUID:   raw.ParentUUID,
		SessionID:    raw.SessionID,
		Type:         raw.Type,
		Timestamp:    raw.Timestamp,
	}
	out, err := json.Marshal(stub)
	if err != nil {
		// json.Marshal on a struct of primitives cannot realistically fail;
		// fall back to a static string so we never lose the "stripped" signal.
		return []byte(`{"_ccvault_stripped":true,"_ccvault_marshal_error":true}`)
	}
	return out
}

// parseTurnInternal parses a JSONL line and returns the Turn, the intermediate
// RawTurn so callers can reuse it without re-unmarshalling, and whether the
// line's timestamp was readable.
//
// The third return value is false when the turn's Timestamp is a fallback
// rather than the value the transcript recorded — reported here because this
// is the only place that knows, and silently substituting one is #146.
func parseTurnInternal(data []byte) (*models.Turn, *models.RawTurn, bool, error) {
	var raw models.RawTurn
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, nil, false, fmt.Errorf("unmarshal turn: %w", err)
	}

	// Skip entries without UUID (like file-history-snapshot)
	if raw.UUID == "" {
		return nil, &raw, true, nil
	}

	// Parse timestamp. An unreadable one leaves ts at the zero value, which
	// updateSessionMetadata then keeps out of the session's bounds — see the
	// note on TurnsWithUnparsableTimestamp for why that is the fallback and
	// time.Now() was not (#165).
	ts, tsErr := parseTimestamp(raw.Timestamp)
	timestampOK := tsErr == nil
	if !timestampOK {
		ts = time.Time{}
	}

	turn := &models.Turn{
		ID:        raw.UUID,
		SessionID: raw.SessionID,
		ParentID:  raw.ParentUUID,
		Type:      raw.Type,
		Timestamp: ts,
		RawJSON:   data,
	}

	// Extract content based on type
	turn.Content = extractContent(raw)

	// The one place ccvault assigns a type the transcript did not literally
	// state, and the reason it has to.
	//
	// Claude Code writes a tool result as a message with role "user" on a line
	// of type "user" — the API shape, where a result is something the caller
	// hands back to the model. Copied verbatim, that files every tool result
	// in the archive as a message the person typed: 315,985 of them in the
	// author's 52,375 sessions, 145,798 of which came out with no content at
	// all and rendered as a bare `User` header with nothing beneath it (#185).
	// Meanwhile `tool_result`, the type the TUI, the exporter and the MCP
	// schema were all written for, had zero rows.
	//
	// The test is that *every* block is a tool_result. A line carrying prose
	// beside a result is a user message with a result attached, and retagging
	// it would file the person's words under a machine's answer — see
	// TestParseTurn_MixedProseAndToolResultStaysUser.
	//
	// Applied after extractContent on purpose: the content of such a line is
	// the result's text and extractUserContent is what reads it, so the switch
	// there still keys on the transcript's own "user".
	if raw.Type == "user" && isToolResultOnlyUserLine(raw) {
		turn.Type = "tool_result"
	}

	// Extract token usage for assistant messages
	if raw.Type == "assistant" && raw.Message != nil {
		var msg models.RawAssistantMessage
		if err := json.Unmarshal(raw.Message, &msg); err == nil {
			if msg.Usage != nil {
				turn.InputTokens = msg.Usage.InputTokens
				turn.OutputTokens = msg.Usage.OutputTokens
				// The cache pair is read here for the same reason the other
				// two are, and was missing for the whole life of the parser
				// (#155). Claude Code's four usage counters are disjoint, so
				// these are additions to the turn's cost, not a breakdown of
				// input_tokens.
				turn.CacheReadTokens = msg.Usage.CacheReadInputTokens
				turn.CacheWriteTokens = msg.Usage.CacheCreationInputTokens
			}
		}
	}

	return turn, &raw, timestampOK, nil
}

// extractContent extracts searchable text content from a turn
func extractContent(raw models.RawTurn) string {
	switch raw.Type {
	case "user":
		if raw.Message != nil {
			var msg models.RawUserMessage
			if err := json.Unmarshal(raw.Message, &msg); err == nil {
				return extractUserContent(msg.Content)
			}
		}
	case "assistant":
		if raw.Message != nil {
			var msg models.RawAssistantMessage
			if err := json.Unmarshal(raw.Message, &msg); err == nil {
				var parts []string
				for _, c := range msg.Content {
					switch c.Type {
					case "text":
						parts = append(parts, c.Text)
					case "thinking":
						parts = append(parts, c.Thinking)
					case "tool_use":
						parts = append(parts, formatToolUse(c.Name, c.Input))
					}
				}
				return strings.Join(parts, "\n")
			}
		}
	case "tool_result":
		if raw.Data != nil {
			// Tool results can be complex - just store a summary
			return "[Tool Result]"
		}
	}
	return ""
}

// updateSessionMetadata updates session metadata from a turn and its parsed raw entry
func updateSessionMetadata(session *models.Session, turn *models.Turn, raw *models.RawTurn) {
	// Set session ID
	if session.ID == "" && turn.SessionID != "" {
		session.ID = turn.SessionID
	}

	// The session's time bounds are the extrema of its turn timestamps, kept as
	// a running min and max over the walk the parser already does.
	//
	// Not the first and last turns in file order, which is what they used to
	// be. #29 measured the clock skew that makes those different things: on the
	// author's archive 6,597 adjacent turn pairs carry a timestamp earlier than
	// the turn written before them, so a session's ended_at could precede a
	// turn belonging to it (12 sessions did) and its started_at could follow
	// one (15 did). db.MergeFrom decides which copy of a duplicated session
	// wins by comparing ended_at, so a skewed final turn kept the wrong copy;
	// stats and orient report date ranges from the pair; and a duration
	// computed from them could come out negative.
	//
	// This is not an argument against #29's ordering decision and does not
	// touch it. File order reproduced the source .jsonl sequence 1,412 times
	// out of 1,413 where timestamp-first ordering managed 1,336, so file order
	// is the correct sequence. MAX is the correct extremum. Different
	// questions, and the skew affects them differently.
	//
	// IsZero is the "not set yet" sentinel for both, so the first turn seen
	// establishes the pair regardless of where it sits in time.
	//
	// Only turns whose time the transcript actually recorded take part. A turn
	// with an unreadable timestamp carries the zero time (#165) and says
	// nothing about when the session ran, so letting it into the extrema would
	// put a value in these columns that no line of the transcript claims.
	// db.MergeFrom chooses between duplicate copies of a session by comparing
	// ended_at, which makes that a data-loss path rather than a display bug:
	// the old time.Now() fallback dragged ended_at past every real transcript
	// and won merges it should have lost. The zero value would drag started_at
	// to year 1 for the same reason, because the sentinel cannot tell "not set
	// yet" from "set to zero". Skipping is what holds both off.
	//
	// The same rule, for the same reason, is what the codex adapter's
	// !ts.IsZero() guard already does.
	if !turn.Timestamp.IsZero() {
		if session.StartedAt.IsZero() || turn.Timestamp.Before(session.StartedAt) {
			session.StartedAt = turn.Timestamp
		}
		if session.EndedAt.IsZero() || turn.Timestamp.After(session.EndedAt) {
			session.EndedAt = turn.Timestamp
		}
	}

	// Accumulate tokens. All four counters, because all four are disjoint
	// parts of what the session cost — see models.Turn.CacheReadTokens.
	session.InputTokens += int64(turn.InputTokens)
	session.OutputTokens += int64(turn.OutputTokens)
	session.CacheReadTokens += int64(turn.CacheReadTokens)
	session.CacheWriteTokens += int64(turn.CacheWriteTokens)

	// Extract model from assistant messages
	if turn.Type == "assistant" && session.Model == "" && raw.Message != nil {
		var msg models.RawAssistantMessage
		if err := json.Unmarshal(raw.Message, &msg); err == nil && msg.Model != "" {
			session.Model = msg.Model
		}
	}

	if raw.GitBranch != "" && session.GitBranch == "" {
		session.GitBranch = raw.GitBranch
	}

	// CWD is the ground truth for a top-level session's project path; lock to
	// first non-empty value. Canonicalised on the way in, so an agent worktree
	// resolves to the repository it was cut from rather than becoming a
	// project of its own (#72). This is the single place a cwd becomes a
	// project identity, which is why the rule lives here and not in the sync
	// layer — that layer also runs codex, jeff, hex and nanoclaw, none of
	// which have a .claude/worktrees convention to know about.
	//
	// A subagent transcript's cwd does NOT decide its project: the adapter
	// overrides this with SubagentProjectPath, which takes the project of the
	// session that dispatched it (#156).
	if raw.CWD != "" && session.ProjectPath == "" {
		session.ProjectPath = CanonicalProjectPath(raw.CWD)
	}
}

// parseTimestamp parses ISO 8601 timestamp string
func parseTimestamp(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, fmt.Errorf("empty timestamp")
	}

	// Try RFC3339 first (most common)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}

	// Try RFC3339Nano
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}

	// Try ISO 8601 with milliseconds
	if t, err := time.Parse("2006-01-02T15:04:05.000Z", s); err == nil {
		return t, nil
	}

	return time.Time{}, fmt.Errorf("unknown timestamp format: %s", s)
}

// ExtractToolUses extracts tool usage information from turns, including each
// call's provider id, its input, and the result it produced.
//
// Two passes, because a call and its result live in different turns: Claude
// Code writes the tool_use into an assistant message and the tool_result into
// a later user message, joined by tool_use_id. The first pass indexes the
// results by that id, the second walks the calls and attaches them. Pairing by
// position would be wrong — a turn can issue several calls and their results
// come back in whatever order they finish, which they demonstrably do.
func ExtractToolUses(turns []models.Turn) []models.ToolUse {
	results := collectToolResults(turns)

	var toolUses []models.ToolUse

	for _, turn := range turns {
		if turn.Type != "assistant" {
			continue
		}

		var raw models.RawTurn
		if err := json.Unmarshal(turn.RawJSON, &raw); err != nil || raw.Message == nil {
			continue
		}

		var msg models.RawAssistantMessage
		if err := json.Unmarshal(raw.Message, &msg); err != nil {
			continue
		}

		for _, content := range msg.Content {
			if content.Type != "tool_use" {
				continue
			}

			toolUse := models.ToolUse{
				TurnID:    turn.ID,
				SessionID: turn.SessionID,
				ToolName:  content.Name,
				ToolUseID: content.ID,
				Timestamp: turn.Timestamp,
			}

			// Extract file path for file-related tools
			if content.Input != nil {
				toolUse.FilePath = extractFilePath(content.Name, content.Input)
				toolUse.InputJSON = string(content.Input)
				toolUse.InputLength = len(content.Input)
			}

			if res, ok := results[content.ID]; ok && content.ID != "" {
				decided := toolpayload.ResultFromJSON(content.Name, res.content)
				toolUse.HasResult = true
				toolUse.ResultContent = decided.Content
				toolUse.ResultLength = decided.Length
				toolUse.ResultOmittedReason = decided.OmitReason
				// Taken from the block rather than derived from the text, and
				// set for every result including an omitted one: a failing
				// Read keeps no body to read a failure out of, and the flag is
				// the only thing such a row can say (#83).
				toolUse.IsError = &res.isError
			}

			toolUses = append(toolUses, toolUse)
		}
	}

	return toolUses
}

// rawToolResultBlock is a tool_result content block as the transcripts write
// it. Content is left raw because the sources use both shapes: a JSON string
// (129,296 of the author's results) and an array of content blocks (130,516).
//
// Separate from models.UserContentBlock, whose Content is typed as a string
// and so cannot hold the array shape at all. That typing was the archive's
// 145,648 contentless turns, and #185 fixed it for the content path by
// reading user blocks through userContentBlock instead; this type stays its
// own because it reads a different field set (tool_use_id and is_error) for a
// different purpose.
//
// IsError is a plain bool because
// the key is absent far more often than present — 217,581 of the author's
// 273,023 result blocks omit it — and Claude Code writes it only when there is
// something to report, so absent and false are the same statement. The
// three-state distinction lives one level up, in whether a result was found at
// all (#83).
type rawToolResultBlock struct {
	Type      string          `json:"type"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// toolResult is one answered call: the result content as the transcript wrote
// it, and whether that result reported a failure. Kept together because they
// come from the same block and a lookup that returned only the content would
// make the flag unreachable without a second pass over every turn.
type toolResult struct {
	content json.RawMessage
	isError bool
}

// collectToolResults indexes every tool_result block in the session by the
// tool_use_id it names. Walks all turn types rather than only "user": the
// result's home is the user message today, and keying on the block's own type
// costs nothing and does not assume that.
func collectToolResults(turns []models.Turn) map[string]toolResult {
	results := make(map[string]toolResult)

	for _, turn := range turns {
		var raw models.RawTurn
		if err := json.Unmarshal(turn.RawJSON, &raw); err != nil || raw.Message == nil {
			continue
		}

		var msg models.RawUserMessage
		if err := json.Unmarshal(raw.Message, &msg); err != nil {
			continue
		}

		var blocks []rawToolResultBlock
		if err := json.Unmarshal(msg.Content, &blocks); err != nil {
			// A plain-string message content, which carries no tool results.
			continue
		}

		for _, block := range blocks {
			if block.Type != "tool_result" || block.ToolUseID == "" {
				continue
			}
			// First result wins. A repeated id would mean the transcript
			// answered one call twice; keeping the first keeps the mapping
			// stable regardless of which turn order a re-parse sees.
			if _, seen := results[block.ToolUseID]; !seen {
				results[block.ToolUseID] = toolResult{content: block.Content, isError: block.IsError}
			}
		}
	}

	return results
}

// userContentBlock is one block of a user message's content array, read with
// the tool_result's own nested content left raw.
//
// Deliberately not models.UserContentBlock, whose Content is typed as a
// string. Claude Code writes a tool_result's content both ways — a JSON string
// (11,000 of 11,408 blocks in the author's live transcripts) and an array of
// content blocks (408) — and a string-typed field makes the whole block array
// fail to unmarshal whenever the array shape appears. That failure is the
// archive's 145,648 contentless turns, and it takes any prose on the same line
// down with it.
//
// models.UserContentBlock is left alone because the two adapters that use it
// read only Type and IsError from it, and widening it there would buy nothing.
type userContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Content   json.RawMessage `json:"content"`
	ToolUseID string          `json:"tool_use_id"`
}

// parseUserBlocks reads a user message's content as an array of blocks,
// reporting whether it was one at all.
func parseUserBlocks(content json.RawMessage) ([]userContentBlock, bool) {
	var blocks []userContentBlock
	if err := json.Unmarshal(content, &blocks); err != nil {
		return nil, false
	}
	return blocks, true
}

// isToolResultOnlyUserLine reports whether a transcript line's user message
// consists entirely of tool_result blocks — the condition under which
// parseTurnInternal retags the turn as a tool result. See the comment there.
//
// An empty block array is not a tool result. Nor is a plain-string message,
// nor one with no message at all: those are the archive's pasted screenshots,
// PDFs and oversized-line placeholders, which carry no result and never did.
func isToolResultOnlyUserLine(raw models.RawTurn) bool {
	if raw.Message == nil {
		return false
	}
	var msg models.RawUserMessage
	if err := json.Unmarshal(raw.Message, &msg); err != nil {
		return false
	}
	blocks, ok := parseUserBlocks(msg.Content)
	if !ok || len(blocks) == 0 {
		return false
	}
	for _, block := range blocks {
		if block.Type != "tool_result" {
			return false
		}
	}
	return true
}

// extractUserContent extracts text from user message content
// Content can be a plain string or an array of content blocks
func extractUserContent(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}

	// Try parsing as a string first (most common case)
	var str string
	if err := json.Unmarshal(content, &str); err == nil {
		return str
	}

	// Try parsing as an array of content blocks
	blocks, ok := parseUserBlocks(content)
	if !ok {
		return ""
	}

	var parts []string
	for _, block := range blocks {
		switch block.Type {
		case "text":
			if block.Text != "" {
				parts = append(parts, block.Text)
			}
		case "tool_result":
			if preview := toolResultPreview(block.Content); preview != "" {
				parts = append(parts, preview)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// toolResultPreview summarises a tool_result block's content for
// turns.content, empty when there is nothing to summarise.
//
// A preview and not the body, which is the decision issue #185 forces. The
// whole result text already lives twice over: verbatim in turns.raw_json, and
// in tool_uses.result_content under issue #28's storage policy with an FTS
// index of its own. Measured on the author's archive, storing the bodies here
// as well would add 453 MB to a turns.content that holds 296 MB today and
// duplicate every one of those bytes into turns_fts, where they would compete
// with the prose that index exists to find. A bounded preview is what the
// column has carried for these turns since the beginning; the type is what was
// wrong.
//
// Image-bearing results get no preview at all. The payload is base64 and must
// not reach the FTS index (#28 detects it by shape, not by tool name), and a
// preview that kept a screenshot's caption while dropping the screenshot is a
// misleading partial — 191 of the archive's affected rows are exactly that.
func toolResultPreview(content json.RawMessage) string {
	text, ok := toolResultText(content)
	if !ok || text == "" {
		return ""
	}
	return fmt.Sprintf("[Tool Result: %s]", truncateRunes(text, 200))
}

// toolResultText reads the text a tool_result block carries, reporting false
// when the content holds an image payload and so must not be summarised.
//
// Both shapes the transcripts use: a JSON string, and an array of blocks whose
// text blocks are joined. A tool_reference block, which names a tool and
// carries no text, contributes nothing — 287 of the archive's rows are a list
// of those and legitimately have no content.
func toolResultText(content json.RawMessage) (string, bool) {
	if len(content) == 0 {
		return "", false
	}

	var str string
	if err := json.Unmarshal(content, &str); err == nil {
		return str, true
	}

	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &blocks); err != nil {
		return "", false
	}

	var parts []string
	for _, block := range blocks {
		switch block.Type {
		case "image":
			return "", false
		case "text":
			if block.Text != "" {
				parts = append(parts, block.Text)
			}
		}
	}
	return strings.Join(parts, "\n"), true
}

// truncateRunes truncates a string to maxRunes characters with an ellipsis.
//
// Characters and not bytes, for two reasons that both bite. A byte cut can
// land inside a multi-byte rune and put invalid UTF-8 into turns.content and
// the FTS index behind it. And migration 017 reproduces this same string in
// SQL, where substr()'s unit is the character: 67,845 of the 145,006 rows that
// migration fills carry a non-ASCII character inside the first 200, so a byte
// cut here would disagree with it on 47% of them.
func truncateRunes(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxRunes-3]) + "..."
}

// formatToolUse formats a tool use with key parameters
func formatToolUse(toolName string, input json.RawMessage) string {
	if len(input) == 0 {
		return fmt.Sprintf("[Tool: %s]", toolName)
	}

	var params map[string]interface{}
	if err := json.Unmarshal(input, &params); err != nil {
		return fmt.Sprintf("[Tool: %s]", toolName)
	}

	// Extract key parameters based on tool type
	var details []string

	switch toolName {
	case "Bash":
		if cmd, ok := params["command"].(string); ok {
			// Truncate long commands
			if len(cmd) > 100 {
				cmd = cmd[:97] + "..."
			}
			details = append(details, fmt.Sprintf("$ %s", cmd))
		}
	case "Read":
		if fp, ok := params["file_path"].(string); ok {
			details = append(details, fp)
		}
	case "Write":
		if fp, ok := params["file_path"].(string); ok {
			details = append(details, fmt.Sprintf("-> %s", fp))
		}
	case "Edit":
		if fp, ok := params["file_path"].(string); ok {
			details = append(details, fp)
		}
	case "Glob":
		if pattern, ok := params["pattern"].(string); ok {
			details = append(details, pattern)
		}
	case "Grep":
		if pattern, ok := params["pattern"].(string); ok {
			if len(pattern) > 50 {
				pattern = pattern[:47] + "..."
			}
			details = append(details, fmt.Sprintf("/%s/", pattern))
		}
	case "Task":
		if desc, ok := params["description"].(string); ok {
			details = append(details, desc)
		}
	case "WebFetch":
		if url, ok := params["url"].(string); ok {
			details = append(details, url)
		}
	case "WebSearch":
		if query, ok := params["query"].(string); ok {
			details = append(details, fmt.Sprintf("\"%s\"", query))
		}
	case "TodoWrite":
		if todos, ok := params["todos"].([]interface{}); ok {
			details = append(details, fmt.Sprintf("%d items", len(todos)))
		}
	case "Skill":
		if skill, ok := params["skill"].(string); ok {
			details = append(details, skill)
		}
	default:
		// For other tools, try to extract common parameters
		for _, key := range []string{"file_path", "path", "query", "command", "name"} {
			if v, ok := params[key].(string); ok && v != "" {
				if len(v) > 60 {
					v = v[:57] + "..."
				}
				details = append(details, v)
				break
			}
		}
	}

	if len(details) > 0 {
		return fmt.Sprintf("[Tool: %s] %s", toolName, strings.Join(details, " "))
	}
	return fmt.Sprintf("[Tool: %s]", toolName)
}

// extractFilePath extracts file path from tool input
func extractFilePath(toolName string, input json.RawMessage) string {
	var params map[string]interface{}
	if err := json.Unmarshal(input, &params); err != nil {
		return ""
	}

	// Different tools use different parameter names
	pathKeys := []string{"file_path", "path", "filepath", "filename"}
	for _, key := range pathKeys {
		if v, ok := params[key].(string); ok && v != "" {
			return v
		}
	}

	return ""
}
