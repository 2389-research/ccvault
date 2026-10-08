// ABOUTME: Tests that the MCP initialize response advertises the real build version
// ABOUTME: Pins that an unstamped build says so instead of inventing a release number

package mcp

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/2389-research/ccvault/internal/buildinfo"
	"github.com/2389-research/ccvault/internal/config"
)

// initializeServerInfo runs one initialize request against s and returns the
// serverInfo object the client would see.
func initializeServerInfo(t *testing.T, s *Server) serverInfo {
	t.Helper()

	buf := &bytes.Buffer{}
	s.out = buf
	s.handleRequest(&jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      float64(1),
		Method:  "initialize",
		Params:  json.RawMessage(`{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"1"}}`),
	})

	var resp struct {
		Result initializeResult `json:"result"`
	}
	if err := json.Unmarshal(buf.Bytes(), &resp); err != nil {
		t.Fatalf("initialize response is not JSON: %v\n%s", err, buf.String())
	}
	return resp.Result.ServerInfo
}

// TestInitialize_AdvertisesTheResolvedBuildVersion is the fix for issue #116:
// the handler used to answer with a literal, so every client was told it was
// talking to 0.1.0 no matter what it was talking to.
func TestInitialize_AdvertisesTheResolvedBuildVersion(t *testing.T) {
	const want = "v0.2.0-148-g44ae23b"

	got := initializeServerInfo(t, &Server{build: buildinfo.Report{Version: want}})

	if got.Name != "ccvault" {
		t.Errorf("serverInfo.name = %q, want %q", got.Name, "ccvault")
	}
	if got.Version != want {
		t.Errorf("serverInfo.version = %q, want %q", got.Version, want)
	}
}

// TestInitialize_UnstampedBuildDoesNotInventAVersion covers the plain
// `go build` every contributor runs. Such a build has no release version to
// report, and the honest answer is the one `ccvault version` already gives --
// "dev" -- not a plausible-looking number no build ever was.
func TestInitialize_UnstampedBuildDoesNotInventAVersion(t *testing.T) {
	unstamped := buildinfo.Resolve(buildinfo.Stamps{}, nil)

	got := initializeServerInfo(t, &Server{build: unstamped})

	if got.Version != "dev" {
		t.Errorf("serverInfo.version = %q, want %q", got.Version, "dev")
	}
	if got.Version == "0.1.0" {
		t.Errorf("serverInfo.version is the hardcoded %q from issue #116", got.Version)
	}
}

// TestInitialize_NoBuildFactsAdvertisesUnknown pins what a Server carrying no
// build facts at all advertises. The field is required by the MCP schema, so
// it cannot be omitted; an empty string would read to a client as a version
// it failed to parse rather than one the server does not have.
func TestInitialize_NoBuildFactsAdvertisesUnknown(t *testing.T) {
	got := initializeServerInfo(t, &Server{})

	if got.Version != buildinfo.Unknown {
		t.Errorf("serverInfo.version = %q, want %q", got.Version, buildinfo.Unknown)
	}
}

// TestNewServer_CarriesTheBuildThroughToInitialize pins the constructor
// plumbing, which is the half that can silently rot: the resolver could be
// correct and the version still never reach the protocol field.
func TestNewServer_CarriesTheBuildThroughToInitialize(t *testing.T) {
	want := buildinfo.Report{Version: "9.9.9", Commit: "abc123", Date: "2026-10-08T12:34:56Z"}

	s, err := NewServer(nil, &config.Config{DataDir: t.TempDir()}, want)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if got := initializeServerInfo(t, s); got.Version != want.Version {
		t.Errorf("serverInfo.version = %q, want %q", got.Version, want.Version)
	}
}
