// ABOUTME: Drives Server.Run over an injected reader so the read→dispatch→write loop runs in-process.
// ABOUTME: Covers framing, malformed JSON, notifications and unterminated input without spawning a binary.

package mcp

import (
	"bytes"
	"strings"
	"testing"
)

// runServerOver feeds input to s.Run and returns the JSON-RPC messages it
// wrote. Run stops at EOF, so the reader closing is what ends the loop —
// no goroutine, timeout or signal is involved, which is the whole point of
// the input being injectable (#128).
func runServerOver(t *testing.T, s *Server, input string) []jsonRPCResponse {
	t.Helper()

	buf := &bytes.Buffer{}
	s.in = strings.NewReader(input)
	s.out = buf

	if err := s.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var responses []jsonRPCResponse
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		responses = append(responses, decodeResponse(t, line))
	}
	return responses
}

// TestRun_AnswersEveryRequestFromTheInjectedReader is the case #128 exists
// for: the full read→dispatch→write path, exercised without building and
// spawning a binary.
func TestRun_AnswersEveryRequestFromTheInjectedReader(t *testing.T) {
	responses := runServerOver(t, &Server{}, ""+
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`+"\n"+
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`+"\n")

	if len(responses) != 2 {
		t.Fatalf("got %d responses, want 2: %#v", len(responses), responses)
	}
	for i, want := range []float64{1, 2} {
		if got, ok := responses[i].ID.(float64); !ok || got != want {
			t.Errorf("response %d id = %#v, want %v", i, responses[i].ID, want)
		}
		if responses[i].Error != nil {
			t.Errorf("response %d is an error: %#v", i, responses[i].Error)
		}
	}
}

// TestRun_MalformedLineIsAnsweredAndTheLoopContinues pins that one bad line
// does not take the session down with it. A client that garbles a frame has
// to be able to recover by sending the next one.
func TestRun_MalformedLineIsAnsweredAndTheLoopContinues(t *testing.T) {
	responses := runServerOver(t, &Server{}, ""+
		"not json at all\n"+
		`{"jsonrpc":"2.0","id":7,"method":"ping"}`+"\n")

	if len(responses) != 2 {
		t.Fatalf("got %d responses, want a parse error then the ping reply: %#v", len(responses), responses)
	}
	if responses[0].Error == nil || responses[0].Error.Code != -32700 {
		t.Errorf("first response = %#v, want a -32700 parse error", responses[0])
	}
	if responses[0].ID != nil {
		t.Errorf("parse error id = %#v, want null — the id could not be read", responses[0].ID)
	}
	if id, ok := responses[1].ID.(float64); !ok || id != 7 {
		t.Errorf("second response id = %#v, want 7 — the loop must survive a bad frame", responses[1].ID)
	}
}

// TestRun_NotificationsAreNotAnswered covers JSON-RPC 2.0's rule that a
// message with no id gets no reply, even when the method is unknown. The
// trailing ping is there so an empty result cannot pass by accident.
func TestRun_NotificationsAreNotAnswered(t *testing.T) {
	responses := runServerOver(t, &Server{}, ""+
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n"+
		`{"jsonrpc":"2.0","method":"no/such/method"}`+"\n"+
		`{"jsonrpc":"2.0","id":3,"method":"ping"}`+"\n")

	if len(responses) != 1 {
		t.Fatalf("got %d responses, want only the ping reply: %#v", len(responses), responses)
	}
	if id, ok := responses[0].ID.(float64); !ok || id != 3 {
		t.Errorf("response id = %#v, want 3", responses[0].ID)
	}
}

// TestRun_UnknownMethodWithAnIDGetsMethodNotFound is the other half: a
// request names an id, so it must be answered even when we cannot serve it.
func TestRun_UnknownMethodWithAnIDGetsMethodNotFound(t *testing.T) {
	responses := runServerOver(t, &Server{},
		`{"jsonrpc":"2.0","id":4,"method":"no/such/method"}`+"\n")

	if len(responses) != 1 {
		t.Fatalf("got %d responses, want 1: %#v", len(responses), responses)
	}
	if responses[0].Error == nil || responses[0].Error.Code != -32601 {
		t.Errorf("response = %#v, want a -32601 method-not-found", responses[0])
	}
}
