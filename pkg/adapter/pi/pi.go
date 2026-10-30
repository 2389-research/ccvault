// ABOUTME: pi source adapter that discovers and parses sessions written by the pi coding agent.
// ABOUTME: Implements the SourceAdapter interface for pi's append-only JSONL session trees.

package pi

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/2389-research/ccvault/pkg/adapter"
	"github.com/2389-research/ccvault/pkg/parser"
	"github.com/2389-research/ccvault/pkg/toolpayload"
)

func init() {
	adapter.Register("pi", func() adapter.SourceAdapter {
		return New()
	})
}

// Adapter implements adapter.SourceAdapter for pi JSONL session files.
type Adapter struct{}

// New creates a new pi adapter.
func New() *Adapter {
	return &Adapter{}
}

// Name returns the adapter's identifier.
func (a *Adapter) Name() string {
	return "pi"
}

// sessionHeader is the first line of a pi session file.
//
// pi stamps a version here from v2 onwards, and this adapter deliberately does
// not read it. What matters is whether a given entry carries an id, and that is
// decided per entry in turnIdentity rather than inferred from a header field —
// a file pi half-migrated, or one a third-party writer produced, answers the
// per-entry question correctly and the header question wrongly.
//
// Provider and ModelID are the v1 spelling of the model, which later versions
// moved out into their own model_change entry.
//
// BranchedFrom and ParentSession both name the session this one was forked
// from — the first is v1's spelling, the second is what pi writes now. Neither
// is mapped to parent_session_id: that column records the session that
// *dispatched* this one, which ccvault renders as "subagent of X", and a fork
// was not dispatched by the session it forked from.
type sessionHeader struct {
	Type          string `json:"type"`
	ID            string `json:"id"`
	CWD           string `json:"cwd"`
	Provider      string `json:"provider"`
	ModelID       string `json:"modelId"`
	BranchedFrom  string `json:"branchedFrom"`
	ParentSession string `json:"parentSession"`
}

// sessionsDir returns the directory Discover walks for a configured root.
//
// One rule covers the two layouts pi sessions arrive in. pi itself writes
// <root>/agent/sessions/<encoded cwd>/, so a root pointed at ~/.pi finds its
// sessions one level down. bb, a thread UI that drives pi, writes the same
// format flat into ~/.bb/pi-bridge-sessions with no per-project directory, and
// a root pointed there is itself the sessions directory. Both are the same
// source with the same parser, so neither needs its own adapter type.
func sessionsDir(root string) string {
	nested := filepath.Join(root, "agent", "sessions")
	if info, err := os.Stat(nested); err == nil && info.IsDir() {
		return nested
	}
	return root
}

// Discover walks a pi root for session files and keys each one on the cwd its
// header records.
//
// The cwd is read from the file rather than decoded from the directory name
// because the encoding cannot be reversed: pi replaces every separator in the
// path with a dash, so "/Users/dyl/work/pi-mono" and "/Users/dyl/work/pi/mono"
// both land in --Users-dyl-work-pi-mono--. The header says which it was.
func (a *Adapter) Discover(root string) ([]adapter.SessionFile, error) {
	dir := sessionsDir(root)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil, nil
	}

	var files []adapter.SessionFile

	// A session file can be reachable by more than one path once symlinks are
	// followed — sessions/current -> sessions/--some-project-- leaves every
	// file in that project reachable both ways. Keyed on the resolved path, so
	// whichever route arrives first wins and the rest are the same file again.
	// Reporting it twice would give one session two source files, and each
	// sync would rewrite the same rows from whichever path it reached last.
	seen := make(map[string]bool)

	// collect reports one walked path, naming it reportAs. The two differ only
	// when the walk is running inside a symlink's target — see below.
	collect := func(path, reportAs string, info os.FileInfo) {
		if info.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			resolved = path
		}
		if seen[resolved] {
			return
		}
		hdr, err := readSessionHeader(path)
		if err != nil {
			// Not a pi session. Declining it here keeps Discover and Parse
			// agreeing on which files are sessions: a file with no header has
			// no id for Parse to key a session row on either.
			return
		}
		seen[resolved] = true
		files = append(files, adapter.SessionFile{
			Path:        reportAs,
			ProjectPath: hdr.CWD,
			ModTime:     info.ModTime(),
		})
	}

	// The sessions directory may itself be a link — ~/.pi kept on another disk
	// or in a synced folder. filepath.Walk lstats its root, so a symlink there
	// is reported once as a symlink and descended into never, and the project
	// directories under it are reached through the link rather than past it.
	// Resolving the root first is what makes the walk see them at all.
	if err := walkReportingThrough(dir, collect); err != nil {
		return nil, fmt.Errorf("walking pi sessions: %w", err)
	}

	if err := walkSymlinkedProjectDirs(dir, collect); err != nil {
		return nil, err
	}

	return files, nil
}

