// ABOUTME: Client-side SSH connection tests
// ABOUTME: Reuses server test helpers to drive real end-to-end scenarios

package client

import (
	"bufio"
	"encoding/json"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/2389-research/ccvault/internal/remote/protocol"
	"github.com/2389-research/ccvault/internal/remote/server"
)

func TestClientRunVersion(t *testing.T) {
	_, addr, signer, cleanup := server.StartTestServer(t)
	defer cleanup()

	c := &Client{
		Addr:    addr,
		User:    "ccvault",
		Signers: []ssh.Signer{signer},
		HostKey: ssh.InsecureIgnoreHostKey(),
	}
	stdout, _, err := c.Run("version", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stdout.Close() }()

	line, _, _ := bufio.NewReader(stdout).ReadLine()
	var resp protocol.VersionResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("%v: %q", err, line)
	}
	if resp.SchemaVersion != protocol.SchemaVersion {
		t.Errorf("wrong schema version: %d", resp.SchemaVersion)
	}
}

func TestParseRemoteURL(t *testing.T) {
	cases := []struct {
		in                 string
		wantUser, wantHost string
		wantPort           string
	}{
		{"ccvault@vault.company.com", "ccvault", "vault.company.com", "22"},
		{"alice@host:2222", "alice", "host", "2222"},
		{"ssh://vault.company.com", "ccvault", "vault.company.com", "22"},
		{"ssh://alice@host:2222/anything", "alice", "host", "2222"},
		{"host.only", "ccvault", "host.only", "22"},
	}
	for _, tc := range cases {
		u, h, p, err := parseRemoteURL(tc.in)
		if err != nil {
			t.Fatalf("%s: %v", tc.in, err)
		}
		if u != tc.wantUser || h != tc.wantHost || p != tc.wantPort {
			t.Errorf("%s: got (%s, %s, %s), want (%s, %s, %s)",
				tc.in, u, h, p, tc.wantUser, tc.wantHost, tc.wantPort)
		}
	}
}
