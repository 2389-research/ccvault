// ABOUTME: End-to-end push tests
// ABOUTME: Real client + real ccvaultd; verifies data lands on the server correctly

package client

import (
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/2389-research/ccvault/internal/db"
	"github.com/2389-research/ccvault/internal/remote/server"
)

func TestPushSyncsSessions(t *testing.T) {
	srv, addr, signer, cleanup := server.StartTestServer(t)
	defer cleanup()

	// Build a local ccvault DB with a session ready to push
	dir := t.TempDir()
	localDataDir := filepath.Join(dir, "local")
	localDB, err := db.Open(localDataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = localDB.Close() }()

	seedLocalSession(t, localDB, "sess-push-1")

	c := &Client{
		Addr:    addr,
		User:    "ccvault",
		Signers: []ssh.Signer{signer},
		HostKey: ssh.InsecureIgnoreHostKey(),
	}
	stats, err := Push(c, localDB, "origin", false)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if stats.SessionsPushed != 1 {
		t.Errorf("SessionsPushed = %d, want 1", stats.SessionsPushed)
	}

	var count int
	if err := srv.DB().QueryRow("SELECT COUNT(*) FROM sessions WHERE id = ?", "sess-push-1").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("session missing on server")
	}

	stats, err = Push(c, localDB, "origin", false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.SessionsPushed != 0 {
		t.Errorf("second push SessionsPushed = %d, want 0 (incremental should skip)", stats.SessionsPushed)
	}
}

func TestPushDryRunSendsNothing(t *testing.T) {
	srv, addr, signer, cleanup := server.StartTestServer(t)
	defer cleanup()

	dir := t.TempDir()
	localDB, err := db.Open(filepath.Join(dir, "local"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = localDB.Close() }()
	seedLocalSession(t, localDB, "sess-dry-1")

	c := &Client{Addr: addr, User: "ccvault", Signers: []ssh.Signer{signer}, HostKey: ssh.InsecureIgnoreHostKey()}
	stats, err := Push(c, localDB, "origin", true) // dryRun
	if err != nil {
		t.Fatal(err)
	}
	if stats.SessionsPushed != 1 {
		t.Errorf("dry-run should report 1 pending session, got %d", stats.SessionsPushed)
	}
	var count int
	if err := srv.DB().QueryRow("SELECT COUNT(*) FROM sessions WHERE id = ?", "sess-dry-1").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("dry-run should not have written to server; count=%d", count)
	}
}

func TestPushDoesNotAdvanceWatermarkOnServerFailure(t *testing.T) {
	// Bring up a server so we know the addr is valid, then tear it down so
	// the Push dial fails. This is the cheapest way to guarantee that
	// c.Run("ingest", pr) fails, exercising the error path.
	_, addr, signer, cleanup := server.StartTestServer(t)
	cleanup() // stop the server; the port should now refuse connections.

	dir := t.TempDir()
	localDB, err := db.Open(filepath.Join(dir, "local"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = localDB.Close() }()
	seedLocalSession(t, localDB, "sess-fail-1")

	c := &Client{
		Addr:    addr,
		User:    "ccvault",
		Signers: []ssh.Signer{signer},
		HostKey: ssh.InsecureIgnoreHostKey(),
	}
	stats, err := Push(c, localDB, "origin", false)
	if err == nil {
		t.Fatalf("expected Push to error against dead server, got stats=%+v", stats)
	}

	// Nothing should have been recorded — the same session must still be
	// pending on the next attempt.
	pending, perr := localDB.SessionsPendingPush("origin")
	if perr != nil {
		t.Fatalf("pending: %v", perr)
	}
	found := false
	for _, id := range pending {
		if id == "sess-fail-1" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("failed push should not advance watermark; pending=%v", pending)
	}
}

// seedLocalSession creates a minimal project+session pair in the local DB.
// Uses time.Time bindings for date columns (modernc.org/sqlite serializes them
// consistently with production code; T5 hit this same gotcha).
func seedLocalSession(t *testing.T, d *db.DB, id string) {
	t.Helper()
	now := time.Now().UTC()
	_, err := d.Exec(`INSERT INTO projects (path, display_name, first_seen_at, last_activity_at, session_count, total_tokens, source)
        VALUES ('/tmp/p', 'p', ?, ?, 1, 0, 'claude-code')`, now, now)
	if err != nil {
		t.Fatal(err)
	}
	var pid int64
	if err := d.QueryRow("SELECT last_insert_rowid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	_, err = d.Exec(`INSERT INTO sessions
        (id, project_id, started_at, ended_at, model, git_branch, turn_count, input_tokens, output_tokens, source_file, source, pushed_by)
        VALUES (?, ?, ?, ?, '', '', 0, 0, 0, '/tmp/p/x.jsonl', 'claude-code', '')`,
		id, pid, now, now)
	if err != nil {
		t.Fatal(err)
	}
}
