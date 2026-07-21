// ABOUTME: Tests for authorized_keys parsing
// ABOUTME: Verifies pubkey -> identity mapping and reload behavior

package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
)

// genPubKeyLine generates a fresh ed25519 SSH pubkey and returns
// (authorized_keys line without trailing newline, parsed ssh.PublicKey).
func genPubKeyLine(t *testing.T, comment string) (string, ssh.PublicKey) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("wrap ssh public key: %v", err)
	}
	marshaled := ssh.MarshalAuthorizedKey(sshPub)
	// MarshalAuthorizedKey returns "<type> <base64>\n"; strip the newline
	// and append the comment so the trailing identity is captured.
	line := string(marshaled[:len(marshaled)-1]) + " " + comment
	return line, sshPub
}

func TestLoadAuthorizedKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "authorized_keys")

	aliceLine, alicePub := genPubKeyLine(t, "alice@2389.ai")
	bobLine, _ := genPubKeyLine(t, "bob@2389.ai")

	contents := aliceLine + "\n" +
		"# a comment\n" +
		"\n" +
		bobLine + "\n"
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}

	a, err := LoadAuthorizedKeys(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	identity, ok := a.Lookup(alicePub)
	if !ok {
		t.Fatal("alice key not found")
	}
	if identity != "alice@2389.ai" {
		t.Errorf("identity = %q, want alice@2389.ai", identity)
	}
}

func TestLoadAuthorizedKeysRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "authorized_keys")
	if err := os.WriteFile(path, []byte("not-a-key oops\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadAuthorizedKeys(path)
	if err == nil {
		t.Fatal("expected error on garbage line")
	}
}
