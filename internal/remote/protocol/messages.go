// ABOUTME: Wire message types shared between ccvault client and ccvaultd server
// ABOUTME: Newline-delimited JSON over SSH channels; one message per line

package protocol

import (
	"github.com/2389-research/ccvault/pkg/models"
)

// SchemaVersion is bumped whenever the wire format changes incompatibly.
// The `version` command exposes this so clients can refuse to push to older servers.
const SchemaVersion = 1

// Kind discriminates message types on the ingest stream.
type Kind string

const (
	KindSession    Kind = "session"
	KindTurn       Kind = "turn"
	KindToolUse    Kind = "tool_use"
	KindSessionEnd Kind = "session_end"
)

// IngestMessage is one line of the client → server ingest stream.
// Exactly one of Session/Turn/ToolUse is populated per message; SessionID is
// used for KindSessionEnd (the commit marker).
type IngestMessage struct {
	Kind      Kind            `json:"kind"`
	Session   *models.Session `json:"session,omitempty"`
	Turn      *models.Turn    `json:"turn,omitempty"`
	ToolUse   *models.ToolUse `json:"tool_use,omitempty"`
	SessionID string          `json:"session_id,omitempty"`
}

// VersionResponse is what the server returns for the `version` command.
type VersionResponse struct {
	SchemaVersion int    `json:"schema_version"`
	BuildVersion  string `json:"build_version"`
}

// SearchResult mirrors models.SearchResult but is used as the query response type.
// Streamed one per line on the search command's stdout.
type SearchResult = models.SearchResult
