// ABOUTME: Drives the real binary's MCP server and compares what it advertises to `ccvault version`
// ABOUTME: Pins issue #116 -- initialize used to answer with a hardcoded 0.1.0 for every build

package integration

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// mcpInitialize sends one initialize request to `ccvault mcp` over stdin and
// returns the serverInfo from the response. Closing stdin after the request
// is what shuts the server down, so this needs no timeout or signal handling.
func mcpInitialize(t *testing.T, f *cliFixture) (name, version string) {
	t.Helper()

	const request = `{"jsonrpc":"2.0","id":1,"method":"initialize",` +
		`"params":{"protocolVersion":"2024-11-05","capabilities":{},` +
		`"clientInfo":{"name":"integration","version":"1"}}}` + "\n"

	//nolint:gosec // G204: f.bin is the binary this harness built; the data dir is under t.TempDir()
	cmd := exec.Command(f.bin, "--data-dir", f.DataDir, "mcp")
	cmd.Env = f.env
	cmd.Dir = f.Home
	cmd.Stdin = strings.NewReader(request)

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("ccvault mcp: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}

	var resp struct {
		Result struct {
			ServerInfo struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	line := strings.TrimSpace(stdout.String())
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		t.Fatalf("initialize response is not JSON: %v\n%s", err, line)
	}
	if resp.Result.ServerInfo.Version == "" {
		t.Fatalf("initialize response carries no serverInfo.version: %s", line)
	}
	return resp.Result.ServerInfo.Name, resp.Result.ServerInfo.Version
}

// TestMCP_InitializeAdvertisesTheSameVersionAsTheCLI is the end-to-end form of
// issue #116. The two halves of one binary have to agree: whatever
// `ccvault version` resolves is what an MCP client gets told, so a client
// deciding which tools and response fields to expect is reading a fact rather
// than a literal somebody forgot to update.
//
// This asserts agreement rather than a specific string, because which version
// an unstamped build resolves depends on how it was built.
func TestMCP_InitializeAdvertisesTheSameVersionAsTheCLI(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	f := newCLIFixture(t)

	cliVersion, _ := versionFields(t, f.bin)
	name, mcpVersion := mcpInitialize(t, f)

	if name != "ccvault" {
		t.Errorf("serverInfo.name = %q, want %q", name, "ccvault")
	}
	if mcpVersion != cliVersion {
		t.Errorf("MCP advertises %q but `ccvault version` reports %q", mcpVersion, cliVersion)
	}
	if mcpVersion == "0.1.0" {
		t.Errorf("MCP advertises the hardcoded %q from issue #116", mcpVersion)
	}
}

// TestMCP_InitializeCarriesReleaseStamps covers the case the hardcoded literal
// hurt most: a real release, where the version a client is told is the one it
// would use to decide what the server can do.
func TestMCP_InitializeCarriesReleaseStamps(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}

	const wantVersion = "9.9.9"
	f := newCLIFixture(t)
	f.bin = buildStamped(t, "-X", "main.version="+wantVersion)

	if _, got := mcpInitialize(t, f); got != wantVersion {
		t.Errorf("serverInfo.version = %q, want the stamped %q", got, wantVersion)
	}
}
