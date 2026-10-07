// ABOUTME: Harness for driving the real ccvault binary against a throwaway archive
// ABOUTME: Builds cmd/ccvault once per run and gives each test its own data dir, HOME, and synthetic Claude home

package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The built binary is shared by every test in this package; TestMain removes
// its directory afterwards. Building once keeps a package full of CLI tests
// from paying the link cost per test.
var (
	binDir    string
	buildOnce sync.Once
	binPath   string
	buildErr  error
	buildOut  string
)

func TestMain(m *testing.M) {
	code := m.Run()
	if binDir != "" {
		_ = os.RemoveAll(binDir)
	}
	os.Exit(code)
}

// ccvaultBinary builds cmd/ccvault and returns the path to the executable.
func ccvaultBinary(t *testing.T) string {
	t.Helper()

	// Resolved outside the Once: repoRoot fails the test on error, and a
	// Once marked done by an aborted call would leave every later test with
	// an empty binary path and no error to explain it.
	root := repoRoot(t)

	buildOnce.Do(func() {
		binDir, buildErr = os.MkdirTemp("", "ccvault-cli-")
		if buildErr != nil {
			return
		}
		binPath = filepath.Join(binDir, "ccvault")
		//nolint:gosec // G204: binPath is a path this test just created under os.TempDir
		cmd := exec.Command("go", "build", "-o", binPath, "github.com/2389-research/ccvault/cmd/ccvault")
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		buildOut, buildErr = string(out), err
	})
	if buildErr != nil {
		t.Fatalf("build ccvault: %v\n%s", buildErr, buildOut)
	}
	return binPath
}

// repoRoot walks up from the test's working directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod found above %s", dir)
		}
		dir = parent
	}
}

// cliFixture is a throwaway ccvault archive plus the binary to drive it.
//
// Everything it touches lives under one t.TempDir(): the data directory the
// --data-dir flag names, a synthetic Claude home holding session JSONL, and a
// HOME the child process inherits. The real archive at $HOME/.ccvault is
// unreachable from here — HOME is redirected and CCVAULT_* variables are
// stripped from the child environment — which is what makes it safe to run
// destructive commands such as sync --rebuild and vacuum.
type cliFixture struct {
	// DataDir is the archive directory; passed as --data-dir on every run.
	DataDir string
	// ClaudeHome is the synthetic source tree the fixture config scans.
	ClaudeHome string
	// Home is the HOME the child process sees, so DefaultDataDir() resolves
	// to Home/.ccvault rather than the real one.
	Home string

	bin string
	env []string
}

// newCLIFixture builds the binary and lays out an archive holding one
// Claude Code project with one two-turn session.
func newCLIFixture(t *testing.T) *cliFixture {
	t.Helper()

	root := t.TempDir()
	f := &cliFixture{
		DataDir:    filepath.Join(root, "data"),
		ClaudeHome: filepath.Join(root, "claude"),
		Home:       filepath.Join(root, "home"),
		bin:        ccvaultBinary(t),
	}

	for _, dir := range []string{f.DataDir, f.Home} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	f.AddSession(t, f.ClaudeHome, "-Users-test-fixture", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "fixture session one")

	// config.toml lives inside the data directory, which is where the
	// --data-dir override points the config search.
	f.WriteConfig(t, filepath.Join(f.DataDir, "config.toml"), f.ClaudeHome)

	// Strip CCVAULT_* so an exported variable in the developer's shell
	// cannot redirect a fixture run, and redirect HOME so the default data
	// dir resolves inside the fixture.
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "CCVAULT_") || strings.HasPrefix(kv, "HOME=") {
			continue
		}
		f.env = append(f.env, kv)
	}
	f.env = append(f.env, "HOME="+f.Home)

	return f
}

