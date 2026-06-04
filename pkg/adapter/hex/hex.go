// ABOUTME: Hex source adapter that discovers and parses Hex agentic loop session files.
// ABOUTME: Implements the SourceAdapter interface for discovering and parsing Hex JSON sessions.

package hex

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/2389-research/ccvault/pkg/adapter"
)

// sessionIDPrefix namespaces Hex session IDs so they can't collide with other
// sources' IDs on the global sessions primary key.
const sessionIDPrefix = "hex:"

func init() {
	adapter.Register("hex", func() adapter.SourceAdapter {
		return New()
	})
}

// hexSession represents the top-level JSON structure of a Hex session file.
type hexSession struct {
	ID        string       `json:"id"`
	Title     string       `json:"title"`
	CreatedAt time.Time    `json:"created_at"`
	UpdatedAt time.Time    `json:"updated_at"`
	Messages  []hexMessage `json:"messages"`
	Favorite  bool         `json:"favorite"`
}

// hexMessage represents a single message within a Hex session.
type hexMessage struct {
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	Timestamp time.Time `json:"timestamp"`
}

// Adapter implements adapter.SourceAdapter for Hex JSON session files.
type Adapter struct{}

// New creates a new Hex adapter.
func New() *Adapter {
	return &Adapter{}
}

// Name returns the adapter's identifier.
func (a *Adapter) Name() string {
	return "hex"
}

// Discover scans a Hex sessions directory for JSON session files and returns
// them as adapter.SessionFile entries.
func (a *Adapter) Discover(root string) ([]adapter.SessionFile, error) {
	sessionsDir := filepath.Join(root, "sessions")

	info, err := os.Stat(sessionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat sessions dir: %w", err)
	}
	if !info.IsDir() {
		return nil, nil
	}

	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		return nil, fmt.Errorf("read sessions dir: %w", err)
	}

	// Hex sessions carry no filesystem CWD, so we key the project by install
	// root. Distinct Hex roots stay distinct; a single install collapses to
	// one project.
	projectPath := projectPathForRoot(root)

	var files []adapter.SessionFile
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}

		fi, err := entry.Info()
		if err != nil {
			continue
		}

		files = append(files, adapter.SessionFile{
			Path:        filepath.Join(sessionsDir, entry.Name()),
			ProjectPath: projectPath,
			ModTime:     fi.ModTime(),
		})
	}

	return files, nil
}

// projectPathForRoot returns a stable per-install project key for a Hex root.
func projectPathForRoot(root string) string {
	if root == "" {
		return "hex"
	}
	if abs, err := filepath.Abs(root); err == nil {
		return "hex:" + abs
	}
	return "hex:" + root
}

// Parse reads a Hex JSON session file and converts it to adapter.ParsedSession.
func (a *Adapter) Parse(path string) (*adapter.ParsedSession, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read session file: %w", err)
	}

	var session hexSession
	if err := json.Unmarshal(data, &session); err != nil {
		return nil, fmt.Errorf("unmarshal session: %w", err)
	}

	namespacedID := ""
	if session.ID != "" {
		namespacedID = sessionIDPrefix + session.ID
	}

	turns := make([]adapter.ParsedTurn, len(session.Messages))
	var prevTurnID string

	for i, msg := range session.Messages {
		turnID := fmt.Sprintf("%s-%d", namespacedID, i)

		rawJSON, err := json.Marshal(msg)
		if err != nil {
			return nil, fmt.Errorf("marshal message %d: %w", i, err)
		}

		turns[i] = adapter.ParsedTurn{
			ID:        turnID,
			ParentID:  prevTurnID,
			Type:      msg.Role,
			Timestamp: msg.Timestamp,
			Content:   msg.Content,
			RawJSON:   rawJSON,
		}

		prevTurnID = turnID
	}

	parsed := &adapter.ParsedSession{
		ID: namespacedID,
		// ProjectPath left empty so sync falls back to the per-root value set
		// in Discover().
		DisplayName: "Hex",
		Turns:       turns,
		StartedAt:   session.CreatedAt,
		EndedAt:     session.UpdatedAt,
		SourceName:  "hex",
		Metadata:    make(map[string]any),
	}

	return parsed, nil
}
