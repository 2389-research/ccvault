// ABOUTME: Codex source adapter that discovers and parses OpenAI Codex CLI session files.
// ABOUTME: Implements the SourceAdapter interface for ~/.codex/sessions/ JSONL sessions.

package codex

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
	"github.com/2389-research/ccvault/pkg/toolpayload"
)

func init() {
	adapter.Register("codex", func() adapter.SourceAdapter {
		return New()
	})
}

// Adapter implements adapter.SourceAdapter for Codex JSONL session files.
type Adapter struct{}

// New creates a new Codex adapter.
func New() *Adapter {
	return &Adapter{}
}

// Name returns the adapter's identifier.
func (a *Adapter) Name() string {
	return "codex"
}

// jsonlLine represents a single line in a Codex JSONL session file.
type jsonlLine struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

// sessionMetaPayload holds the fields from a session_meta payload.
type sessionMetaPayload struct {
	ID  string `json:"id"`
	CWD string `json:"cwd"`
	Git struct {
		Branch string `json:"branch"`
	} `json:"git"`
}

// messagePayload holds the fields from a response_item message payload.
type messagePayload struct {
	Type    string `json:"type"`
	Role    string `json:"role"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

// functionCallPayload holds the fields from a response_item tool-call payload.
//
// Codex writes two shapes. `function_call` carries its arguments as a JSON
// string in `arguments`; `custom_tool_call` — which is how apply_patch, the
// edit tool, arrives — carries a plain-text body in `input`. Both identify the
// call with `call_id`, and both are answered by a matching `*_output` payload
// carrying the same id.
type functionCallPayload struct {
	Type      string `json:"type"`
	Name      string `json:"name"`
	CallID    string `json:"call_id"`
	Arguments string `json:"arguments"`
	Input     string `json:"input"`
}

// payload returns the call's arguments, whichever field the shape used.
func (p functionCallPayload) payload() string {
	if p.Arguments != "" {
		return p.Arguments
	}
	return p.Input
}

// functionCallOutputPayload holds the fields from a response_item
// function_call_output or custom_tool_call_output payload.
type functionCallOutputPayload struct {
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

// turnContextPayload holds the fields from a turn_context payload.
type turnContextPayload struct {
	Model string `json:"model"`
}

// eventMsgPayload holds the fields from an event_msg payload.
//
// CachedInputTokens is the share of InputTokens that was served from the
// prompt cache rather than charged fresh — a subset, not a sibling: a real
// reading is input 8870 / cached 7040 / output 332 / total 9202, and
// 8870 + 332 = 9202. Claude Code's shape is the opposite, with the cache
// counters sitting beside input_tokens. See the split in the token_count
// branch of Parse for what that difference costs.
type eventMsgPayload struct {
	Type string `json:"type"`
	Info struct {
		TotalTokenUsage struct {
			InputTokens       int64 `json:"input_tokens"`
			CachedInputTokens int64 `json:"cached_input_tokens"`
			OutputTokens      int64 `json:"output_tokens"`
		} `json:"total_token_usage"`
	} `json:"info"`
}

// Discover scans the Codex sessions directory for JSONL session files and returns
// them as adapter.SessionFile entries. The root parameter should point to the
// Codex home directory (e.g. ~/.codex).
func (a *Adapter) Discover(root string) ([]adapter.SessionFile, error) {
	sessionsDir := filepath.Join(root, "sessions")
	if _, err := os.Stat(sessionsDir); os.IsNotExist(err) {
		return nil, nil
	}

	var files []adapter.SessionFile

	err := filepath.Walk(sessionsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".jsonl") {
			return nil
		}

		// Read the first line to get session_meta for project path
		projectPath, err := extractCWDFromFile(path)
		if err != nil {
			// Skip files we can't parse
			return nil
		}

		files = append(files, adapter.SessionFile{
			Path:        path,
			ProjectPath: projectPath,
			ModTime:     info.ModTime(),
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walking codex sessions: %w", err)
	}

	return files, nil
}

// extractCWDFromFile reads just the first lines of a Codex session file to find
// the session_meta line and extract the CWD as the project path.
func extractCWDFromFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	reader := bufio.NewReader(f)
	for {
		raw, err := adapter.ReadLine(reader)
		if len(raw) > 0 {
			var line jsonlLine
			if jerr := json.Unmarshal(raw, &line); jerr == nil && line.Type == "session_meta" {
				var meta sessionMetaPayload
				if perr := json.Unmarshal(line.Payload, &meta); perr != nil {
					return "", fmt.Errorf("parsing session_meta payload: %w", perr)
				}
				return meta.CWD, nil
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return "", fmt.Errorf("reading %s: %w", path, err)
		}
	}

	return "", fmt.Errorf("no session_meta found in %s", path)
}

// Parse reads a Codex JSONL session file and converts it to adapter.ParsedSession.
func (a *Adapter) Parse(path string) (*adapter.ParsedSession, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening codex session: %w", err)
	}
	defer func() { _ = f.Close() }()

	return a.ParseTranscript(path, f)
}

// ParseTranscript reads the same transcript from a reader rather than from the
// file at originalPath — see adapter.TranscriptReparser for why that is worth
// having.
//
// A codex rollout answers everything from its own lines: the session id, cwd
// and git branch arrive in the session_meta line, so originalPath is unused
// here and kept only to satisfy the interface.
//
// Worth knowing before trusting this against stored lines: codex reports its
// token counts on token_count *event* lines, and the loop below folds them
// into the preceding assistant turn rather than making turns of their own. A
// re-derivation reading turns.raw_json therefore sees no token counts at all,
// which is why the backfill refuses any session whose re-derived total falls
// below the total already stored.
func (a *Adapter) ParseTranscript(originalPath string, r io.Reader) (*adapter.ParsedSession, error) {
	_ = originalPath

	reader := bufio.NewReader(r)

	var (
		sessionID   string
		projectPath string
		gitBranch   string
		model       string
		startedAt   time.Time
		endedAt     time.Time

		turns []adapter.ParsedTurn

		// Track the current assistant turn so we can attach tool uses and tokens
		lastAssistantIdx = -1

		// Track last total token counts to compute per-turn deltas
		prevInputTokens  int64
		prevCachedTokens int64
		prevOutputTokens int64

		// Deltas that arrived before there was an assistant turn to carry
		// them. Codex only materialises a turn for a response_item/message,
		// so a model response made entirely of tool calls produces none — and
		// a run that works through tools before it says anything is billed the
		// whole time. These used to be dropped (#166): 20 of 148 real rollouts
		// on the author's machine lost deltas that way, the worst of them
		// 36.7M of 36.9M tokens. They are held until the first assistant turn
		// appears and applied to it, which is lumpy per turn and exact per
		// session — and the session row is where these counters are read.
		pendingInputTokens  int64
		pendingCachedTokens int64
		pendingOutputTokens int64

		// A call and the output answering it are separate lines joined by
		// call_id, so both halves are collected as they come and matched after
		// the file is read. Resolving inline would only work while outputs
		// follow their calls, which is true of the files on disk today and is
		// not something the format promises.
		outputs   = make(map[string]string)
		callSites []codexCallSite

		// The first timestamp string the layout above could not read, kept
		// verbatim. A count says something is unreadable; the sample says
		// which layout is missing (#146).
		unreadableSample string
	)

	turnCounter := 0

	for {
		raw, readErr := adapter.ReadLine(reader)
		if len(raw) == 0 {
			if readErr != nil {
				if errors.Is(readErr, io.EOF) {
					break
				}
				return nil, fmt.Errorf("reading codex session: %w", readErr)
			}
			continue
		}

		var line jsonlLine
		if err := json.Unmarshal(raw, &line); err != nil {
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				return nil, fmt.Errorf("reading codex session: %w", readErr)
			}
			if errors.Is(readErr, io.EOF) {
				break
			}
			continue
		}

		// Codex writes RFC3339Nano and nothing else, so this stays a
		// one-layout list rather than joining pkg/parser's or jeff's — those
		// read different tools' transcripts and change independently of this
		// one. What was missing is a signal when the layout stops matching:
		// a line codex wrote in some other shape becomes a turn at the zero
		// time, which sorts to the very start of the archive and escapes
		// every date filter while looking like a real epoch value (#146).
		ts, tsErr := time.Parse(time.RFC3339Nano, line.Timestamp)
		if tsErr != nil && unreadableSample == "" && line.Timestamp != "" {
			unreadableSample = line.Timestamp
		}

		// Track session time range
		if !ts.IsZero() {
			if startedAt.IsZero() || ts.Before(startedAt) {
				startedAt = ts
			}
			if ts.After(endedAt) {
				endedAt = ts
			}
		}

		switch line.Type {
		case "session_meta":
			// First session_meta wins. A rollout that was forked — resumed, or
			// spawned as a subagent — opens with its own session_meta and then
			// replays the ancestor's, which names the ancestor's id and cwd.
			// Taking the last one gave the rollout the ancestor's identity: on
			// the author's machine 12 distinct rollouts resolved to one
			// ancestor id, so syncing them wrote 12 files into a single session
			// row and the last file synced overwrote the rest. Eleven rollouts
			// left no row at all — not undercounted, absent (#166).
			//
			// Discover already reads only the first session_meta and keys the
			// file on that id, so this is also what makes Parse agree with it.
			if sessionID == "" {
				var meta sessionMetaPayload
				if err := json.Unmarshal(line.Payload, &meta); err == nil {
					sessionID = meta.ID
					projectPath = meta.CWD
					gitBranch = meta.Git.Branch
				}
			}

		case "turn_context":
			var tc turnContextPayload
			if err := json.Unmarshal(line.Payload, &tc); err == nil {
				if tc.Model != "" {
					model = tc.Model
				}
			}

		case "response_item":
			// Peek at the payload type to decide how to handle
			var peek struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(line.Payload, &peek); err != nil {
				continue
			}

			switch peek.Type {
			case "message":
				var msg messagePayload
				if err := json.Unmarshal(line.Payload, &msg); err != nil {
					continue
				}

				// Skip developer messages (system instructions)
				if msg.Role == "developer" {
					continue
				}

				// Extract text from content blocks
				var textParts []string
				for _, block := range msg.Content {
					if block.Type == "input_text" || block.Type == "output_text" {
						if block.Text != "" {
							textParts = append(textParts, block.Text)
						}
					}
				}

				turnCounter++
				turn := adapter.ParsedTurn{
					ID:        fmt.Sprintf("%s-turn-%d", sessionID, turnCounter),
					Type:      msg.Role,
					Timestamp: ts,
					Content:   strings.Join(textParts, "\n\n"),
					RawJSON:   json.RawMessage(adapter.MakeCopy(raw)),
				}

				turns = append(turns, turn)

				if msg.Role == "assistant" {
					lastAssistantIdx = len(turns) - 1
					// Spend billed before any turn existed to carry it lands
					// on the first assistant turn to appear — the turn it paid
					// for. Only the first one ever finds a pending balance.
					if pendingInputTokens != 0 || pendingCachedTokens != 0 || pendingOutputTokens != 0 {
						turns[lastAssistantIdx].InputTokens += pendingInputTokens
						turns[lastAssistantIdx].CacheReadTokens += pendingCachedTokens
						turns[lastAssistantIdx].OutputTokens += pendingOutputTokens
						pendingInputTokens, pendingCachedTokens, pendingOutputTokens = 0, 0, 0
					}
				}

			case "function_call", "custom_tool_call":
				var fc functionCallPayload
				if err := json.Unmarshal(line.Payload, &fc); err != nil {
					continue
				}

				// Attach to the last assistant turn
				if lastAssistantIdx >= 0 && lastAssistantIdx < len(turns) {
					input := fc.payload()
					turns[lastAssistantIdx].ToolUses = append(
						turns[lastAssistantIdx].ToolUses,
						adapter.ParsedToolUse{
							ToolName:    fc.Name,
							ToolUseID:   fc.CallID,
							InputJSON:   input,
							InputLength: len(input),
						},
					)
					if fc.CallID != "" {
						callSites = append(callSites, codexCallSite{
							callID:  fc.CallID,
							turnIdx: lastAssistantIdx,
							useIdx:  len(turns[lastAssistantIdx].ToolUses) - 1,
						})
					}
				}

			case "function_call_output", "custom_tool_call_output":
				var out functionCallOutputPayload
				if err := json.Unmarshal(line.Payload, &out); err != nil {
					continue
				}
				if out.CallID != "" {
					outputs[out.CallID] = out.Output
				}

			case "reasoning":
				// Skip reasoning blocks
				continue
			}

		case "event_msg":
			var evt eventMsgPayload
			if err := json.Unmarshal(line.Payload, &evt); err != nil {
				continue
			}

			if evt.Type == "token_count" {
				// Compute delta from previous total.
				//
				// total_token_usage is cumulative but it restarts inside a
				// single rollout when the context is cleared — a real reading
				// drops from 767,935 to 40,766 mid-file. A counter that went
				// down has restarted, so the new reading is itself the delta;
				// subtracting the old total from it produced a negative delta
				// that was then added to the turn, and one synced session held
				// input_tokens = -399,066. Each counter is tested on its own
				// because they do not have to reset together.
				deltaInput := delta(evt.Info.TotalTokenUsage.InputTokens, prevInputTokens)
				deltaCached := delta(evt.Info.TotalTokenUsage.CachedInputTokens, prevCachedTokens)
				deltaOutput := delta(evt.Info.TotalTokenUsage.OutputTokens, prevOutputTokens)
				prevInputTokens = evt.Info.TotalTokenUsage.InputTokens
				prevCachedTokens = evt.Info.TotalTokenUsage.CachedInputTokens
				prevOutputTokens = evt.Info.TotalTokenUsage.OutputTokens

				// cached_input_tokens is part of input_tokens, so the cached
				// share is moved rather than added: InputTokens becomes the
				// fresh input and CacheReadTokens the part served from cache.
				// Reporting the raw cached figure as well would inflate the
				// session's total by the amount it reports, because the
				// session row sums all four counters (#155).
				//
				// Codex reports no cache-creation figure, so CacheWriteTokens
				// stays zero for this source — an absence, not a zero.
				if deltaCached > deltaInput {
					// A cached share larger than the input it is a share of.
					// Should not happen; if it does, leave the input delta
					// whole rather than deriving a negative fresh-input count
					// from it. (A negative delta can no longer reach here —
					// see the reset handling above.)
					deltaCached = 0
				}

				// Attach to the last assistant turn, or hold the deltas until
				// one exists. Dropping them was #166.
				if lastAssistantIdx >= 0 && lastAssistantIdx < len(turns) {
					turns[lastAssistantIdx].InputTokens += deltaInput - deltaCached
					turns[lastAssistantIdx].CacheReadTokens += deltaCached
					turns[lastAssistantIdx].OutputTokens += deltaOutput
				} else {
					pendingInputTokens += deltaInput - deltaCached
					pendingCachedTokens += deltaCached
					pendingOutputTokens += deltaOutput
				}
			}
		}

		if errors.Is(readErr, io.EOF) {
			break
		}
	}

	// A rollout that never produced an assistant turn still has nowhere exact
	// to put its spend, so the last turn carries it: the statement "by the end
	// of this session, this much had been spent" is the least wrong available,
	// and it keeps the session total right, which is what #166 is about. None
	// of the 148 real rollouts on the author's machine is this shape — every
	// one with a token_count event has at least one assistant message — so
	// this is the floor under the format changing, not a path the data takes.
	if len(turns) > 0 && (pendingInputTokens != 0 || pendingCachedTokens != 0 || pendingOutputTokens != 0) {
		last := len(turns) - 1
		turns[last].InputTokens += pendingInputTokens
		turns[last].CacheReadTokens += pendingCachedTokens
		turns[last].OutputTokens += pendingOutputTokens
	}

	attachCodexOutputs(turns, callSites, outputs)

	metadata := make(map[string]any)
	adapter.ReportUnreadableTimestamps(metadata, turns, unreadableSample)

	return &adapter.ParsedSession{
		ID:          sessionID,
		ProjectPath: projectPath,
		DisplayName: displayNameFromPath(projectPath),
		Turns:       turns,
		Model:       model,
		GitBranch:   gitBranch,
		StartedAt:   startedAt,
		EndedAt:     endedAt,
		SourceName:  "codex",
		Metadata:    metadata,
	}, nil
}

// delta returns how much a cumulative counter advanced between two readings.
//
// A reading lower than the one before it means the counter restarted, which
// codex does mid-rollout when the context is cleared. The new reading is then
// the whole of the advance since the restart, so it is returned as-is rather
// than differenced against a total that no longer applies.
func delta(current, previous int64) int64 {
	if current < previous {
		return current
	}
	return current - previous
}

// codexCallSite records where a tool use landed so the output answering it can
// be attached once the whole file has been read.
type codexCallSite struct {
	callID  string
	turnIdx int
	useIdx  int
}

// attachCodexOutputs matches each recorded call to the output carrying its
// call_id and applies the storage policy to it.
func attachCodexOutputs(turns []adapter.ParsedTurn, sites []codexCallSite, outputs map[string]string) {
	for _, site := range sites {
		output, ok := outputs[site.callID]
		if !ok {
			continue
		}
		tu := &turns[site.turnIdx].ToolUses[site.useIdx]
		decided := toolpayload.ResultFromText(tu.ToolName, output)
		tu.HasResult = true
		tu.ResultContent = decided.Content
		tu.ResultLength = decided.Length
		tu.ResultOmittedReason = decided.OmitReason
	}
}

// displayNameFromPath returns a shortened display name from a project path.
func displayNameFromPath(projectPath string) string {
	if projectPath == "" {
		return ""
	}
	home, _ := os.UserHomeDir()
	if strings.HasPrefix(projectPath, home) {
		projectPath = "~" + projectPath[len(home):]
	}
	parts := strings.Split(projectPath, "/")
	if len(parts) <= 3 {
		return projectPath
	}
	return strings.Join(parts[len(parts)-3:], "/")
}
