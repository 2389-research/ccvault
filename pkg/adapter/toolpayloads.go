// ABOUTME: Conversions between adapter.ParsedToolUse and models.ToolUse, in one place rather than three.
// ABOUTME: Every adapter and the sync layer go through these so a new payload field cannot be dropped in transit.

package adapter

import (
	"time"

	"github.com/2389-research/ccvault/pkg/models"
)

// ParsedToolUseFromModel converts a tool use the parser produced into the
// adapter contract's shape. Used by the sources that parse Claude Code JSONL
// (claude-code, nanoclaw) and get models.ToolUse values back from pkg/parser.
//
// This conversion and ToolUseFromParsed exist as functions rather than as
// struct literals at each call site because the fields are copied in three
// places — two adapters and the sync layer — and a field missed in any one of
// them is not a compile error, just a column that is quietly always empty.
// TestToolUseRoundTrip walks the struct by reflection so adding a field
// without threading it through fails the build's tests instead.
func ParsedToolUseFromModel(tu models.ToolUse) ParsedToolUse {
	return ParsedToolUse{
		ToolName:            tu.ToolName,
		FilePath:            tu.FilePath,
		ToolUseID:           tu.ToolUseID,
		InputJSON:           tu.InputJSON,
		InputLength:         tu.InputLength,
		HasResult:           tu.HasResult,
		ResultContent:       tu.ResultContent,
		ResultLength:        tu.ResultLength,
		ResultOmittedReason: tu.ResultOmittedReason,
	}
}

// ToolUseFromParsed converts an adapter's tool use into the row the database
// stores, stamping on the identity the adapter contract carries per turn
// rather than per tool use.
func ToolUseFromParsed(p ParsedToolUse, turnID, sessionID string, timestamp time.Time) models.ToolUse {
	return models.ToolUse{
		TurnID:              turnID,
		SessionID:           sessionID,
		Timestamp:           timestamp,
		ToolName:            p.ToolName,
		FilePath:            p.FilePath,
		ToolUseID:           p.ToolUseID,
		InputJSON:           p.InputJSON,
		InputLength:         p.InputLength,
		HasResult:           p.HasResult,
		ResultContent:       p.ResultContent,
		ResultLength:        p.ResultLength,
		ResultOmittedReason: p.ResultOmittedReason,
	}
}
