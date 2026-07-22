// ABOUTME: Streams pending sessions to the remote via the `ingest` command
// ABOUTME: Updates remote_push_state on success so subsequent pushes are incremental

package client

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/2389-research/ccvault/internal/db"
	"github.com/2389-research/ccvault/internal/remote/protocol"
)

// PushStats records what a Push call did.
type PushStats struct {
	SessionsPushed int
	TurnsPushed    int
	ToolUsesPushed int
}

// Push streams every session in `database` that's pending (per remote_push_state)
// to the remote and records success. If dryRun is true, computes and returns the
// stats but sends nothing.
func Push(c *Client, database *db.DB, remoteName string, dryRun bool) (*PushStats, error) {
	ids, err := database.SessionsPendingPush(remoteName)
	if err != nil {
		return nil, fmt.Errorf("pending push: %w", err)
	}

	stats := &PushStats{}
	if dryRun {
		stats.SessionsPushed = len(ids)
		return stats, nil
	}
	if len(ids) == 0 {
		return stats, nil
	}

	pr, pw := io.Pipe()
	var runErr error
	go func() {
		defer func() { _ = pw.Close() }()
		enc := json.NewEncoder(pw)
		for _, id := range ids {
			sess, err := database.GetSession(id)
			if err != nil {
				runErr = fmt.Errorf("get session %s: %w", id, err)
				return
			}
			if sess == nil {
				runErr = fmt.Errorf("session %s vanished mid-push", id)
				return
			}
			if err := enc.Encode(protocol.IngestMessage{Kind: protocol.KindSession, Session: sess}); err != nil {
				runErr = err
				return
			}
			turns, err := database.GetTurns(id)
			if err != nil {
				runErr = err
				return
			}
			for i := range turns {
				if err := enc.Encode(protocol.IngestMessage{Kind: protocol.KindTurn, Turn: &turns[i]}); err != nil {
					runErr = err
					return
				}
				stats.TurnsPushed++
			}
			// NOTE: tool_uses push deferred (needs GetToolUsesForSession helper).
			if err := enc.Encode(protocol.IngestMessage{Kind: protocol.KindSessionEnd, SessionID: id}); err != nil {
				runErr = err
				return
			}
			stats.SessionsPushed++
			_ = database.RecordPush(remoteName, id, sess.EndedAt)
		}
	}()

	reader, _, err := c.Run("ingest", pr)
	if err != nil {
		return nil, err
	}
	_, _ = io.Copy(io.Discard, reader)
	_ = reader.Close()
	if runErr != nil {
		return nil, runErr
	}
	return stats, nil
}
