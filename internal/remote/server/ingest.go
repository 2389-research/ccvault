// ABOUTME: Ingest handler — buffers per-session records until session_end, then upserts
// ABOUTME: Reuses the same DB helpers as local sync; stamps pushed_by from the SSH identity

package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/2389-research/ccvault/internal/remote/protocol"
	"github.com/2389-research/ccvault/pkg/models"
)

type pending struct {
	session  *models.Session
	turns    []models.Turn
	toolUses []models.ToolUse
}

func handleIngest(ctx HandlerCtx) int {
	dec := json.NewDecoder(ctx.Stdin)
	buffers := make(map[string]*pending)

	for {
		var msg protocol.IngestMessage
		if err := dec.Decode(&msg); err != nil {
			if errors.Is(err, io.EOF) {
				return 0
			}
			_, _ = fmt.Fprintf(ctx.Stderr, "decode: %v\n", err)
			return 1
		}
		switch msg.Kind {
		case protocol.KindSession:
			if msg.Session == nil {
				_, _ = fmt.Fprintln(ctx.Stderr, "session kind with nil Session")
				return 1
			}
			msg.Session.PushedBy = ctx.Identity
			buffers[msg.Session.ID] = &pending{session: msg.Session}
		case protocol.KindTurn:
			if msg.Turn == nil {
				_, _ = fmt.Fprintln(ctx.Stderr, "turn kind with nil Turn")
				return 1
			}
			p := buffers[msg.Turn.SessionID]
			if p == nil {
				_, _ = fmt.Fprintf(ctx.Stderr, "turn for unknown session %s\n", msg.Turn.SessionID)
				return 1
			}
			p.turns = append(p.turns, *msg.Turn)
		case protocol.KindToolUse:
			if msg.ToolUse == nil {
				_, _ = fmt.Fprintln(ctx.Stderr, "tool_use kind with nil ToolUse")
				return 1
			}
			p := buffers[msg.ToolUse.SessionID]
			if p == nil {
				_, _ = fmt.Fprintf(ctx.Stderr, "tool_use for unknown session %s\n", msg.ToolUse.SessionID)
				return 1
			}
			p.toolUses = append(p.toolUses, *msg.ToolUse)
		case protocol.KindSessionEnd:
			p := buffers[msg.SessionID]
			if p == nil {
				_, _ = fmt.Fprintf(ctx.Stderr, "session_end for unknown session %s\n", msg.SessionID)
				return 1
			}
			if err := commitSession(ctx, p); err != nil {
				_, _ = fmt.Fprintf(ctx.Stderr, "commit %s: %v\n", msg.SessionID, err)
				return 1
			}
			delete(buffers, msg.SessionID)
		default:
			_, _ = fmt.Fprintf(ctx.Stderr, "unknown message kind: %q\n", msg.Kind)
			return 1
		}
	}
}

func commitSession(ctx HandlerCtx, p *pending) error {
	d := ctx.Server.db
	return d.WithTx(func(tx *sql.Tx) error {
		proj := &models.Project{
			Path:           p.session.ProjectPath,
			DisplayName:    p.session.ProjectPath,
			FirstSeenAt:    p.session.StartedAt,
			LastActivityAt: p.session.EndedAt,
			SessionCount:   1,
			TotalTokens:    p.session.TotalTokens(),
			Source:         p.session.Source,
		}
		if err := d.UpsertProjectTx(tx, proj); err != nil {
			return fmt.Errorf("upsert project: %w", err)
		}
		p.session.ProjectID = proj.ID

		if err := d.DeleteTurnsForSessionTx(tx, p.session.ID); err != nil {
			return err
		}
		if err := d.DeleteToolUsesForSessionTx(tx, p.session.ID); err != nil {
			return err
		}
		if err := d.UpsertSessionTx(tx, p.session); err != nil {
			return err
		}
		if err := d.InsertTurnsTx(tx, p.turns); err != nil {
			return err
		}
		if len(p.toolUses) > 0 {
			if err := d.InsertToolUsesTx(tx, p.toolUses); err != nil {
				return err
			}
		}
		return nil
	})
}
