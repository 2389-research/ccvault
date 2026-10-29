// ABOUTME: Unit tests for the Class C enrichment helpers and the path/ID lookups.
// ABOUTME: Covers EnrichedRef, SessionRef's omitempty contract, LabelFromPath, and the By* maps.

package projectref

import (
	"testing"
	"time"

	"github.com/2389-research/ccvault/pkg/models"
)

// --- EnrichedRef (Class C) -----------------------------------------------

// The point of EnrichedRef over Ref is that it carries BOTH the doctrine
// `name` (which falls back to the basename) AND the raw `display_name`
// (which does not). A consumer keying on `display_name` must still see the
// empty string when the adapter supplied none, while `name` must have
// resolved to something renderable. Collapsing the two -- emitting the raw
// DisplayName as `name`, or the resolved Label as `display_name` -- is the
// mistake this guards, and it is invisible if you only test a project that
// has a DisplayName set.
func TestEnrichedRef_NameFallsBackButDisplayNameStaysRaw(t *testing.T) {
	p := &models.Project{Path: "/Users/dyl/work/ccvault", DisplayName: ""}
	got := EnrichedRef(p)

	if got["name"] != "ccvault" {
		t.Errorf("name = %v, want basename %q (Label's fallback)", got["name"], "ccvault")
	}
	if got["display_name"] != "" {
		t.Errorf("display_name = %v, want the raw empty value, not the resolved label", got["display_name"])
	}
}

func TestEnrichedRef_PreservesDisplayNameWhenAdapterSuppliedOne(t *testing.T) {
	p := &models.Project{Path: "/Users/dyl/work/x", DisplayName: "branded"}
	got := EnrichedRef(p)

	if got["name"] != "branded" {
		t.Errorf("name = %v, want %q", got["name"], "branded")
	}
	if got["display_name"] != "branded" {
		t.Errorf("display_name = %v, want %q", got["display_name"], "branded")
	}
}

// Every operational field must survive the trip into the map, under the key
// agents read it by. A test that only checked name/path would pass against
// an EnrichedRef that silently dropped the counts -- which is the whole
// reason this helper exists rather than Ref.
func TestEnrichedRef_CarriesEveryOperationalField(t *testing.T) {
	first := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	last := time.Date(2026, 6, 7, 8, 9, 10, 0, time.UTC)
	p := &models.Project{
		ID:             42,
		Path:           "/opt/proj",
		DisplayName:    "proj",
		FirstSeenAt:    first,
		LastActivityAt: last,
		SessionCount:   17,
		TotalTokens:    123456,
		Source:         "claude-code",
	}
	got := EnrichedRef(p)

	want := map[string]any{
		"id":               int64(42),
		"name":             "proj",
		"path":             "/opt/proj",
		"display_name":     "proj",
		"first_seen_at":    first,
		"last_activity_at": last,
		"session_count":    17,
		"total_tokens":     int64(123456),
		"source":           "claude-code",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("key %q = %#v, want %#v", k, got[k], v)
		}
	}
	// A key appearing that nothing documents is also a contract change.
	if len(got) != len(want) {
		t.Errorf("EnrichedRef emitted %d keys, want exactly %d: %v", len(got), len(want), got)
	}
}

// Ref(nil) is documented as a well-formed object so consumers never
// nil-check individual fields; EnrichedRef must honour the same promise
// rather than panicking or returning nil.
func TestEnrichedRef_NilProjectReturnsWellFormedRef(t *testing.T) {
	got := EnrichedRef(nil)
	if got == nil {
		t.Fatal("EnrichedRef(nil) = nil, want a non-nil map")
	}
	if got["name"] != "" || got["path"] != "" {
		t.Errorf("EnrichedRef(nil) = %v, want {name:'', path:''}", got)
	}
}