// walkReportingThrough walks dir, resolving it if it is a symlink, and names
// every file by the path it has *under dir* rather than under the resolved
// target.
//
// That distinction is the whole reason this is not a bare filepath.Walk. The
// archive stores a session's path and an incremental sync re-checks its mtime
// there, so a session has to be recorded where the configured root holds it.
// Recording the resolved path instead would work until the link moved, and
// would then orphan every row that named the old target.
func walkReportingThrough(dir string, collect func(path, reportAs string, info os.FileInfo)) error {
	walkRoot := dir
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		walkRoot = resolved
	}

	return filepath.Walk(walkRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(walkRoot, path)
		if relErr != nil {
			return relErr
		}
		collect(path, filepath.Join(dir, rel), info)
		return nil
	})
}

// walkSymlinkedProjectDirs covers the project directories that are symlinks.
//
// pi goes out of its way to support them: its own session listing keeps a
// child of the sessions directory when it `isDirectory() || isSymbolicLink()`.
// filepath.Walk deliberately does not follow symlinks, so the walk above finds
// nothing inside one — and finds it silently, which is the reason this exists.
// A session missing from the archive is indistinguishable from a session that
// was never written.
//
// Only immediate children are considered, so what gets followed is what the
// user deliberately placed in pi's sessions directory, not wherever a link
// deeper in the tree happens to point.
//
// Files are reported at their path *through* the link rather than at the
// link's target, so the archive records a session where the configured root
// actually holds it and an incremental sync keeps checking it there.
func walkSymlinkedProjectDirs(dir string, collect func(path, reportAs string, info os.FileInfo)) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("reading pi sessions directory: %w", err)
	}

	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink == 0 {
			continue
		}
		link := filepath.Join(dir, entry.Name())

		// Stat follows the link. A dangling one, or one pointing at a file
		// rather than a directory, is left alone: the walk above already
		// reported a symlinked *file*, and a broken link is not a session
		// directory and must not fail the whole source's discovery.
		if info, err := os.Stat(link); err != nil || !info.IsDir() {
			continue
		}

		// Same rule as the sessions root itself: walk what the link points at,
		// name what the link holds.
		if err := walkReportingThrough(link, collect); err != nil {
			return fmt.Errorf("walking pi sessions through %s: %w", link, err)
		}
	}

	return nil
}

// readSessionHeader reads the session header from the start of a pi session
// file. The header is the first line pi writes, so a file whose first
// non-blank line is anything else is not treated as a pi session rather than
// being scanned further — a 3.5MB transcript is not worth re-reading to look
// for a header that pi would never have written there.
func readSessionHeader(path string) (sessionHeader, error) {
	f, err := os.Open(path)
	if err != nil {
		return sessionHeader{}, err
	}
	defer func() { _ = f.Close() }()

	reader := bufio.NewReader(f)
	for {
		raw, readErr := adapter.ReadLine(reader)
		if len(raw) > 0 {
			var hdr sessionHeader
			if err := json.Unmarshal(raw, &hdr); err != nil {
				return sessionHeader{}, fmt.Errorf("first line of %s is not JSON: %w", path, err)
			}
			if hdr.Type != "session" {
				return sessionHeader{}, fmt.Errorf("first line of %s is a %q entry, not a session header", path, hdr.Type)
			}
			return hdr, nil
		}
		if readErr != nil {
			return sessionHeader{}, fmt.Errorf("reading %s: %w", path, readErr)
		}
	}
}

