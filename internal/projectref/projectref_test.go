// ABOUTME: Unit tests for the four surface-class helpers.
// ABOUTME: Covers Label (Class A), Inline (Class B), Ref (Class C), ResolveAll (Class D).

package projectref

import (
	"reflect"
	"strings"
	"testing"

	"github.com/2389-research/ccvault/pkg/models"
)

// --- Label (Class A) -----------------------------------------------------

func TestLabel_UsesDisplayNameWhenPresent(t *testing.T) {
	p := &models.Project{Path: "/Users/dyl/work/ccvault", DisplayName: "friendly-name"}
	if got := Label(p); got != "friendly-name" {
		t.Errorf("Label = %q, want %q", got, "friendly-name")
	}
}

func TestLabel_FallsBackToBasenameWhenDisplayNameEmpty(t *testing.T) {
	p := &models.Project{Path: "/Users/dyl/work/ccvault", DisplayName: ""}
	if got := Label(p); got != "ccvault" {
		t.Errorf("Label = %q, want basename %q", got, "ccvault")
	}
}

func TestLabel_ReturnsEmptyForNilOrEmptyProject(t *testing.T) {
	if got := Label(nil); got != "" {
		t.Errorf("Label(nil) = %q, want empty", got)
	}
	if got := Label(&models.Project{}); got != "" {
		t.Errorf("Label(empty project) = %q, want empty", got)
	}
}

// --- Inline (Class B) ----------------------------------------------------

func TestInline_ShowsPathWhenLabelEqualsBasename(t *testing.T) {
	// If DisplayName is just the basename, "name (path)" would repeat
	// itself — show the path alone.
	p := &models.Project{Path: "/opt/proj/ccvault", DisplayName: "ccvault"}
	got := Inline(p)
	if got != "/opt/proj/ccvault" {
		t.Errorf("Inline = %q, want just the path", got)
	}
}

func TestInline_CombinesLabelAndPathWhenLabelIsCustom(t *testing.T) {
	// When the adapter provides a non-basename label (e.g. a VS Code
	// workspace title), show both so the caller sees the friendly name
	// AND has the disambiguating path.
	p := &models.Project{Path: "/opt/proj/x", DisplayName: "My Cool Project"}
	got := Inline(p)
	if !strings.Contains(got, "My Cool Project") {
		t.Errorf("Inline = %q, expected label to appear", got)
	}
	if !strings.Contains(got, "/opt/proj/x") {
		t.Errorf("Inline = %q, expected path to appear", got)
	}
}

func TestInline_TildeSubstitutesHomeCorrectly(t *testing.T) {
	// HOME is a genuine parent of the path — should abbreviate.
	t.Setenv("HOME", "/Users/testuser")
	p := &models.Project{Path: "/Users/testuser/work/ccvault", DisplayName: "ccvault"}
	got := Inline(p)
	if !strings.Contains(got, "~/work/ccvault") {
		t.Errorf("Inline = %q, expected ~/work/ccvault", got)
	}
	if strings.Contains(got, "/Users/testuser") {
		t.Errorf("Inline = %q, HOME prefix should have been abbreviated", got)
	}
}

func TestInline_TildeGuardsAgainstPrefixCollision(t *testing.T) {
	// HOME is a string prefix of the path but not its parent directory.
	// The abbreviation MUST NOT trigger — "/Users/dyl" and
	// "/Users/dylan/x" belong to different users.
	t.Setenv("HOME", "/Users/dyl")
	p := &models.Project{Path: "/Users/dylan/repo", DisplayName: "repo"}
	got := Inline(p)
	if strings.Contains(got, "~an/repo") {
		t.Errorf("Inline = %q, HOME prefix incorrectly substituted mid-segment", got)
	}
	if !strings.Contains(got, "/Users/dylan/repo") {
		t.Errorf("Inline = %q, expected the full path verbatim", got)
	}
}

func TestInline_EmptyPathReturnsLabelOnly(t *testing.T) {
	p := &models.Project{Path: "", DisplayName: "orphan"}
	if got := Inline(p); got != "orphan" {
		t.Errorf("Inline = %q, want just the label", got)
	}
}

