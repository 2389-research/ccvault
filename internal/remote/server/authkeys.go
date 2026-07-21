// ABOUTME: Loads and looks up SSH pubkeys from an OpenSSH authorized_keys file
// ABOUTME: The trailing comment on each line is used as the pushed_by identity

package server

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
)

// AuthorizedKeys holds a set of authorized SSH pubkeys keyed by their marshaled form,
// mapping to an identity string (the trailing comment on the authorized_keys line).
type AuthorizedKeys struct {
	mu    sync.RWMutex
	byKey map[string]string
	path  string
}

// LoadAuthorizedKeys parses an OpenSSH authorized_keys file.
func LoadAuthorizedKeys(path string) (*AuthorizedKeys, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	a := &AuthorizedKeys{byKey: make(map[string]string), path: path}
	for lineno, raw := range bytes.Split(data, []byte("\n")) {
		line := bytes.TrimSpace(raw)
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		pubkey, comment, _, _, err := ssh.ParseAuthorizedKey(line)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: parse authorized_keys line: %w", path, lineno+1, err)
		}
		identity := strings.TrimSpace(comment)
		if identity == "" {
			identity = fmt.Sprintf("unknown-key-%d", lineno+1)
		}
		a.byKey[string(pubkey.Marshal())] = identity
	}
	return a, nil
}

// Lookup returns the identity for a matching pubkey, or ("", false).
func (a *AuthorizedKeys) Lookup(key ssh.PublicKey) (string, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	id, ok := a.byKey[string(key.Marshal())]
	return id, ok
}

// Reload re-reads the underlying file and atomically swaps the key map.
// Call this on SIGHUP.
func (a *AuthorizedKeys) Reload() error {
	fresh, err := LoadAuthorizedKeys(a.path)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.byKey = fresh.byKey
	a.mu.Unlock()
	return nil
}