// sessionIDPrefix namespaces pi session IDs to prevent collisions with other
// sources' IDs on the global sessions primary key. pi names a session with a
// UUID, which is the shape Claude Code uses too.
const sessionIDPrefix = "pi:"

// entryLine is the part of every pi session entry that does not depend on its
// type.
//
// ID and ParentID are absent on v1 sessions — pi mints them in memory when it
// opens one and only writes them back if it appends. Both of the real sessions
// pi ships as fixtures are v1, so this is the common case rather than a legacy
// corner.
type entryLine struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	ParentID  *string         `json:"parentId"`
	Timestamp string          `json:"timestamp"`
	Message   json.RawMessage `json:"message"`
	Provider  string          `json:"provider"`
	ModelID   string          `json:"modelId"`

	// Content and CustomType belong to a custom_message entry, which is an
	// extension injecting something the model then saw. Not to be confused
	// with a "custom" entry, which is extension state the model never saw.
	Content    json.RawMessage `json:"content"`
	CustomType string          `json:"customType"`

	// Usage is on a usage entry, and on a compaction or branch_summary when pi
	// knew what generating the summary cost. Summary, Kind and Note belong to
	// those same three.
	Usage   *piUsage `json:"usage"`
	Summary string   `json:"summary"`
	Kind    string   `json:"kind"`
	Note    string   `json:"note"`
}

