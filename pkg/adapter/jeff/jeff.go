// ABOUTME: Jeff source adapter that discovers and parses Jeff (GSuite agentic loop) session files.
// ABOUTME: Implements the SourceAdapter interface for ~/.jeff/sessions/ JSONL sessions.

package jeff

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
)

// sessionIDPrefix namespaces Jeff session IDs to prevent collisions with other
// sources' IDs on the global sessions primary key.
const sessionIDPrefix = "jeff:"

func init() {
	adapter.Register("jeff", func() adapter.SourceAdapter {
		return New()
	})
}

// Adapter implements adapter.SourceAdapter for Jeff JSONL session files.
type Adapter struct{}

// New creates a new Jeff adapter.
func New() *Adapter {
	return &Adapter{}
}

// Name returns the adapter's identifier.
func (a *Adapter) Name() string {
	return "jeff"
}

// jsonlLine represents a single line in a Jeff JSONL session file.
type jsonlLine struct {
	Timestamp string          `json:"timestamp"`
	EntryType string          `json:"entry_type"`
	ConvID    string          `json:"conversation_id"`
	Data      json.RawMessage `json:"data"`
}

// sessionStartData holds the fields from a session_start entry's data.
type sessionStartData struct {
	Model     string `json:"model"`
	SessionID string `json:"session_id"`
}

// messageData holds content from user_message and assistant_message entries.
type messageData struct {
	Content string `json:"content"`
}

// toolRequestData holds the fields from a tool_request entry's data.
type toolRequestData struct {
	ToolName string `json:"tool_name"`
	ToolID   string `json:"tool_id"`
}

// Discover scans the Jeff sessions directory for JSONL session files and returns
// them as adapter.SessionFile entries. The root parameter should point to the
// Jeff home directory (e.g. ~/.jeff).
func (a *Adapter) Discover(root string) ([]adapter.SessionFile, error) {
	sessionsDir := filepath.Join(root, "sessions")
	if _, err := os.Stat(sessionsDir); os.IsNotExist(err) {
		return nil, nil
	}

	// Jeff sessions have no per-session CWD, so we key the project on the
	// install root. Multiple Jeff installs in different roots stay distinct;
	// a single install collapses to one project (the intended behaviour).
	projectPath := projectPathForRoot(root)

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

		files = append(files, adapter.SessionFile{
			Path:        path,
			ProjectPath: projectPath,
			ModTime:     info.ModTime(),
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walking jeff sessions: %w", err)
	}

	return files, nil
}

// projectPathForRoot returns a stable per-install project key for a Jeff root.
func projectPathForRoot(root string) string {
	if root == "" {
		return "jeff"
	}
	if abs, err := filepath.Abs(root); err == nil {
		return "jeff:" + abs
	}
	return "jeff:" + root
}

// Parse reads a Jeff JSONL session file and converts it to adapter.ParsedSession.
func (a *Adapter) Parse(path string) (*adapter.ParsedSession, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening jeff session: %w", err)
	}
	defer func() { _ = f.Close() }()

	reader := bufio.NewReader(f)

	var (
		convID    string
		model     string
		startedAt time.Time
		endedAt   time.Time
		hasError  bool

		turns []adapter.ParsedTurn

		// Track the current assistant turn so we can attach tool uses
		lastAssistantIdx = -1
	)

	turnCounter := 0

	for {
		raw, readErr := adapter.ReadLine(reader)
		if len(raw) == 0 {
			if readErr != nil {
				if errors.Is(readErr, io.EOF) {
					break
				}
				return nil, fmt.Errorf("reading jeff session: %w", readErr)
			}
			continue
		}

		var line jsonlLine
		if err := json.Unmarshal(raw, &line); err != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			continue
		}

		// Recover the conversation ID from any line that carries it, not just
		// session_start — a corrupt or missing session_start record would
		// otherwise leave the session unidentifiable and break dedupe.
		if convID == "" && line.ConvID != "" {
			convID = line.ConvID
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

		switch line.EntryType {
		case "session_start":
			var sd sessionStartData
			if err := json.Unmarshal(line.Data, &sd); err == nil {
				if sd.Model != "" {
					model = sd.Model
				}
			}

		case "user_message":
			var md messageData
			if err := json.Unmarshal(line.Data, &md); err != nil {
				continue
			}

			turnCounter++
			turn := adapter.ParsedTurn{
				ID:        fmt.Sprintf("%s%s-turn-%d", sessionIDPrefix, convID, turnCounter),
				Type:      "user",
				Timestamp: ts,
				Content:   md.Content,
				RawJSON:   json.RawMessage(adapter.MakeCopy(raw)),
			}
			turns = append(turns, turn)

		case "assistant_message":
			var md messageData
			if err := json.Unmarshal(line.Data, &md); err != nil {
				continue
			}

			turnCounter++
			turn := adapter.ParsedTurn{
				ID:        fmt.Sprintf("%s%s-turn-%d", sessionIDPrefix, convID, turnCounter),
				Type:      "assistant",
				Timestamp: ts,
				Content:   md.Content,
				RawJSON:   json.RawMessage(adapter.MakeCopy(raw)),
			}
			turns = append(turns, turn)
			lastAssistantIdx = len(turns) - 1

		case "tool_request":
			var tr toolRequestData
			if err := json.Unmarshal(line.Data, &tr); err != nil {
				continue
			}

			// Attach to the last assistant turn
			if lastAssistantIdx >= 0 && lastAssistantIdx < len(turns) {
				turns[lastAssistantIdx].ToolUses = append(
					turns[lastAssistantIdx].ToolUses,
					adapter.ParsedToolUse{ToolName: tr.ToolName},
				)
			}

		case "error":
			hasError = true
		}

		if errors.Is(readErr, io.EOF) {
			break
		}
	}

	metadata := make(map[string]any)
	if hasError {
		metadata["has_error"] = true
	}

	sessionID := ""
	if convID != "" {
		sessionID = sessionIDPrefix + convID
	}

	return &adapter.ParsedSession{
		ID: sessionID,
		// ProjectPath left empty so the sync layer falls back to the per-root
		// value set in Discover(); this keeps the source of truth in one place.
		DisplayName: "Jeff",
		Turns:       turns,
		Model:       model,
		StartedAt:   startedAt,
		EndedAt:     endedAt,
		SourceName:  "jeff",
		Metadata:    metadata,
	}, nil
}