// WriteConfig writes a ccvault config file pointing claude-code at claudeHome.
func (f *cliFixture) WriteConfig(t *testing.T, path, claudeHome string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	body := fmt.Sprintf("claude_home = %q\n", claudeHome)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// AddSession writes a two-turn Claude Code session under claudeHome.
func (f *cliFixture) AddSession(t *testing.T, claudeHome, encodedProject, sessionID, text string) {
	t.Helper()

	dir := filepath.Join(claudeHome, "projects", encodedProject)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	jsonl := fmt.Sprintf(`{"uuid":"%[1]s-t1","parentUuid":null,"type":"user","message":{"role":"user","content":%[2]q},"timestamp":"2026-01-01T00:00:01Z","sessionId":%[1]q,"cwd":"/Users/test/fixture","version":"1.0"}
{"uuid":"%[1]s-t2","parentUuid":"%[1]s-t1","type":"assistant","message":{"id":"%[1]s-m1","model":"claude-sonnet-4-20250514","role":"assistant","content":[{"type":"text","text":"acknowledged"}],"usage":{"input_tokens":11,"output_tokens":7}},"timestamp":"2026-01-01T00:00:02Z","sessionId":%[1]q}
`, sessionID, text)
	path := filepath.Join(dir, sessionID+".jsonl")
	if err := os.WriteFile(path, []byte(jsonl), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// cliResult is the outcome of one command run.
type cliResult struct {
	Args   []string
	Stdout string
	Stderr string
	Err    error
}

// JSON decodes Stdout into v, failing the test if it is not valid JSON.
func (r cliResult) JSON(t *testing.T, v interface{}) {
	t.Helper()

	if err := json.Unmarshal([]byte(r.Stdout), v); err != nil {
		t.Fatalf("ccvault %s: stdout is not JSON: %v\n%s", strings.Join(r.Args, " "), err, r.Stdout)
	}
}

// TryRun runs the binary with --data-dir prepended and returns the result,
// error and all. Stdin is /dev/null, so commands that refuse to act
// non-interactively take that path rather than blocking.
func (f *cliFixture) TryRun(t *testing.T, args ...string) cliResult {
	t.Helper()

	full := append([]string{"--data-dir", f.DataDir}, args...)
	//nolint:gosec // G204: f.bin is the binary this harness built; args come from the test
	cmd := exec.Command(f.bin, full...)
	cmd.Env = f.env
	cmd.Dir = f.Home // keep the "." config search path out of the repo

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	return cliResult{Args: full, Stdout: stdout.String(), Stderr: stderr.String(), Err: err}
}

// Run is TryRun with a non-zero exit treated as a test failure.
func (f *cliFixture) Run(t *testing.T, args ...string) cliResult {
	t.Helper()

	r := f.TryRun(t, args...)
	if r.Err != nil {
		t.Fatalf("ccvault %s failed: %v\nstdout:\n%s\nstderr:\n%s",
			strings.Join(r.Args, " "), r.Err, r.Stdout, r.Stderr)
	}
	return r
}

// TestCLI_EndToEndAgainstThrowawayDataDir drives the real binary through the
// commands whose CLI paths had no coverage before --data-dir existed.
func TestCLI_EndToEndAgainstThrowawayDataDir(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	f := newCLIFixture(t)

	syncOut := f.Run(t, "sync")
	if !strings.Contains(syncOut.Stdout, "Sessions:  1 indexed") {
		t.Errorf("sync did not index the fixture session:\n%s", syncOut.Stdout)
	}

	// The database must land in the fixture, not anywhere near the default.
	if _, err := os.Stat(filepath.Join(f.DataDir, "ccvault.db")); err != nil {
		t.Fatalf("no database in the fixture data dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.Home, ".ccvault")); !os.IsNotExist(err) {
		t.Errorf("the default data dir under the fixture HOME should not exist, stat gave %v", err)
	}

	var stats struct {
		Sessions    int `json:"sessions"`
		Turns       int `json:"turns"`
		SearchIndex struct {
			Consistent bool `json:"consistent"`
			Indexed    int  `json:"indexed"`
		} `json:"search_index"`
	}
	f.Run(t, "stats", "--json").JSON(t, &stats)
	if stats.Sessions != 1 || stats.Turns != 2 {
		t.Errorf("stats --json: sessions=%d turns=%d, want 1 and 2", stats.Sessions, stats.Turns)
	}
	if !stats.SearchIndex.Consistent || stats.SearchIndex.Indexed != 2 {
		t.Errorf("stats --json search_index = %+v, want 2 indexed and consistent", stats.SearchIndex)
	}

	var orient struct {
		Status   string `json:"status"`
		Database struct {
			Sessions int `json:"sessions"`
			Projects int `json:"projects"`
		} `json:"database"`
	}
	f.Run(t, "orient", "--json").JSON(t, &orient)
	if orient.Status != "ready" || orient.Database.Sessions != 1 || orient.Database.Projects != 1 {
		t.Errorf("orient --json = %+v, want ready with 1 session in 1 project", orient)
	}

	search := f.Run(t, "search", "fixture session one")
	if !strings.Contains(search.Stdout, "Found 1 results") {
		t.Errorf("search did not find the fixture turn:\n%s", search.Stdout)
	}

	var vacuum struct {
		Database string `json:"database"`
		After    struct {
			FreelistCount int64 `json:"freelist_count"`
		} `json:"after"`
	}
	f.Run(t, "vacuum", "--json").JSON(t, &vacuum)
	if vacuum.Database != filepath.Join(f.DataDir, "ccvault.db") {
		t.Errorf("vacuum compacted %q, want the fixture database", vacuum.Database)
	}
	if vacuum.After.FreelistCount != 0 {
		t.Errorf("after vacuum freelist_count = %d, want 0", vacuum.After.FreelistCount)
	}

	// The archive still reads correctly after the file was rewritten.
	f.Run(t, "stats", "--json").JSON(t, &stats)
	if stats.Sessions != 1 || stats.Turns != 2 {
		t.Errorf("post-vacuum stats: sessions=%d turns=%d, want 1 and 2", stats.Sessions, stats.Turns)
	}
}

// TestCLI_DataDirIgnoresConfigInDefaultDataDir is the behavioural proof that
// --data-dir replaces the config search path. A config.toml is planted in the
// default data dir pointing at a decoy source tree; if the override merely
// prepended its own directory, viper would still find and read that file.
func TestCLI_DataDirIgnoresConfigInDefaultDataDir(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	f := newCLIFixture(t)

	decoy := filepath.Join(f.Home, "decoy-claude")
	f.AddSession(t, decoy, "-Users-test-decoy", "dddddddd-dddd-dddd-dddd-dddddddddddd", "decoy session text")
	f.WriteConfig(t, filepath.Join(f.Home, ".ccvault", "config.toml"), decoy)

	f.Run(t, "sync")

	decoyHit := f.Run(t, "search", "decoy session text")
	if !strings.Contains(decoyHit.Stdout, "No results found") {
		t.Errorf("the config in the default data dir was read after all:\n%s", decoyHit.Stdout)
	}
	fixtureHit := f.Run(t, "search", "fixture session one")
	if !strings.Contains(fixtureHit.Stdout, "Found 1 results") {
		t.Errorf("the config in the overridden data dir was not read:\n%s", fixtureHit.Stdout)
	}
}

// TestCLI_DataDirFlagBeatsEnv pins the top two rungs of the precedence order
// through the real binary rather than through the config package alone.
func TestCLI_DataDirFlagBeatsEnv(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	f := newCLIFixture(t)

	envDir := filepath.Join(f.Home, "from-env")
	f.env = append(f.env, "CCVAULT_DATA_DIR="+envDir)

	f.Run(t, "sync")

	if _, err := os.Stat(filepath.Join(f.DataDir, "ccvault.db")); err != nil {
		t.Errorf("--data-dir lost to CCVAULT_DATA_DIR: %v", err)
	}
	if _, err := os.Stat(envDir); !os.IsNotExist(err) {
		t.Errorf("CCVAULT_DATA_DIR directory should never have been created, stat gave %v", err)
	}
}

// TestCLI_ConfigFlagReadsNamedFile covers --config: a settings file that
// lives nowhere near the data directory.
func TestCLI_ConfigFlagReadsNamedFile(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	f := newCLIFixture(t)

	elsewhere := filepath.Join(f.Home, "elsewhere", "ccvault.toml")
	other := filepath.Join(f.Home, "other-claude")
	f.AddSession(t, other, "-Users-test-other", "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", "named config session")
	f.WriteConfig(t, elsewhere, other)

	f.Run(t, "--config", elsewhere, "sync")

	hit := f.Run(t, "--config", elsewhere, "search", "named config session")
	if !strings.Contains(hit.Stdout, "Found 1 results") {
		t.Errorf("--config file was not used:\n%s", hit.Stdout)
	}
	// The data dir config.toml must not have been consulted as well.
	skipped := f.Run(t, "--config", elsewhere, "search", "fixture session one")
	if !strings.Contains(skipped.Stdout, "No results found") {
		t.Errorf("--config did not replace the data dir config:\n%s", skipped.Stdout)
	}
}

// TestCLI_ErrorsPrintOnce pins error output to one emission per failure.
// Cobra's handler prints the error ahead of the usage block; a second print
// anywhere else doubles the message on every failing command, which is easy
// to reintroduce and invisible to every other test here.
func TestCLI_ErrorsPrintOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	f := newCLIFixture(t)

	// Each case fails on a different path: a RunE return, flag parsing, and
	// command lookup. Cobra reports all three, and all three used to double.
	cases := []struct {
		name string
		args []string
		// needle appears exactly once per emission of the error.
		needle string
	}{
		{
			name:   "command error",
			args:   []string{"--config", filepath.Join(f.Home, "absent.toml"), "stats"},
			needle: "load config: read config",
		},
		{
			name:   "unknown flag",
			args:   []string{"stats", "--bogus-flag"},
			needle: "unknown flag: --bogus-flag",
		},
		{
			name:   "unknown command",
			args:   []string{"nosuchcommand"},
			needle: `unknown command "nosuchcommand"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := f.TryRun(t, tc.args...)
			if r.Err == nil {
				t.Fatalf("expected a non-zero exit\nstdout:\n%s\nstderr:\n%s", r.Stdout, r.Stderr)
			}
			if got := strings.Count(r.Stderr, tc.needle); got != 1 {
				t.Errorf("stderr names %q %d times, want exactly 1:\n%s", tc.needle, got, r.Stderr)
			}
			if strings.Contains(r.Stdout, tc.needle) {
				t.Errorf("errors belong on stderr, but stdout carries %q:\n%s", tc.needle, r.Stdout)
			}
		})
	}
}

// TestCLI_MissingConfigFileIsAnError keeps a typo'd --config from silently
// falling back to defaults.
func TestCLI_MissingConfigFileIsAnError(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	f := newCLIFixture(t)

	r := f.TryRun(t, "--config", filepath.Join(f.Home, "absent.toml"), "stats")
	if r.Err == nil {
		t.Fatalf("expected a non-zero exit for a missing --config file\nstdout:\n%s", r.Stdout)
	}
	if !strings.Contains(r.Stderr, "absent.toml") {
		t.Errorf("stderr should name the missing file, got:\n%s", r.Stderr)
	}
}