// piMessage is the message a "message" entry carries. pi has one envelope for
// every role, so the fields below are the union of all of them and which ones
// are set depends on Role.
type piMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`

	// assistant
	StopReason   string   `json:"stopReason"`
	ErrorMessage string   `json:"errorMessage"`
	Usage        *piUsage `json:"usage"`

	// toolResult. pi repeats the tool's name here; the name used for the
	// storage policy is the one on the call, which is present even when no
	// result ever arrives.
	ToolCallID string `json:"toolCallId"`
	IsError    *bool  `json:"isError"`

	// bashExecution — pi's "!" command, whose text is in these fields rather
	// than in Content, which is null on this role.
	Command   string `json:"command"`
	Output    string `json:"output"`
	ExitCode  *int   `json:"exitCode"`
	Cancelled bool   `json:"cancelled"`

	// branchSummary and compactionSummary
	Summary string `json:"summary"`
}

// contentBlock is one block of a message's content list.
type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// textFromContent renders a message's content as the text ccvault stores and
// searches. pi writes content either as a plain string or as a list of blocks,
// and both shapes appear in the real sessions.
//
// Thinking text is kept, which is what the Claude Code adapter does too: a
// search for something the model only said while reasoning should find the
// turn it said it in. Images contribute nothing.
//
// Tool calls contribute nothing either, which leaves an assistant message made
// entirely of them with no content at all — 217 of the 544 turns in pi's own
// before-compaction fixture. That is deliberate: the tool's name, arguments
// and result are stored on the tool use, where they are indexed, so repeating
// them here would make a search for "read" match every turn that read a file.
// pi.TestParse_APureToolCallTurnHasNoContent is the pin.
func textFromContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString
	}

	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}

	var parts []string
	for _, block := range blocks {
		switch block.Type {
		case "text":
			if block.Text != "" {
				parts = append(parts, block.Text)
			}
		case "thinking":
			if block.Thinking != "" {
				parts = append(parts, block.Thinking)
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

// bashExecutionText renders a "!" command and what it printed.
//
// This is ccvault's rendering, not pi's. pi builds its own prose for the model
// ("Ran `ls`" and a fenced block); reproducing it here would be a second copy
// of a function that can change without this one noticing. The exact fields
// are in the turn's raw_json either way.
func bashExecutionText(msg piMessage) string {
	text := "$ " + msg.Command
	if msg.Output != "" {
		text += "\n" + msg.Output
	}
	if msg.Cancelled {
		text += "\n\n(cancelled)"
	} else if msg.ExitCode != nil && *msg.ExitCode != 0 {
		text += fmt.Sprintf("\n\n(exit %d)", *msg.ExitCode)
	}
	return text
}

// turnTypeForRole maps a pi message role onto the three turn types ccvault
// stores.
//
// toolResult is not among them, and callers must take it out before asking:
// it answers a tool call rather than being a turn of its own, and it is
// attached to that call's row the way every other source's tool results are.
// The caller has to special-case it regardless, because recording the result
// needs the toolCallId this function never sees — so the decision lives there
// and only there.
//
// Everything else maps to "system", including a role pi has not shipped yet. A
// role is only ever something that was in the conversation, so storing an
// unrecognised one as a system turn keeps its text searchable, where dropping
// it would lose the content and say nothing about having done so.
func turnTypeForRole(role string) string {
	switch role {
	case "user", "bashExecution":
		return "user"
	case "assistant":
		return "assistant"
	default:
		// system, custom, hookMessage (what custom was called before pi's v3
		// rename), branchSummary, compactionSummary, and anything new.
		return "system"
	}
}

// contentForRole picks the text of a message, which is in a different place
// for the roles that do not use the content field.
func contentForRole(msg piMessage) string {
	switch msg.Role {
	case "bashExecution":
		return bashExecutionText(msg)
	case "branchSummary", "compactionSummary":
		return msg.Summary
	default:
		return textFromContent(msg.Content)
	}
}

// Parse reads a pi JSONL session file and converts it to adapter.ParsedSession.
func (a *Adapter) Parse(path string) (*adapter.ParsedSession, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening pi session: %w", err)
	}
	defer func() { _ = f.Close() }()

	return a.ParseTranscript(path, f)
}

// ParseTranscript reads the same transcript from a reader rather than from the
// file at originalPath — see adapter.TranscriptReparser for why that is worth
// having.
//
// A pi session answers everything from its own lines: the id, cwd and model
// all arrive in the transcript, so originalPath is unused here and kept only
// to satisfy the interface.
//
// Worth knowing before trusting this against stored lines: pi's session header
// is not a turn, so a transcript reassembled from turns.raw_json does not
// contain it and the result has no ID, ProjectPath or header-supplied model.
// The token counters are unaffected, which is what the #155 backfill reads —
// every figure pi records sits on a line that became a turn, including the
// usage and compaction entries, which are stored as turns of their own for
// exactly this reason. TestParseTranscript_TokenTotalsSurviveTheArchiveRoundTrip
// and TestParseTranscript_HasNoSessionIdentityWithoutItsHeader pin both halves.
func (a *Adapter) ParseTranscript(originalPath string, r io.Reader) (*adapter.ParsedSession, error) {
	_ = originalPath

	reader := bufio.NewReader(r)

	var (
		hdr       sessionHeader
		model     string
		startedAt time.Time
		endedAt   time.Time

		turns []adapter.ParsedTurn

		// Tool calls and the results answering them, matched after the whole
		// file is read — see callSite.
		results   = make(map[string]toolResult)
		callSites []callSite

		// Turn ids for the v1 sessions that carry none of their own, and the
		// chain that stands in for their missing parentId. See turnIdentity.
		turnCounter int
		lastTurnID  string

		// Lines pi wrote that are not JSON. Counted rather than ignored so a
		// half-written file shows up in sync's warning instead of looking
		// like a short session.
		skippedLines int

		// The first timestamp string the layout below could not read, kept
		// verbatim. A count says something is unreadable; the sample says
		// which layout is missing (#146).
		unreadableSample string
	)

	// turnIdentity names a turn and points it at its parent.
	//
	// v2 and later carry both on the entry, and the session id is prepended
	// because pi's entry ids are 8 hex characters unique within one session's
	// tree, not across an archive that holds every source's turns.
	//
	// v1 carries neither — pi mints them in memory when it opens such a file
	// and only writes them back if it appends — so the turn is numbered by the
	// order it was read in and chained to the turn before it. That is what
	// pi's own v1 migration does, with one difference: the chain runs through
	// the turns ccvault stores rather than through every entry, because the
	// entries in between are not turns.
	turnIdentity := func(line entryLine) (id, parentID string) {
		prefix := sessionIDPrefix + hdr.ID
		if line.ID != "" {
			if line.ParentID != nil && *line.ParentID != "" {
				parentID = prefix + "-" + *line.ParentID
			}
			return prefix + "-" + line.ID, parentID
		}
		turnCounter++
		return fmt.Sprintf("%s-turn-%d", prefix, turnCounter), lastTurnID
	}

	// appendTurn returns the new turn's index rather than a pointer to it.
	// `turns` keeps growing, so a pointer handed out here would point into a
	// stale backing array the moment it outlived one append.
	appendTurn := func(line entryLine, raw []byte, ts time.Time, turnType, content string) int {
		id, parentID := turnIdentity(line)
		turns = append(turns, adapter.ParsedTurn{
			ID:        id,
			ParentID:  parentID,
			Type:      turnType,
			Timestamp: ts,
			Content:   content,
			RawJSON:   json.RawMessage(adapter.MakeCopy(raw)),
		})
		lastTurnID = id
		return len(turns) - 1
	}

	for {
		raw, readErr := adapter.ReadLine(reader)
		if len(raw) == 0 {
			if readErr != nil {
				if errors.Is(readErr, io.EOF) {
					break
				}
				return nil, fmt.Errorf("reading pi session: %w", readErr)
			}
			continue
		}

		var line entryLine
		if err := json.Unmarshal(raw, &line); err != nil {
			skippedLines++
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				return nil, fmt.Errorf("reading pi session: %w", readErr)
			}
			if errors.Is(readErr, io.EOF) {
				break
			}
			continue
		}

		// pi writes RFC3339 with milliseconds and a Z on every entry. This
		// stays a one-layout list rather than joining codex's or jeff's —
		// those read different tools' transcripts and change independently of
		// this one. A line pi wrote in some other shape would otherwise become
		// a turn at the zero time, which sorts to the very start of the
		// archive and escapes every date filter while looking like a real
		// epoch value (#146).
		ts, tsErr := time.Parse(time.RFC3339Nano, line.Timestamp)
		if tsErr != nil && unreadableSample == "" && line.Timestamp != "" {
			unreadableSample = line.Timestamp
		}

		// The minimum and maximum rather than the first and last line: pi's
		// timestamps are not monotonic in file order, because a forked session
		// replays its ancestor's entries ahead of its own. That makes a fork's
		// range cover the history it inherited, which is what the file holds.
		if !ts.IsZero() {
			if startedAt.IsZero() || ts.Before(startedAt) {
				startedAt = ts
			}
			if ts.After(endedAt) {
				endedAt = ts
			}
		}

		switch line.Type {
		case "session":
			// First header wins, the same way codex takes its first
			// session_meta: a file's identity is the session it is, not the
			// ancestor whose entries it replays.
			if hdr.ID == "" {
				if err := json.Unmarshal(raw, &hdr); err == nil {
					model = joinModel(hdr.Provider, hdr.ModelID)
				}
			}

		case "model_change":
			// Last change wins, so the session reports the model it ended on.
			if m := joinModel(line.Provider, line.ModelID); m != "" {
				model = m
			}

		case "message":
			var msg piMessage
			if err := json.Unmarshal(line.Message, &msg); err != nil {
				skippedLines++
				break
			}

			if msg.Role == "toolResult" {
				if msg.ToolCallID != "" {
					results[msg.ToolCallID] = toolResult{
						content: textFromContent(msg.Content),
						isError: msg.IsError,
					}
				}
				break
			}

			idx := appendTurn(line, raw, ts, turnTypeForRole(msg.Role), contentForRole(msg))

			if msg.Role != "assistant" {
				break
			}
			msg.Usage.applyTo(&turns[idx])
			if msg.StopReason == "error" || msg.ErrorMessage != "" {
				turns[idx].HasError = true
			}
			collectToolCalls(msg.Content, idx, turns, &callSites)

		case "usage", "compaction", "branch_summary":
			// Spend pi recorded without a message to hang it on, and the two
			// summaries it writes to shorten a conversation.
			//
			// Each is stored as a turn of its own rather than folded into the
			// turn before it. Folding is what the codex adapter does with its
			// token_count lines, and because those numbers never reach
			// turns.raw_json, a re-derivation reads zero for them and the
			// backfill has to refuse every such session permanently. A turn
			// keeps the line itself in the archive, so the counters can always
			// be read back out of it.
			content := line.Summary
			if line.Type == "usage" {
				content = usageEntryText(line.Kind, line.Note)
			}
			// The index is taken in its own statement, not inlined into
			// &turns[...]: Go leaves the order between reading the `turns`
			// operand and calling a function in the index expression
			// unspecified, and appendTurn reassigns `turns`. Read first and the
			// slice still has its old length while the returned index is one
			// past it, which is an out-of-range panic mid-sync.
			idx := appendTurn(line, raw, ts, "system", content)
			line.Usage.applyTo(&turns[idx])

		case "custom_message":
			// An extension injecting content the model then saw, unlike a
			// "custom" entry, which is extension state it never saw.
			appendTurn(line, raw, ts, "system", textFromContent(line.Content))
		}

		if errors.Is(readErr, io.EOF) {
			break
		}
	}

	attachResults(turns, callSites, results)

	metadata := make(map[string]any)
	if forked := forkedFrom(hdr); forked != "" {
		metadata["pi_forked_from"] = forked
	}
	if skippedLines > 0 {
		metadata["skipped_lines"] = skippedLines
	}
	for _, turn := range turns {
		if turn.HasError {
			metadata["has_error"] = true
			break
		}
	}
	adapter.ReportUnreadableTimestamps(metadata, turns, unreadableSample)

	sessionID := ""
	if hdr.ID != "" {
		sessionID = sessionIDPrefix + hdr.ID
	}

	return &adapter.ParsedSession{
		ID:          sessionID,
		ProjectPath: hdr.CWD,
		DisplayName: parser.GetDisplayName(hdr.CWD),
		Turns:       turns,
		Model:       model,
		StartedAt:   startedAt,
		EndedAt:     endedAt,
		SourceName:  "pi",
		Metadata:    metadata,
	}, nil
}

// piUsage is pi's token record. The four fields map straight onto ccvault's
// four counters, which is unusual among the sources: pi already reports them
// disjoint, so nothing has to be split out of anything the way codex's
// cached_input_tokens has to be subtracted from its input_tokens.
//
// Two fields pi also writes are deliberately absent. `reasoning` is a subset
// of `output` and `cacheWrite1h` is a subset of `cacheWrite` — pi's own types
// say so — and the session row sums all four counters, so reading either one
// would charge those tokens a second time (#155). `cost` is pi's own money
// estimate and ccvault does not store a per-turn cost.
type piUsage struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cacheRead"`
	CacheWrite int64 `json:"cacheWrite"`
}

