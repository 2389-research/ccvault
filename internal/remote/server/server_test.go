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

func TestServerStatsCommand(t *testing.T) {
	_, addr, signer, cleanup := StartTestServer(t)
	defer cleanup()

	conn, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:            "ccvault",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
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
	if err := sess.Start("stats"); err != nil {
		t.Fatal(err)
	}
	var resp map[string]any
	if err := json.NewDecoder(stdout).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if _, ok := resp["projects"]; !ok {
		t.Errorf("expected 'projects' key in stats response, got %+v", resp)
	}
	// Empty DB — projects/sessions/turns/tokens should all be zero-valued.
	for _, k := range []string{"projects", "sessions", "turns", "tokens", "project_tokens"} {
		v, ok := resp[k]
		if !ok {
			t.Errorf("missing key %q in stats response", k)
			continue
		}
		// JSON numbers decode as float64.
		if n, ok := v.(float64); ok {
			if n != 0 {
				t.Errorf("expected %q = 0 on empty DB, got %v", k, n)
			}
		}
	}
	_ = sess.Wait()
}

func TestServerRejectsMalformedExec(t *testing.T) {
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

	// Open a session channel directly so we can send a raw exec request
	// with a truncated payload (only 2 bytes instead of the required 4+).
	ch, reqs, err := conn.OpenChannel("session", nil)
	if err != nil {
		t.Fatal(err)
	}
	go ssh.DiscardRequests(reqs)
	defer func() { _ = ch.Close() }()

	ok, err := ch.SendRequest("exec", true, []byte{0x00, 0x00})
	if err != nil {
		t.Fatalf("SendRequest: %v", err)
	}
	if ok {
		t.Errorf("server accepted malformed exec payload; expected reply=false")
	}
	// Server should not have panicked — a second, well-formed connection
	// still succeeds.
	conn2, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:            "ccvault",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(clientKey)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		t.Fatalf("server unhealthy after malformed exec: %v", err)
	}
	_ = conn2.Close()
}

func TestServerSearchAfterIngest(t *testing.T) {
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

	// ---- Ingest a session with a searchable turn ----
	ingestSess, err := conn.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	stdin, _ := ingestSess.StdinPipe()
	if err := ingestSess.Start("ingest"); err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(stdin)
	now := time.Now().UTC()
	_ = enc.Encode(protocol.IngestMessage{
		Kind: protocol.KindSession,
		Session: &models.Session{
			ID:          "sess-search-1",
			ProjectPath: "/tmp/searchproj",
			StartedAt:   now,
			EndedAt:     now,
			Source:      "claude-code",
		},
	})
	_ = enc.Encode(protocol.IngestMessage{
		Kind: protocol.KindTurn,
		Turn: &models.Turn{
			ID:        "turn-search-1",
			SessionID: "sess-search-1",
			Type:      "user",
			Timestamp: now,
			Content:   "the quick brown fox jumps over the lazy dog",
		},
	})
	_ = enc.Encode(protocol.IngestMessage{
		Kind: protocol.KindSessionEnd, SessionID: "sess-search-1",
	})
	_ = stdin.Close()
	if err := ingestSess.Wait(); err != nil {
		t.Fatalf("ingest wait: %v", err)
	}
	_ = ingestSess.Close()

	// ---- Search for it via a fresh SSH session ----
	searchSess, err := conn.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = searchSess.Close() }()
	stdout, _ := searchSess.StdoutPipe()
	// Multi-word quoted query — guards against the parseKV regression where
	// strings.Fields would split this into separate tokens and drop the tail.
	if err := searchSess.Start(`search q="quick brown fox"`); err != nil {
		t.Fatal(err)
	}

	// Read ndjson lines — one per result. Look for our session.
	found := false
	dec := json.NewDecoder(stdout)
	for dec.More() {
		var r map[string]any
		if err := dec.Decode(&r); err != nil {
			t.Fatalf("decode search result: %v", err)
		}
		if r["session_id"] == "sess-search-1" {
			found = true
		}
	}
	if err := searchSess.Wait(); err != nil {
		t.Fatalf("search wait: %v", err)
	}
	if !found {
		t.Errorf(`search q="quick brown fox" did not return sess-search-1`)
	}
}