// --- Ref (Class C) -------------------------------------------------------

func TestRef_AlwaysEmitsBothFields(t *testing.T) {
	p := &models.Project{Path: "/opt/x", DisplayName: "x"}
	got := Ref(p)
	want := map[string]any{"name": "x", "path": "/opt/x"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Ref = %v, want %v", got, want)
	}
}

func TestRef_NilProjectReturnsEmptyStructure(t *testing.T) {
	// Consumers should never need to nil-check individual fields —
	// even a nil project produces a well-formed object.
	got := Ref(nil)
	if got["name"] != "" || got["path"] != "" {
		t.Errorf("Ref(nil) = %v, want {name:'', path:''}", got)
	}
}

func TestRefsFromValues_PreservesOrder(t *testing.T) {
	projects := []models.Project{
		{Path: "/a", DisplayName: "one"},
		{Path: "/b", DisplayName: "two"},
		{Path: "/c", DisplayName: "three"},
	}
	got := RefsFromValues(projects)
	if len(got) != 3 {
		t.Fatalf("RefsFromValues len = %d, want 3", len(got))
	}
	for i, want := range []string{"one", "two", "three"} {
		if got[i]["name"] != want {
			t.Errorf("index %d: name = %v, want %s", i, got[i]["name"], want)
		}
	}
}

// --- ResolveAll (Class D) -----------------------------------------------

func TestResolveAll_ReturnsEveryMatchNotJustFirst(t *testing.T) {
	// Three projects share a basename — Class D must return ALL of them
	// so the caller can disambiguate rather than silently picking one.
	projects := []models.Project{
		{Path: "/Users/dyl/work/2389/ccvault", DisplayName: "ccvault"},
		{Path: "/Users/dyl/personal/ccvault", DisplayName: "ccvault"},
		{Path: "/tmp/ccvault-experiment", DisplayName: "ccvault-experiment"},
	}
	got := ResolveAll(projects, "ccvault")
	if len(got) != 3 {
		t.Errorf("ResolveAll returned %d matches, want 3 (all three contain 'ccvault')", len(got))
	}
}

func TestResolveAll_CaseInsensitive(t *testing.T) {
	projects := []models.Project{
		{Path: "/opt/CCVault", DisplayName: "CCVault"},
	}
	if got := ResolveAll(projects, "ccvault"); len(got) != 1 {
		t.Errorf("case-insensitive match failed: got %d, want 1", len(got))
	}
}

func TestResolveAll_MatchesEitherPathOrDisplayName(t *testing.T) {
	projects := []models.Project{
		{Path: "/opt/a", DisplayName: "friendly"},
		{Path: "/opt/friendly-place", DisplayName: "b"},
	}
	got := ResolveAll(projects, "friendly")
	if len(got) != 2 {
		t.Errorf("expected 2 matches (one by DisplayName, one by Path), got %d", len(got))
	}
}

func TestResolveAll_EmptyFilterReturnsNil(t *testing.T) {
	projects := []models.Project{{Path: "/a", DisplayName: "one"}}
	if got := ResolveAll(projects, ""); got != nil {
		t.Errorf("empty filter should return nil, got %v", got)
	}
}

func TestResolveAll_NoMatchReturnsEmpty(t *testing.T) {
	projects := []models.Project{{Path: "/a", DisplayName: "one"}}
	if got := ResolveAll(projects, "zzz"); len(got) != 0 {
		t.Errorf("no-match should return empty, got %v", got)
	}
}

func TestResolveAll_PreservesInputOrder(t *testing.T) {
	projects := []models.Project{
		{Path: "/z-first", DisplayName: "match"},
		{Path: "/a-second", DisplayName: "match"},
	}
	got := ResolveAll(projects, "match")
	if len(got) != 2 || got[0].Path != "/z-first" || got[1].Path != "/a-second" {
		t.Errorf("order not preserved: got %v", got)
	}
}