// applyTo adds a usage record to a turn's counters.
func (u *piUsage) applyTo(turn *adapter.ParsedTurn) {
	if u == nil {
		return
	}
	turn.InputTokens += u.Input
	turn.OutputTokens += u.Output
	turn.CacheReadTokens += u.CacheRead
	turn.CacheWriteTokens += u.CacheWrite
}

// usageEntryText labels a usage entry, which has no prose of its own. pi says
// what kind of spend it was and sometimes why.
func usageEntryText(kind, note string) string {
	if note == "" {
		return kind
	}
	return kind + ": " + note
}

// callSite records where a tool use landed so the result answering it can be
// attached once the whole file has been read. Resolving inline would only work
// while results follow their calls, which is true of the files on disk today
// and is not something the format promises.
type callSite struct {
	toolCallID string
	turnIdx    int
	useIdx     int
}

// toolResult is one result message, held until the call it answers is known.
type toolResult struct {
	content string

	// isError is pi's own flag, carried as a pointer so an unanswered call
	// stays unjudged rather than being recorded as a success. pi writes the
	// flag on every result it produces, so a nil here means no result arrived.
	isError *bool
}

// collectToolCalls records every toolCall block of an assistant message as a
// tool use on its turn, and remembers where each landed so the result
// answering it can be attached later.
//
// pi emits parallel calls as sibling blocks of one message, so a single turn
// routinely carries several.
func collectToolCalls(content json.RawMessage, turnIdx int, turns []adapter.ParsedTurn, sites *[]callSite) {
	var blocks []contentBlock
	if err := json.Unmarshal(content, &blocks); err != nil {
		return
	}

	turn := &turns[turnIdx]
	for _, block := range blocks {
		if block.Type != "toolCall" {
			continue
		}
		args := string(block.Arguments)
		turn.ToolUses = append(turn.ToolUses, adapter.ParsedToolUse{
			ToolName:    block.Name,
			ToolUseID:   block.ID,
			InputJSON:   args,
			InputLength: len(args),
		})
		if block.ID != "" {
			*sites = append(*sites, callSite{
				toolCallID: block.ID,
				turnIdx:    turnIdx,
				useIdx:     len(turn.ToolUses) - 1,
			})
		}
	}
}

