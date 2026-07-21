// ABOUTME: Test helpers exported for cross-package use (client tests, integration tests)
// ABOUTME: Not imported by production code paths — safe to include in the binary

package server

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/2389-research/ccvault/internal/db"
)

// StartTestServer starts a ccvaultd on a random localhost port with a fresh
// SQLite tempdir and a temp authorized_keys containing the returned client key.
// Returns (server, addr, clientSigner, cleanup). Cleanup stops the server and
// closes the DB.
func StartTestServer(t *testing.T) (*Server, string, ssh.Signer, func()) {
	t.Helper()

	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}

	// Generate a client key and write its pubkey to authorized_keys.
	// Reuse LoadOrGenerateHostKey because it's just an ed25519 keypair.
	clientKeyPath := filepath.Join(dir, "client_key")
	clientSigner, err := LoadOrGenerateHostKey(clientKeyPath)
	if err != nil {
		t.Fatal(err)
	}

	pubLine := string(ssh.MarshalAuthorizedKey(clientSigner.PublicKey()))
	// MarshalAuthorizedKey emits a trailing newline. Insert an identity comment
	// before that newline.
	pubLine = pubLine[:len(pubLine)-1] + " test-user\n"
	authPath := filepath.Join(dir, "authorized_keys")
	if err := os.WriteFile(authPath, []byte(pubLine), 0600); err != nil {
		t.Fatal(err)
	}

	authKeys, err := LoadAuthorizedKeys(authPath)
	if err != nil {
		t.Fatal(err)
	}

	hostKeyPath := filepath.Join(dir, "host_key")
	hostSigner, err := LoadOrGenerateHostKey(hostKeyPath)
	if err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	srv := New(database, authKeys, hostSigner, "0.0.1-test")
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Serve(ctx, ln) }()

	cleanup := func() {
		cancel()
		_ = ln.Close()
		_ = database.Close()
	}
	return srv, ln.Addr().String(), clientSigner, cleanup
}
