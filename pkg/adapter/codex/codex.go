// ABOUTME: Codex source adapter that discovers and parses OpenAI Codex CLI session files.
// ABOUTME: Implements the SourceAdapter interface for ~/.codex/sessions/ JSONL sessions.

package codex

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/2389-research/ccvault/pkg/adapter"
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

// functionCallPayload holds the fields from a response_item function_call payload.
type functionCallPayload struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// turnContextPayload holds the fields from a turn_context payload.
type turnContextPayload struct {
	Model string `json:"model"`
}

// eventMsgPayload holds the fields from an event_msg payload.
type eventMsgPayload struct {
	Type string `json:"type"`
	Info struct {
		TotalTokenUsage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
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

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		var line jsonlLine
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			continue
		}
		if line.Type == "session_meta" {
			var meta sessionMetaPayload
			if err := json.Unmarshal(line.Payload, &meta); err != nil {
				return "", fmt.Errorf("parsing session_meta payload: %w", err)
			}
			return meta.CWD, nil
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

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

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
		prevOutputTokens int64
	)

	turnCounter := 0

	for scanner.Scan() {
		raw := scanner.Bytes()

		var line jsonlLine
		if err := json.Unmarshal(raw, &line); err != nil {
			continue
		}

		ts, _ := time.Parse(time.RFC3339Nano, line.Timestamp)

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
			var meta sessionMetaPayload
			if err := json.Unmarshal(line.Payload, &meta); err == nil {
				sessionID = meta.ID
				projectPath = meta.CWD
				gitBranch = meta.Git.Branch
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
					RawJSON:   json.RawMessage(makeCopy(raw)),
				}

				turns = append(turns, turn)

				if msg.Role == "assistant" {
					lastAssistantIdx = len(turns) - 1
				}

			case "function_call":
				var fc functionCallPayload
				if err := json.Unmarshal(line.Payload, &fc); err != nil {
					continue
				}

				// Attach to the last assistant turn
				if lastAssistantIdx >= 0 && lastAssistantIdx < len(turns) {
					turns[lastAssistantIdx].ToolUses = append(
						turns[lastAssistantIdx].ToolUses,
						adapter.ParsedToolUse{ToolName: fc.Name},
					)
				}

			case "reasoning", "function_call_output":
				// Skip reasoning blocks and function call outputs
				continue
			}

		case "event_msg":
			var evt eventMsgPayload
			if err := json.Unmarshal(line.Payload, &evt); err != nil {
				continue
			}

			if evt.Type == "token_count" {
				// Compute delta from previous total
				deltaInput := evt.Info.TotalTokenUsage.InputTokens - prevInputTokens
				deltaOutput := evt.Info.TotalTokenUsage.OutputTokens - prevOutputTokens
				prevInputTokens = evt.Info.TotalTokenUsage.InputTokens
				prevOutputTokens = evt.Info.TotalTokenUsage.OutputTokens

				// Attach to the last assistant turn
				if lastAssistantIdx >= 0 && lastAssistantIdx < len(turns) {
					turns[lastAssistantIdx].InputTokens += deltaInput
					turns[lastAssistantIdx].OutputTokens += deltaOutput
				}
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanning codex session: %w", err)
	}

	return &adapter.ParsedSession{
		ID:          sessionID,
		ProjectPath: projectPath,
		Turns:       turns,
		Model:       model,
		GitBranch:   gitBranch,
		StartedAt:   startedAt,
		EndedAt:     endedAt,
		SourceName:  "codex",
		Metadata:    make(map[string]any),
	}, nil
}

// makeCopy returns a copy of the byte slice to avoid referencing the scanner buffer.
func makeCopy(b []byte) []byte {
	c := make([]byte, len(b))
	copy(c, b)
	return c
}
