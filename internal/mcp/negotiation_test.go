// ABOUTME: Tests MCP protocol version negotiation — agree, downgrade and no-overlap.
// ABOUTME: initialize used to ignore the client's proposal and assert 2024-11-05 at everyone.

package mcp

import (
	"encoding/json"
	"testing"
)

// initializeOver runs one initialize request carrying params through the real
// read → dispatch → write path and returns the reply. Driving Run rather than
// calling the handler keeps the request in the shape a client actually sends,
// which is what #128's injectable reader bought.
func initializeOver(t *testing.T, params string) jsonRPCResponse {
	t.Helper()

	responses := runServerOver(t, &Server{},
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":`+params+"}\n")
	if len(responses) != 1 {
		t.Fatalf("got %d responses, want 1: %#v", len(responses), responses)
	}
	return responses[0]
}

// negotiatedVersion fails the test if the reply is an error, and otherwise
// returns the protocol version the server agreed to.
func negotiatedVersion(t *testing.T, resp jsonRPCResponse) string {
	t.Helper()

	if resp.Error != nil {
		t.Fatalf("initialize failed: %#v", resp.Error)
	}
	raw, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatalf("remarshal result: %v", err)
	}
	var result initializeResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("result is not an initializeResult: %v (%s)", err, raw)
	}
	return result.ProtocolVersion
}

// TestInitialize_AgreesWithAVersionItSupports is the spec's first rule: if
// the server supports the requested version it MUST respond with the same
// one. Every revision we claim has to hold, not just the newest — a client
// pinned to an older one is the case the old hardcoded literal got right by
// accident and would have got wrong for any other answer.
func TestInitialize_AgreesWithAVersionItSupports(t *testing.T) {
	for _, want := range supportedProtocolVersions {
		t.Run(want, func(t *testing.T) {
			resp := initializeOver(t, `{"protocolVersion":"`+want+`","capabilities":{}}`)
			if got := negotiatedVersion(t, resp); got != want {
				t.Errorf("negotiated %q, want the requested %q", got, want)
			}
		})
	}
}

// TestInitialize_DowngradesToItsLatestForAnUnsupportedRevision is the
// spec's second rule: otherwise the server MUST respond with another
// version it supports, and that SHOULD be its latest. Both directions
// count — a revision newer than anything we know, and the first MCP
// revision, which predates every version we speak.
func TestInitialize_DowngradesToItsLatestForAnUnsupportedRevision(t *testing.T) {
	for _, requested := range []string{"2099-01-01", "2024-10-07"} {
		t.Run(requested, func(t *testing.T) {
			resp := initializeOver(t, `{"protocolVersion":"`+requested+`","capabilities":{}}`)
			got := negotiatedVersion(t, resp)
			if got == requested {
				t.Fatalf("negotiated %q, which this server does not support", got)
			}
			if got != supportedProtocolVersions[0] {
				t.Errorf("negotiated %q, want the latest supported %q", got, supportedProtocolVersions[0])
			}
		})
	}
}

// TestInitialize_RejectsAProposalThatIsNotAnMCPRevision is the no-overlap
// branch, and the spec's own example: "1.0.0" names no MCP revision, so
// there is nothing to downgrade to and answering with a date would be
// asserting agreement that does not exist. The reply is the spec's error,
// carrying what we support and what was asked so the client can say why it
// is disconnecting.
func TestInitialize_RejectsAProposalThatIsNotAnMCPRevision(t *testing.T) {
	const requested = "1.0.0"

	resp := initializeOver(t, `{"protocolVersion":"`+requested+`","capabilities":{}}`)

	if resp.Error == nil {
		t.Fatalf("initialize succeeded with result %#v, want an unsupported-version error", resp.Result)
	}
	if resp.Error.Code != invalidParamsCode {
		t.Errorf("error.code = %d, want %d", resp.Error.Code, invalidParamsCode)
	}
	if resp.Error.Message != "Unsupported protocol version" {
		t.Errorf("error.message = %q, want %q", resp.Error.Message, "Unsupported protocol version")
	}

	data, ok := resp.Error.Data.(map[string]interface{})
	if !ok {
		t.Fatalf("error.data = %#v, want an object carrying supported and requested", resp.Error.Data)
	}
	if got := data["requested"]; got != requested {
		t.Errorf("error.data.requested = %#v, want %q", got, requested)
	}
	supported, ok := data["supported"].([]interface{})
	if !ok || len(supported) != len(supportedProtocolVersions) {
		t.Fatalf("error.data.supported = %#v, want the %d versions this server speaks",
			data["supported"], len(supportedProtocolVersions))
	}
	for i, want := range supportedProtocolVersions {
		if supported[i] != want {
			t.Errorf("error.data.supported[%d] = %#v, want %q", i, supported[i], want)
		}
	}
}

// TestInitialize_WithNoProposalAnswersTheLatestSupported covers the request
// the issue reproduced with: params carrying no protocolVersion at all. The
// spec requires clients to send one, so there is nothing to negotiate
// against; answering our latest is a version we do support, which is more
// than the old literal could claim.
func TestInitialize_WithNoProposalAnswersTheLatestSupported(t *testing.T) {
	for name, params := range map[string]string{
		"empty params":  `{}`,
		"null params":   `null`,
		"empty version": `{"protocolVersion":""}`,
	} {
		t.Run(name, func(t *testing.T) {
			resp := initializeOver(t, params)
			if got := negotiatedVersion(t, resp); got != supportedProtocolVersions[0] {
				t.Errorf("negotiated %q, want the latest supported %q", got, supportedProtocolVersions[0])
			}
		})
	}
}

// TestInitialize_RejectsParamsItCannotRead pins that a protocolVersion of
// the wrong JSON type is reported rather than silently read as absent.
// Treating it as "no proposal" would answer a version the client never
// asked about and never find out it had sent nonsense.
func TestInitialize_RejectsParamsItCannotRead(t *testing.T) {
	resp := initializeOver(t, `{"protocolVersion":20241105}`)

	if resp.Error == nil {
		t.Fatalf("initialize succeeded with result %#v, want an invalid-params error", resp.Result)
	}
	if resp.Error.Code != invalidParamsCode {
		t.Errorf("error.code = %d, want %d", resp.Error.Code, invalidParamsCode)
	}
}

// TestInitialize_DoesNotAssertAVersionAtEveryClient is issue #127 stated as
// a test. The handler never unmarshalled req.Params, so whatever a client
// proposed it was told 2024-11-05 — agreement it had no basis to claim.
func TestInitialize_DoesNotAssertAVersionAtEveryClient(t *testing.T) {
	const newer = "2025-06-18"

	resp := initializeOver(t, `{"protocolVersion":"`+newer+`","capabilities":{}}`)

	if got := negotiatedVersion(t, resp); got == "2024-11-05" && newer != "2024-11-05" {
		t.Errorf("negotiated the hardcoded %q for a client proposing %q (#127)", got, newer)
	}
}