func TestEnrichedRefsFromValues_KeepsRowsDistinctAndOrdered(t *testing.T) {
	projects := []models.Project{
		{ID: 1, Path: "/a", DisplayName: "one", SessionCount: 10},
		{ID: 2, Path: "/b", DisplayName: "two", SessionCount: 20},
		{ID: 3, Path: "/c", DisplayName: "three", SessionCount: 30},
	}
	got := EnrichedRefsFromValues(projects)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	// Taking the address of a range variable instead of &projects[i] yields
	// three refs describing the last project. Asserting per-row values (not
	// just the length) is what distinguishes the two.
	for i, want := range []struct {
		id    int64
		name  string
		count int
	}{{1, "one", 10}, {2, "two", 20}, {3, "three", 30}} {
		if got[i]["id"] != want.id || got[i]["name"] != want.name || got[i]["session_count"] != want.count {
			t.Errorf("row %d = {id:%v name:%v session_count:%v}, want {id:%d name:%s session_count:%d}",
				i, got[i]["id"], got[i]["name"], got[i]["session_count"], want.id, want.name, want.count)
		}
	}
}

func TestEnrichedRefsFromValues_EmptyInputIsEmptyNotNil(t *testing.T) {
	got := EnrichedRefsFromValues(nil)
	if got == nil {
		t.Fatal("got nil, want an empty slice so it marshals as [] not null")
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}

// --- SessionRef (Class C) ------------------------------------------------

// The omitempty contract, which is the subtle half of SessionRef. Agents
// check presence (`if resp.model`), so an empty-but-present key reads as a
// real value. These four fields carry `omitempty` on models.Session and must
// be ABSENT, not empty, when zero -- emitting them as "" would be invisible
// to any test that only read the populated case.
func TestSessionRef_OmitsEmptyOmitemptyFields(t *testing.T) {
	s := &models.Session{
		ID:        "sess-1",
		ProjectID: 7,
		StartedAt: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		// ProjectPath, EndedAt, Model, GitBranch all deliberately zero.
	}
	got := SessionRef(s, nil)

	for _, key := range []string{"project_path", "ended_at", "model", "git_branch"} {
		if _, present := got[key]; present {
			t.Errorf("key %q is present with value %#v; an omitempty field must be absent when zero, "+
				"because agents presence-check it", key, got[key])
		}
	}
}

func TestSessionRef_IncludesOmitemptyFieldsWhenSet(t *testing.T) {
	ended := time.Date(2026, 3, 1, 1, 0, 0, 0, time.UTC)
	s := &models.Session{
		ID:          "sess-2",
		ProjectID:   7,
		ProjectPath: "/opt/proj",
		StartedAt:   time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		EndedAt:     ended,
		Model:       "claude-opus-4",
		GitBranch:   "main",
	}
	got := SessionRef(s, nil)

	if got["project_path"] != "/opt/proj" {
		t.Errorf("project_path = %#v, want /opt/proj", got["project_path"])
	}
	if got["ended_at"] != ended {
		t.Errorf("ended_at = %#v, want %v", got["ended_at"], ended)
	}
	if got["model"] != "claude-opus-4" {
		t.Errorf("model = %#v, want claude-opus-4", got["model"])
	}
	if got["git_branch"] != "main" {
		t.Errorf("git_branch = %#v, want main", got["git_branch"])
	}
}

// parent_session_id is documented as always present, and nil rather than ""
// for a top-level session, so a script reading a default listing can see
// that subagent rows were held back. "" would be a third state to learn.
func TestSessionRef_ParentSessionIDIsPresentAndNilForTopLevel(t *testing.T) {
	s := &models.Session{ID: "top", ProjectID: 1, SubagentCount: 3}
	got := SessionRef(s, nil)

	v, present := got["parent_session_id"]
	if !present {
		t.Fatal("parent_session_id is absent; it must always be present so a reader can see rows were held back")
	}
	if v != nil {
		t.Errorf("parent_session_id = %#v, want nil for a top-level session (not an empty string)", v)
	}
	if got["subagent_count"] != 3 {
		t.Errorf("subagent_count = %#v, want 3", got["subagent_count"])
	}
}

func TestSessionRef_ParentSessionIDCarriesTheParentWhenSet(t *testing.T) {
	s := &models.Session{ID: "child", ProjectID: 1, ParentSessionID: "parent-uuid"}
	got := SessionRef(s, nil)

	if got["parent_session_id"] != "parent-uuid" {
		t.Errorf("parent_session_id = %#v, want parent-uuid", got["parent_session_id"])
	}
}

// The reason the projectsByID lookup exists: a session knows its project's
// path but not its adapter-provided label. Without the lookup, project_name
// falls through to the basename and jeff/hex/nanoclaw branding is silently
// lost. Note the path basename here differs from the brand, so a helper
// that ignored the lookup would produce "repo" and fail.
func TestSessionRef_PrefersAdapterDisplayNameFromLookup(t *testing.T) {
	s := &models.Session{ID: "s", ProjectID: 9, ProjectPath: "/srv/repo"}
	byID := map[int64]*models.Project{
		9: {ID: 9, Path: "/srv/repo", DisplayName: "jeff-soma"},
	}
	if got := SessionRef(s, byID)["project_name"]; got != "jeff-soma" {
		t.Errorf("project_name = %#v, want the adapter label %q, not the path basename", got, "jeff-soma")
	}
}

func TestSessionRef_FallsBackToBasenameWhenLookupMisses(t *testing.T) {
	s := &models.Session{ID: "s", ProjectID: 9, ProjectPath: "/srv/repo"}
	// Lookup has a different project, and a nil entry for the right id --
	// both must take the basename path rather than panicking.
	byID := map[int64]*models.Project{
		1: {ID: 1, Path: "/other", DisplayName: "other"},
		9: nil,
	}
	if got := SessionRef(s, byID)["project_name"]; got != "repo" {
		t.Errorf("project_name = %#v, want basename %q", got, "repo")
	}
}

func TestSessionRef_NilSessionReturnsEmptyMapNotNil(t *testing.T) {
	got := SessionRef(nil, nil)
	if got == nil {
		t.Fatal("got nil, want an empty map")
	}
	if len(got) != 0 {
		t.Errorf("got %v, want an empty map", got)
	}
}

func TestSessionRefsFromValues_KeepsRowsDistinctAndOrdered(t *testing.T) {
	sessions := []models.Session{
		{ID: "a", ProjectID: 1, ProjectPath: "/p/one", TurnCount: 1},
		{ID: "b", ProjectID: 2, ProjectPath: "/p/two", TurnCount: 2},
	}
	byID := map[int64]*models.Project{
		1: {ID: 1, Path: "/p/one", DisplayName: "first-brand"},
		2: {ID: 2, Path: "/p/two", DisplayName: "second-brand"},
	}
	got := SessionRefsFromValues(sessions, byID)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	// Per-row assertions, so aliasing the range variable (which would make
	// both rows describe session "b") fails here rather than passing.
	if got[0]["id"] != "a" || got[0]["project_name"] != "first-brand" || got[0]["turn_count"] != 1 {
		t.Errorf("row 0 = %v, want id=a project_name=first-brand turn_count=1", got[0])
	}
	if got[1]["id"] != "b" || got[1]["project_name"] != "second-brand" || got[1]["turn_count"] != 2 {
		t.Errorf("row 1 = %v, want id=b project_name=second-brand turn_count=2", got[1])
	}
}

func TestSessionRefsFromValues_EmptyInputIsEmptyNotNil(t *testing.T) {
	got := SessionRefsFromValues(nil, nil)
	if got == nil {
		t.Fatal("got nil, want an empty slice so it marshals as [] not null")
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}

// --- ProjectsByID / ProjectsByPath --------------------------------------

// These build maps of pointers INTO the caller's slice. Written as
// `for _, p := range projects { out[p.ID] = &p }` every entry would point at
// one reused variable, so every lookup would return the last project. The
// assertion that distinguishes correct from broken is reading a field back
// through two different keys -- a length check cannot see it.
func TestProjectsByID_EachKeyResolvesToItsOwnProject(t *testing.T) {
	projects := []models.Project{
		{ID: 1, Path: "/a", DisplayName: "one"},
		{ID: 2, Path: "/b", DisplayName: "two"},
		{ID: 3, Path: "/c", DisplayName: "three"},
	}
	got := ProjectsByID(projects)

	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	for _, want := range projects {
		p, ok := got[want.ID]
		if !ok {
			t.Errorf("id %d missing from the map", want.ID)
			continue
		}
		if p == nil {
			t.Errorf("id %d maps to nil", want.ID)
			continue
		}
		if p.Path != want.Path || p.DisplayName != want.DisplayName {
			t.Errorf("id %d -> {Path:%q DisplayName:%q}, want {Path:%q DisplayName:%q} "+
				"(all keys resolving to one project means a reused loop variable)",
				want.ID, p.Path, p.DisplayName, want.Path, want.DisplayName)
		}
	}
}

func TestProjectsByPath_EachKeyResolvesToItsOwnProject(t *testing.T) {
	projects := []models.Project{
		{ID: 1, Path: "/a", DisplayName: "one"},
		{ID: 2, Path: "/b", DisplayName: "two"},
	}
	got := ProjectsByPath(projects)

	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got["/a"] == nil || got["/a"].DisplayName != "one" {
		t.Errorf(`got["/a"] = %+v, want the project labelled "one"`, got["/a"])
	}
	if got["/b"] == nil || got["/b"].DisplayName != "two" {
		t.Errorf(`got["/b"] = %+v, want the project labelled "two"`, got["/b"])
	}
}

func TestProjectsByID_EmptyInputIsAnEmptyMap(t *testing.T) {
	got := ProjectsByID(nil)
	if got == nil {
		t.Fatal("got nil, want an empty map so callers can index it without a nil check")
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}

func TestProjectsByPath_EmptyInputIsAnEmptyMap(t *testing.T) {
	got := ProjectsByPath(nil)
	if got == nil {
		t.Fatal("got nil, want an empty map so callers can index it without a nil check")
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}

// --- LabelFromPath (Class A) --------------------------------------------

// LabelFromPath exists so CLI/TUI session lists show branded labels instead
// of basenames. The basename and the brand differ here on purpose: a helper
// that ignored the lookup would return "soma" and fail.
func TestLabelFromPath_PrefersTheLookupLabel(t *testing.T) {
	byPath := map[string]*models.Project{
		"/srv/soma": {Path: "/srv/soma", DisplayName: "jeff-soma"},
	}
	if got := LabelFromPath("/srv/soma", byPath); got != "jeff-soma" {
		t.Errorf("LabelFromPath = %q, want the branded label %q", got, "jeff-soma")
	}
}

func TestLabelFromPath_FallsBackToBasenameOnMiss(t *testing.T) {
	byPath := map[string]*models.Project{
		"/srv/other": {Path: "/srv/other", DisplayName: "other-brand"},
	}
	if got := LabelFromPath("/srv/soma", byPath); got != "soma" {
		t.Errorf("LabelFromPath = %q, want basename %q", got, "soma")
	}
}

// A nil map and a nil value under the right key are different code paths;
// both must reach the basename rather than panicking.
func TestLabelFromPath_HandlesNilMapAndNilEntry(t *testing.T) {
	if got := LabelFromPath("/srv/soma", nil); got != "soma" {
		t.Errorf("LabelFromPath(nil map) = %q, want basename %q", got, "soma")
	}
	byPath := map[string]*models.Project{"/srv/soma": nil}
	if got := LabelFromPath("/srv/soma", byPath); got != "soma" {
		t.Errorf("LabelFromPath(nil entry) = %q, want basename %q", got, "soma")
	}
}

// A lookup hit whose DisplayName is empty must still go through Label, so it
// lands on the basename rather than returning "".
func TestLabelFromPath_LookupHitWithEmptyDisplayNameStillRenders(t *testing.T) {
	byPath := map[string]*models.Project{
		"/srv/soma": {Path: "/srv/soma", DisplayName: ""},
	}
	if got := LabelFromPath("/srv/soma", byPath); got != "soma" {
		t.Errorf("LabelFromPath = %q, want basename %q, not an empty label", got, "soma")
	}
}

// --- Inline (Class B), remaining branch ---------------------------------

// Inline's nil guard, the one branch its existing tests leave untaken.
func TestInline_NilProjectReturnsEmpty(t *testing.T) {
	if got := Inline(nil); got != "" {
		t.Errorf("Inline(nil) = %q, want empty", got)
	}
}
