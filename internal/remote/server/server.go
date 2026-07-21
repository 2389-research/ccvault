// ABOUTME: SSH server for ccvaultd — accepts pubkey-authed connections and dispatches commands
// ABOUTME: One connection = one command; identity is the authorized_keys comment field

package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"

	"golang.org/x/crypto/ssh"

	"github.com/2389-research/ccvault/internal/db"
)

type Server struct {
	db           *db.DB
	authKeys     *AuthorizedKeys
	hostKey      ssh.Signer
	buildVersion string
}

func New(database *db.DB, authKeys *AuthorizedKeys, hostKey ssh.Signer, buildVersion string) *Server {
	return &Server{db: database, authKeys: authKeys, hostKey: hostKey, buildVersion: buildVersion}
}

// DB exposes the underlying database (used by handlers).
func (s *Server) DB() *db.DB { return s.db }

// Serve accepts connections until ctx is cancelled or the listener closes.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		conn, err := ln.Accept()
		if err != nil {
			if isClosed(err) {
				return nil
			}
			return err
		}
		go s.handleConn(conn)
	}
}

func (s *Server) sshConfig() *ssh.ServerConfig {
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(md ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			id, ok := s.authKeys.Lookup(key)
			if !ok {
				return nil, fmt.Errorf("pubkey not authorized")
			}
			return &ssh.Permissions{Extensions: map[string]string{"ccvault-identity": id}}, nil
		},
	}
	cfg.AddHostKey(s.hostKey)
	return cfg
}

func (s *Server) handleConn(nc net.Conn) {
	defer func() { _ = nc.Close() }()
	sconn, chans, reqs, err := ssh.NewServerConn(nc, s.sshConfig())
	if err != nil {
		log.Printf("ssh handshake: %v", err)
		return
	}
	defer func() { _ = sconn.Close() }()
	identity := sconn.Permissions.Extensions["ccvault-identity"]

	go ssh.DiscardRequests(reqs)
	for ch := range chans {
		if ch.ChannelType() != "session" {
			_ = ch.Reject(ssh.UnknownChannelType, "only 'session' supported")
			continue
		}
		channel, requests, err := ch.Accept()
		if err != nil {
			log.Printf("accept channel: %v", err)
			continue
		}
		go s.handleChannel(channel, requests, identity)
	}
}

func (s *Server) handleChannel(ch ssh.Channel, reqs <-chan *ssh.Request, identity string) {
	for req := range reqs {
		switch req.Type {
		case "exec":
			command := string(req.Payload[4:]) // strip 4-byte length prefix
			_ = req.Reply(true, nil)
			code := s.dispatch(command, HandlerCtx{
				Server:   s,
				Identity: identity,
				Stdin:    ch,
				Stdout:   ch,
				Stderr:   ch.Stderr(),
			})
			_, _ = ch.SendRequest("exit-status", false, exitStatusPayload(code))
			_ = ch.Close()
			return
		case "shell":
			_ = req.Reply(false, nil)
			_ = ch.Close()
			return
		default:
			_ = req.Reply(false, nil)
		}
	}
}

func exitStatusPayload(code int) []byte {
	return []byte{
		byte(code >> 24), byte(code >> 16), byte(code >> 8), byte(code),
	}
}

func isClosed(err error) bool {
	if errors.Is(err, io.EOF) {
		return true
	}
	if err == nil {
		return false
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return opErr.Err.Error() == "use of closed network connection"
	}
	return err.Error() == "use of closed network connection"
}
