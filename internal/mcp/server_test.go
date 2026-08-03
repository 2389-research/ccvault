// ABOUTME: Tests for the MCP JSON-RPC server — notification handling per issue #7.
// ABOUTME: Uses the injected io.Writer to inspect exactly what bytes are emitted.

package mcp

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// newTestServer builds a Server with just enough state to exercise the
// dispatch layer. The db is nil because notification handlers don't touch
// it. If a future test needs the DB, use db.Open(t.TempDir()) instead.
func newTestServer(t *testing.T) (*Server, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	return &Server{out: buf}, buf
}

func TestServer_NotificationsInitialized_IsSilent(t *testing.T) {
	// Reporter's exact repro shape (issue #7).
	s, buf := newTestServer(t)
	s.handleRequest(&jsonRPCRequest{
		JSONRPC: "2.0",
		Method:  "notifications/initialized",
	})
	if buf.Len() != 0 {
		t.Errorf("notification produced a response: %q", buf.String())
	}
}

func TestServer_NotificationsCancelled_IsSilent(t *testing.T) {
	s, buf := newTestServer(t)
	s.handleRequest(&jsonRPCRequest{
		JSONRPC: "2.0",
		Method:  "notifications/cancelled",
		Params:  json.RawMessage(`{"requestId":1}`),
	})
	if buf.Len() != 0 {
		t.Errorf("notification produced a response: %q", buf.String())
	}
}

func TestServer_NotificationsRootsListChanged_IsSilent(t *testing.T) {
	s, buf := newTestServer(t)
	s.handleRequest(&jsonRPCRequest{
		JSONRPC: "2.0",
		Method:  "notifications/roots/list_changed",
	})
	if buf.Len() != 0 {
		t.Errorf("notification produced a response: %q", buf.String())
	}
}

func TestServer_UnknownNotification_IsSilent(t *testing.T) {
	// Any unrecognized method with no id must produce no output, per the
	// JSON-RPC 2.0 notification rule. Guards the default-branch fix.
	s, buf := newTestServer(t)
	s.handleRequest(&jsonRPCRequest{
		JSONRPC: "2.0",
		Method:  "notifications/some/future/thing",
	})
	if buf.Len() != 0 {
		t.Errorf("unknown notification produced a response: %q", buf.String())
	}
}

func TestServer_UnknownRequest_ReturnsErrorWithID(t *testing.T) {
	// Regression guard: an unknown REQUEST (has id) must still receive
	// a proper -32601 Method not found. The fix should only suppress
	// responses when id is nil.
	s, buf := newTestServer(t)
	s.handleRequest(&jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      float64(42), // json.Unmarshal turns numeric ids into float64
		Method:  "resources/list",
	})
	var resp map[string]any
	if err := json.Unmarshal(buf.Bytes(), &resp); err != nil {
		t.Fatalf("response is not valid JSON: %q", buf.String())
	}
	if resp["id"] != float64(42) {
		t.Errorf("response id = %v, want 42", resp["id"])
	}
	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("no error object in response: %v", resp)
	}
	if errObj["code"] != float64(-32601) {
		t.Errorf("error code = %v, want -32601", errObj["code"])
	}
}

func TestServer_Initialize_ReturnsExpectedShape(t *testing.T) {
	// Regression guard: the happy-path handler still returns a
	// spec-shaped result. Verifies our fix didn't break the working
	// request path.
	s, buf := newTestServer(t)
	s.handleRequest(&jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      float64(1),
		Method:  "initialize",
		Params:  json.RawMessage(`{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"1"}}`),
	})
	body := buf.String()
	for _, want := range []string{`"id":1`, `"result"`, `"protocolVersion":"2024-11-05"`, `"serverInfo"`} {
		if !strings.Contains(body, want) {
			t.Errorf("initialize response missing %q; got: %s", want, body)
		}
	}
}
