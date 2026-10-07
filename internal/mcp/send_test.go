// ABOUTME: Tests that a response which will not marshal still answers the request id.
// ABOUTME: A dropped reply hangs a JSON-RPC client; these assert bytes reach the transport.

package mcp

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// decodeResponse reads the single JSON-RPC message written to the
// transport. A test that expected a reply and got silence stops here
// naming that, because silence is the failure under test.
func decodeResponse(t *testing.T, raw string) jsonRPCResponse {
	t.Helper()

	line := strings.TrimSpace(raw)
	if line == "" {
		t.Fatal("nothing was written to the transport; the client is left waiting on a request that will never be answered")
	}

	var resp jsonRPCResponse
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		t.Fatalf("response is not valid JSON-RPC: %v (wrote %q)", err, line)
	}
	return resp
}

// TestSendResult_AnswersIDWhenPayloadWillNotMarshal is the liveness case.
// A NaN float — reachable from the DuckDB analytics aggregates, e.g. an
// empty-window division — makes json.Marshal fail. The reply must still
// arrive, carrying the id, or the client blocks until it times out.
func TestSendResult_AnswersIDWhenPayloadWillNotMarshal(t *testing.T) {
	s, buf := newBufferedServer(t)

	s.sendResult(float64(7), map[string]interface{}{"avg_tokens": math.NaN()})

	resp := decodeResponse(t, buf.String())
	if resp.JSONRPC != "2.0" {
		t.Errorf("jsonrpc = %q, want \"2.0\"", resp.JSONRPC)
	}
	id, ok := resp.ID.(float64)
	if !ok || id != 7 {
		t.Errorf("id = %#v, want 7 — the reply must answer the outstanding request", resp.ID)
	}
	if resp.Error == nil {
		t.Fatalf("error is missing; an unserializable result has to come back as an error, got result %#v", resp.Result)
	}
	if resp.Error.Code != internalErrorCode {
		t.Errorf("error.code = %d, want %d", resp.Error.Code, internalErrorCode)
	}
	if resp.Error.Message == "" {
		t.Error("error.message is empty; the client has nothing to react to")
	}
}

// TestSendResult_AnswersStringIDWhenPayloadWillNotMarshal covers the other
// id type a JSON-RPC client may use. The id is echoed verbatim, not
// normalized, because the client matches on it.
func TestSendResult_AnswersStringIDWhenPayloadWillNotMarshal(t *testing.T) {
	s, buf := newBufferedServer(t)

	s.sendResult("req-abc", map[string]interface{}{"ratio": math.Inf(1)})

	resp := decodeResponse(t, buf.String())
	if resp.ID != "req-abc" {
		t.Errorf("id = %#v, want \"req-abc\"", resp.ID)
	}
	if resp.Error == nil {
		t.Fatal("error is missing from the fallback reply")
	}
}

// TestSendError_AnswersIDWhenDataWillNotMarshal guards the other entry
// point. sendError takes caller-supplied `data`, so it can fail to
// marshal the same way a result can.
func TestSendError_AnswersIDWhenDataWillNotMarshal(t *testing.T) {
	s, buf := newBufferedServer(t)

	s.sendError(float64(11), -32602, "Invalid params", map[string]interface{}{"bad": math.NaN()})

	resp := decodeResponse(t, buf.String())
	id, ok := resp.ID.(float64)
	if !ok || id != 11 {
		t.Errorf("id = %#v, want 11", resp.ID)
	}
	if resp.Error == nil {
		t.Fatal("error is missing from the fallback reply")
	}
}

// TestSend_FallbackCarriesNoCallerData is the point of the fallback: it is
// built from literals and the id alone, so it cannot fail to marshal for
// the same reason the response it replaces did.
func TestSend_FallbackCarriesNoCallerData(t *testing.T) {
	s, buf := newBufferedServer(t)

	// A channel is unmarshallable for a different reason than NaN, and a
	// fallback that echoed caller data would carry it straight back in.
	s.sendResult(float64(3), map[string]interface{}{"ch": make(chan int)})

	resp := decodeResponse(t, buf.String())
	if resp.Error == nil {
		t.Fatal("error is missing from the fallback reply")
	}
	if resp.Result != nil {
		t.Errorf("result = %#v, want absent — the payload is what failed", resp.Result)
	}
}

// TestSend_MarshallableResponseIsUnchanged keeps the fallback off the happy
// path: a response that marshals fine is written exactly as before.
func TestSend_MarshallableResponseIsUnchanged(t *testing.T) {
	s, buf := newBufferedServer(t)

	s.sendResult(float64(1), map[string]interface{}{"ok": true})

	resp := decodeResponse(t, buf.String())
	if resp.Error != nil {
		t.Fatalf("error = %#v, want none for a marshallable result", resp.Error)
	}
	result, ok := resp.Result.(map[string]interface{})
	if !ok {
		t.Fatalf("result is %T, want map", resp.Result)
	}
	if result["ok"] != true {
		t.Errorf("result[ok] = %#v, want true", result["ok"])
	}
}
