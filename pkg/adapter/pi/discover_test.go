// ABOUTME: Tests for pi session discovery across pi's own layout and bb's flat bridge directory.
// ABOUTME: Covers project paths read from the session header and the files Discover declines.

package pi

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// header returns a pi session header line for cwd. The field order and the
// inline provider/modelId/thinkingLevel are what pi writes.
func header(id, cwd string) string {
	return `{"type":"session","id":"` + id + `","timestamp":"2025-12-09T00:53:29.825Z","cwd":"` + cwd +
		`","provider":"anthropic","modelId":"claude-opus-4-5","thinkingLevel":"off"}`
}

// writeSession writes one JSONL session file, creating its directory, with a
// trailing newline on every line the way pi appends them.
func writeSession(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := ""
	for _, line := range lines {
		body += line + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestDiscover_PiLayout walks the directory pi itself writes to:
// <root>/agent/sessions/<encoded cwd>/<timestamp>_<id>.jsonl.
func TestDiscover_PiLayout(t *testing.T) {
	root := t.TempDir()
	sessions := filepath.Join(root, "agent", "sessions")

	writeSession(t, filepath.Join(sessions, "--Users-dyl-work-alpha--", "2025-12-09T00-53-29-825Z_a1.jsonl"),
		header("a1", "/Users/dyl/work/alpha"))
	writeSession(t, filepath.Join(sessions, "--Users-dyl-work-beta--", "2025-12-09T01-00-00-000Z_b2.jsonl"),
		header("b2", "/Users/dyl/work/beta"))

	files, err := New().Discover(root)
	if err != nil {
		t.Fatalf("Discover() error: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("Discover() returned %d files, want 2", len(files))
	}

	got := map[string]string{}
	for _, sf := range files {
		got[filepath.Base(sf.Path)] = sf.ProjectPath
		if sf.ModTime.IsZero() {
			t.Errorf("%s: ModTime is zero", sf.Path)
		}
	}
	want := map[string]string{
		"2025-12-09T00-53-29-825Z_a1.jsonl": "/Users/dyl/work/alpha",
		"2025-12-09T01-00-00-000Z_b2.jsonl": "/Users/dyl/work/beta",
	}
	for name, wantPath := range want {
		if got[name] != wantPath {
			t.Errorf("ProjectPath for %s = %q, want %q", name, got[name], wantPath)
		}
	}
}

// TestDiscover_ProjectPathComesFromTheHeaderNotTheDirectoryName is the reason
// Discover opens every file it finds. pi encodes the cwd into the directory
// name by replacing every separator with a dash, so a path that already
// contains a dash decodes to a different path than the one pi was run in —
// "/Users/dyl/work/pi-mono" and "/Users/dyl/work/pi/mono" share one directory
// name. The header carries the cwd verbatim, so nothing has to be decoded.
func TestDiscover_ProjectPathComesFromTheHeaderNotTheDirectoryName(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "agent", "sessions", "--Users-dyl-work-pi-mono--")
	writeSession(t, filepath.Join(dir, "s.jsonl"), header("s", "/Users/dyl/work/pi-mono"))

	files, err := New().Discover(root)
	if err != nil {
		t.Fatalf("Discover() error: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("Discover() returned %d files, want 1", len(files))
	}
	if files[0].ProjectPath != "/Users/dyl/work/pi-mono" {
		t.Errorf("ProjectPath = %q, want %q", files[0].ProjectPath, "/Users/dyl/work/pi-mono")
	}
}

// TestDiscover_FlatRootIsTheSessionsDir covers bb, a thread UI that drives pi
// and writes the same format into ~/.bb/pi-bridge-sessions with no
// agent/sessions beneath it and no per-project directory. One rule covers
// both: pi's own layout when it is there, otherwise the configured root is
// itself the sessions directory.
func TestDiscover_FlatRootIsTheSessionsDir(t *testing.T) {
	root := t.TempDir()
	writeSession(t, filepath.Join(root, "01999-abc.jsonl"), header("abc", "/Users/dyl/work/gamma"))
	writeSession(t, filepath.Join(root, "01999-def.jsonl"), header("def", "/Users/dyl/work/delta"))

	files, err := New().Discover(root)
	if err != nil {
		t.Fatalf("Discover() error: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("Discover() returned %d files, want 2", len(files))
	}
	paths := []string{files[0].ProjectPath, files[1].ProjectPath}
	sort.Strings(paths)
	if paths[0] != "/Users/dyl/work/delta" || paths[1] != "/Users/dyl/work/gamma" {
		t.Errorf("ProjectPaths = %v, want [/Users/dyl/work/delta /Users/dyl/work/gamma]", paths)
	}
}

// TestDiscover_FollowsASymlinkedProjectDirectory covers a layout pi itself
// supports and goes out of its way to support: its own session listing keeps a
// child of the sessions directory when it `isDirectory() || isSymbolicLink()`,
// so a project directory may be a link to somewhere else on disk.
//
// filepath.Walk does not descend into symlinked directories, so the obvious
// implementation finds nothing here — and finds it silently, which is the
// reason this test exists. Sessions missing from the archive look exactly like
// sessions that were never there.
func TestDiscover_FollowsASymlinkedProjectDirectory(t *testing.T) {
	root := t.TempDir()
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	writeSession(t, filepath.Join(elsewhere, "s.jsonl"), header("sym1", "/Users/dyl/work/linked"))

	sessions := filepath.Join(root, "agent", "sessions")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(sessions, "--Users-dyl-work-linked--")
	if err := os.Symlink(elsewhere, link); err != nil {
		t.Fatal(err)
	}

	files, err := New().Discover(root)
	if err != nil {
		t.Fatalf("Discover() error: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("found %d sessions behind a symlinked project directory, want 1", len(files))
	}
	// The path is reported through the link, not through its target, so the
	// archive records the file where the configured root actually holds it.
	if want := filepath.Join(link, "s.jsonl"); files[0].Path != want {
		t.Errorf("Path = %q, want %q", files[0].Path, want)
	}
	if files[0].ProjectPath != "/Users/dyl/work/linked" {
		t.Errorf("ProjectPath = %q, want %q", files[0].ProjectPath, "/Users/dyl/work/linked")
	}
}

// TestDiscover_ASessionReachableTwoWaysIsFoundOnce covers the convenience link
// somebody will eventually make — sessions/current -> sessions/--some-project--
// — which leaves every file in that project reachable both directly and
// through the link.
//
// Reporting it twice would give one session two source files, so each sync
// would rewrite the same rows from whichever path it reached last and the
// incremental mtime check would never settle.
func TestDiscover_ASessionReachableTwoWaysIsFoundOnce(t *testing.T) {
	root := t.TempDir()
	sessions := filepath.Join(root, "agent", "sessions")
	writeSession(t, filepath.Join(sessions, "--Users-dyl-work-alpha--", "s.jsonl"),
		header("a1", "/Users/dyl/work/alpha"))
	if err := os.Symlink(filepath.Join(sessions, "--Users-dyl-work-alpha--"),
		filepath.Join(sessions, "current")); err != nil {
		t.Fatal(err)
	}

	files, err := New().Discover(root)
	if err != nil {
		t.Fatalf("Discover() error: %v", err)
	}
	if len(files) != 1 {
		paths := make([]string, len(files))
		for i, f := range files {
			paths[i] = f.Path
		}
		t.Fatalf("Discover() returned %d files, want 1: %v", len(files), paths)
	}
}

// TestDiscover_ABrokenSymlinkIsSkipped because a dangling link is not a
// session directory and must not fail the whole source's discovery.
func TestDiscover_ABrokenSymlinkIsSkipped(t *testing.T) {
	root := t.TempDir()
	sessions := filepath.Join(root, "agent", "sessions")
	writeSession(t, filepath.Join(sessions, "--Users-dyl-work-alpha--", "s.jsonl"),
		header("a1", "/Users/dyl/work/alpha"))
	if err := os.Symlink(filepath.Join(t.TempDir(), "gone"), filepath.Join(sessions, "--dangling--")); err != nil {
		t.Fatal(err)
	}

	files, err := New().Discover(root)
	if err != nil {
		t.Fatalf("Discover() error: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("Discover() returned %d files, want 1", len(files))
	}
}

func TestDiscover_SkipsNonJSONLFiles(t *testing.T) {
	root := t.TempDir()
	sessions := filepath.Join(root, "agent", "sessions", "--Users-dyl-work-alpha--")
	writeSession(t, filepath.Join(sessions, "s.jsonl"), header("s", "/Users/dyl/work/alpha"))
	writeSession(t, filepath.Join(sessions, "notes.txt"), "not a session")
	writeSession(t, filepath.Join(sessions, "s.jsonl.tmp"), header("t", "/Users/dyl/work/alpha"))

	files, err := New().Discover(root)
	if err != nil {
		t.Fatalf("Discover() error: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("Discover() returned %d files, want 1", len(files))
	}
}

// TestDiscover_SkipsAFileWithNoSessionHeader keeps Discover and Parse agreeing
// on which files are sessions. A file with no header has no id for Parse to
// key a session row on, so returning it here would only produce a parse that
// sync then discards.
func TestDiscover_SkipsAFileWithNoSessionHeader(t *testing.T) {
	root := t.TempDir()
	sessions := filepath.Join(root, "agent", "sessions", "--Users-dyl-work-alpha--")
	writeSession(t, filepath.Join(sessions, "good.jsonl"), header("good", "/Users/dyl/work/alpha"))
	writeSession(t, filepath.Join(sessions, "headerless.jsonl"),
		`{"type":"message","timestamp":"2025-12-09T00:53:30.000Z","message":{"role":"user","content":[{"type":"text","text":"hi"}],"timestamp":1765233665292}}`)
	writeSession(t, filepath.Join(sessions, "garbage.jsonl"), "{not json")

	files, err := New().Discover(root)
	if err != nil {
		t.Fatalf("Discover() error: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("Discover() returned %d files, want 1", len(files))
	}
	if filepath.Base(files[0].Path) != "good.jsonl" {
		t.Errorf("kept %q, want good.jsonl", filepath.Base(files[0].Path))
	}
}

func TestDiscover_MissingRootIsNotAnError(t *testing.T) {
	files, err := New().Discover(filepath.Join(t.TempDir(), "no-such-pi-home"))
	if err != nil {
		t.Fatalf("Discover() error: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("Discover() returned %d files, want 0", len(files))
	}
}
