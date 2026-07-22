// ABOUTME: Routes an SSH channel's command string to the right handler
// ABOUTME: Handlers get the channel (stdin/stdout) and the connection identity

package server

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/2389-research/ccvault/internal/remote/protocol"
)

// HandlerCtx is passed to each command handler.
type HandlerCtx struct {
	Server   *Server
	Identity string
	Stdin    io.Reader
	Stdout   io.Writer
	Stderr   io.Writer
	Args     string
}

// dispatch parses the SSH command string and invokes the matching handler.
// Returns the exit code.
func (s *Server) dispatch(command string, ctx HandlerCtx) int {
	name, args := splitCommand(command)
	ctx.Args = args
	switch name {
	case "version":
		return handleVersion(ctx)
	case "ingest":
		return handleIngest(ctx)
	case "search":
		return handleSearch(ctx)
	case "sessions":
		return handleSessions(ctx)
	case "show":
		return handleShow(ctx)
	case "stats":
		return handleStats(ctx)
	default:
		_, _ = fmt.Fprintf(ctx.Stderr, "unknown command: %q\n", name)
		return 2
	}
}

func splitCommand(cmd string) (name, args string) {
	for i := 0; i < len(cmd); i++ {
		if cmd[i] == ' ' {
			return cmd[:i], cmd[i+1:]
		}
	}
	return cmd, ""
}

func handleVersion(ctx HandlerCtx) int {
	resp := protocol.VersionResponse{
		SchemaVersion: protocol.SchemaVersion,
		BuildVersion:  ctx.Server.buildVersion,
	}
	if err := json.NewEncoder(ctx.Stdout).Encode(resp); err != nil {
		_, _ = fmt.Fprintln(ctx.Stderr, err)
		return 1
	}
	return 0
}
