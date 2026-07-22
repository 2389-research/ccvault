// ABOUTME: SSH client wrapper for talking to ccvaultd
// ABOUTME: Uses ssh-agent when available; falls back to identity files under ~/.ssh

package client

import (
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// Client is a minimal SSH client for ccvaultd.
type Client struct {
	Addr    string
	User    string
	Signers []ssh.Signer
	HostKey ssh.HostKeyCallback
	Timeout time.Duration
}

// FromRemoteURL builds a Client from a user-supplied URL string.
// Accepted forms:
//   - user@host
//   - user@host:port
//   - ssh://user@host:port/anything
//   - ssh://host:port (default user "ccvault")
//   - host (default user "ccvault", default port 22)
func FromRemoteURL(raw string) (*Client, error) {
	user, host, port, err := parseRemoteURL(raw)
	if err != nil {
		return nil, err
	}
	signers, err := defaultSigners()
	if err != nil {
		return nil, err
	}
	return &Client{
		Addr:    net.JoinHostPort(host, port),
		User:    user,
		Signers: signers,
		HostKey: knownHostsCallback(),
		Timeout: 30 * time.Second,
	}, nil
}

func parseRemoteURL(raw string) (user, host, port string, err error) {
	port = "22"
	user = "ccvault"

	if strings.HasPrefix(raw, "ssh://") {
		u, perr := url.Parse(raw)
		if perr != nil {
			return "", "", "", perr
		}
		if u.User != nil {
			user = u.User.Username()
		}
		host = u.Hostname()
		if p := u.Port(); p != "" {
			port = p
		}
		if host == "" {
			return "", "", "", fmt.Errorf("no host in %s", raw)
		}
		return
	}

	if at := strings.Index(raw, "@"); at >= 0 {
		user = raw[:at]
		raw = raw[at+1:]
	}
	host = raw
	if colon := strings.LastIndex(raw, ":"); colon >= 0 && !strings.Contains(raw, "]") {
		host = raw[:colon]
		port = raw[colon+1:]
	}
	if host == "" {
		return "", "", "", fmt.Errorf("no host in remote URL")
	}
	return
}

func defaultSigners() ([]ssh.Signer, error) {
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		conn, err := net.Dial("unix", sock)
		if err == nil {
			ag := agent.NewClient(conn)
			signers, err := ag.Signers()
			if err == nil && len(signers) > 0 {
				return signers, nil
			}
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"id_ed25519", "id_rsa"} {
		path := filepath.Join(home, ".ssh", name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		signer, err := ssh.ParsePrivateKey(data)
		if err != nil {
			continue
		}
		return []ssh.Signer{signer}, nil
	}
	return nil, fmt.Errorf("no SSH keys available (no agent, no ~/.ssh/id_ed25519 or id_rsa)")
}

func knownHostsCallback() ssh.HostKeyCallback {
	// v1: known_hosts wiring deferred. See README/design doc.
	return ssh.InsecureIgnoreHostKey()
}

// Run executes a single command against the server. If stdin is non-nil, it is
// piped to the server and closed when the returned reader is Closed. Caller
// must Close the returned reader.
func (c *Client) Run(command string, stdin io.Reader) (io.ReadCloser, *ssh.Session, error) {
	cfg := &ssh.ClientConfig{
		User:            c.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(c.Signers...)},
		HostKeyCallback: c.HostKey,
		Timeout:         c.Timeout,
	}
	conn, err := ssh.Dial("tcp", c.Addr, cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("dial %s: %w", c.Addr, err)
	}
	sess, err := conn.NewSession()
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		_ = sess.Close()
		_ = conn.Close()
		return nil, nil, err
	}
	if stdin != nil {
		w, err := sess.StdinPipe()
		if err != nil {
			_ = sess.Close()
			_ = conn.Close()
			return nil, nil, err
		}
		go func() {
			_, _ = io.Copy(w, stdin)
			_ = w.Close()
		}()
	}
	if err := sess.Start(command); err != nil {
		_ = sess.Close()
		_ = conn.Close()
		return nil, nil, err
	}
	return &sessionReader{r: stdout, sess: sess, conn: conn}, sess, nil
}

type sessionReader struct {
	r    io.Reader
	sess *ssh.Session
	conn *ssh.Client
}

func (s *sessionReader) Read(p []byte) (int, error) { return s.r.Read(p) }

// Close waits for the remote command to finish and returns its exit-status
// error (if any). A non-nil Wait error means the server rejected or errored
// mid-command; callers should treat any local side effects predicated on a
// successful run as invalid.
func (s *sessionReader) Close() error {
	waitErr := s.sess.Wait()
	_ = s.sess.Close()
	connErr := s.conn.Close()
	if waitErr != nil {
		return waitErr
	}
	return connErr
}
