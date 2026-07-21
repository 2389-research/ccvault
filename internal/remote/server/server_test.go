// ABOUTME: End-to-end SSH server tests
// ABOUTME: Spins up a real ccvaultd on a random port and connects via ssh client

package server

import (
	"bufio"
	"encoding/json"
	"io"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/2389-research/ccvault/internal/remote/protocol"
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
