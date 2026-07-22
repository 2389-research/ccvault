// ABOUTME: End-to-end SSH server tests
// ABOUTME: Spins up a real ccvaultd on a random port and connects via ssh client

package server

import (
	"bufio"
	"encoding/json"
	"io"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/2389-research/ccvault/internal/remote/protocol"
	"github.com/2389-research/ccvault/pkg/models"
)

func TestServerVersionCommand(t *testing.T) {
	_, addr, clientKey, cleanup := StartTestServer(t)
	defer cleanup()

	conn, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:            "ccvault",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(clientKey)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	sess, err := conn.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()

	stdout, _ := sess.StdoutPipe()
	if err := sess.Start("version"); err != nil {
		t.Fatal(err)
	}

	line, _, err := bufio.NewReader(stdout).ReadLine()
	if err != nil && err != io.EOF {
		t.Fatal(err)
	}
	var resp protocol.VersionResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("unmarshal %q: %v", string(line), err)
	}
	if resp.SchemaVersion != protocol.SchemaVersion {
		t.Errorf("schema_version = %d, want %d", resp.SchemaVersion, protocol.SchemaVersion)
	}
	if resp.BuildVersion != "0.0.1-test" {
		t.Errorf("build_version = %q, want 0.0.1-test", resp.BuildVersion)
	}
	_ = sess.Wait()
}

func TestServerIngestSession(t *testing.T) {
	srv, addr, clientKey, cleanup := StartTestServer(t)
	defer cleanup()

	conn, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:            "ccvault",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(clientKey)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	sess, err := conn.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()

	stdin, _ := sess.StdinPipe()
	if err := sess.Start("ingest"); err != nil {
		t.Fatal(err)
	}

	enc := json.NewEncoder(stdin)
	now := time.Now().UTC()
	_ = enc.Encode(protocol.IngestMessage{
		Kind: protocol.KindSession,
		Session: &models.Session{
			ID:          "sess-e2e-1",
			ProjectPath: "/tmp/p",
			StartedAt:   now,
			EndedAt:     now,
			Source:      "claude-code",
		},
	})
	_ = enc.Encode(protocol.IngestMessage{
		Kind: protocol.KindSessionEnd, SessionID: "sess-e2e-1",
	})
	_ = stdin.Close()
	_ = sess.Wait()

	var pushedBy string
	var count int
	row := srv.DB().QueryRow(
		"SELECT COUNT(*), COALESCE(MAX(pushed_by), '') FROM sessions WHERE id = ?",
		"sess-e2e-1",
	)
	if err := row.Scan(&count, &pushedBy); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("session not persisted, count=%d", count)
	}
	if pushedBy != "test-user" {
		t.Errorf("pushed_by = %q, want test-user", pushedBy)
	}
}