// attachResults matches each recorded call to the result carrying its
// toolCallId and applies the storage policy to its output.
//
// Matching is on the id alone, with no fallback to order: pi writes a
// toolCallId on both halves of every pair, so a result that matches nothing
// is a result whose call is not in this file rather than one to guess a home
// for. A turn whose tool failed is marked as having an error, which is where
// the Claude Code adapter puts the same thing — pi records the failure on the
// result message, and the turn a reader sees is the one that made the call.
func attachResults(turns []adapter.ParsedTurn, sites []callSite, results map[string]toolResult) {
	for _, site := range sites {
		res, ok := results[site.toolCallID]
		if !ok {
			continue
		}
		tu := &turns[site.turnIdx].ToolUses[site.useIdx]
		decided := toolpayload.ResultFromText(tu.ToolName, res.content)
		tu.HasResult = true
		tu.ResultContent = decided.Content
		tu.ResultLength = decided.Length
		tu.ResultOmittedReason = decided.OmitReason
		tu.IsError = res.isError
		if res.isError != nil && *res.isError {
			turns[site.turnIdx].HasError = true
		}
	}
}

// joinModel renders pi's two-part model identity the way the rest of ccvault
// reads a model name: one string, provider first.
func joinModel(provider, modelID string) string {
	switch {
	case provider != "" && modelID != "":
		return provider + "/" + modelID
	case modelID != "":
		return modelID
	default:
		return ""
	}
}

// forkedFrom returns the id of the session this one was forked from, or empty
// when it was not.
//
// pi spells it two ways. parentSession holds the id directly; branchedFrom,
// which is what the older sessions carry, holds the whole path of the ancestor
// file, whose name is "<timestamp>_<id>.jsonl".
//
// It is reported under its own key rather than as parent_session_id — see the
// comment on sessionHeader for why a fork is not a dispatch.
func forkedFrom(hdr sessionHeader) string {
	if hdr.ParentSession != "" {
		return hdr.ParentSession
	}
	if hdr.BranchedFrom == "" {
		return ""
	}
	name := strings.TrimSuffix(filepath.Base(hdr.BranchedFrom), ".jsonl")
	if idx := strings.LastIndex(name, "_"); idx >= 0 {
		return name[idx+1:]
	}
	return name
}
