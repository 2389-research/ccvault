// ABOUTME: Host key management for ccvaultd — load or generate ed25519 on first launch
// ABOUTME: Persists PEM-encoded key at the configured path with 0600 permissions

package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"os"

	"golang.org/x/crypto/ssh"
)

// LoadOrGenerateHostKey returns a signer for the host key at path.
// If the file doesn't exist, a new ed25519 key is generated and persisted.
func LoadOrGenerateHostKey(path string) (ssh.Signer, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		return ssh.ParsePrivateKey(data)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read host key %s: %w", path, err)
	}

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate ed25519: %w", err)
	}
	pemBlock, err := ssh.MarshalPrivateKey(priv, "ccvaultd host key")
	if err != nil {
		return nil, fmt.Errorf("marshal ssh private key: %w", err)
	}
	pemBytes := pem.EncodeToMemory(pemBlock)
	if err := os.WriteFile(path, pemBytes, 0600); err != nil {
		return nil, fmt.Errorf("write host key: %w", err)
	}
	return ssh.ParsePrivateKey(pemBytes)
}
