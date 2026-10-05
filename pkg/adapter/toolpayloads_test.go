// ABOUTME: Guards the ParsedToolUse <-> models.ToolUse conversions against a field that gets added but not threaded through.
// ABOUTME: Walks both structs by reflection, because a forgotten field copy compiles cleanly and just leaves a column empty.

package adapter

import (
	"reflect"
	"testing"
	"time"

	"github.com/2389-research/ccvault/pkg/models"
)

// distinctValue returns a non-zero value for a field type, so a field that
// fails to survive a conversion shows up as a zero where a value was set.
func distinctValue(t *testing.T, kind reflect.Kind, seed int) reflect.Value {
	t.Helper()
	switch kind {
	case reflect.String:
		return reflect.ValueOf("value-" + time.Duration(seed).String())
	case reflect.Int:
		return reflect.ValueOf(seed + 1)
	case reflect.Int64:
		return reflect.ValueOf(int64(seed + 1))
	case reflect.Bool:
		return reflect.ValueOf(true)
	default:
		t.Fatalf("distinctValue has no case for %s — add one when a field of that type is introduced", kind)
		return reflect.Value{}
	}
}

// TestToolUseRoundTrip fills every field of ParsedToolUse with a distinct
// non-zero value, converts to models.ToolUse and back, and requires the result
// to equal the original.
//
// The failure this exists for is the one PR #35 ran into from the other
// direction: a change that compiles and passes lint while silently carrying
// the wrong data. Copying nine fields in three files is exactly that shape —
// a missed line is not an error, it is a column that is always empty.
func TestToolUseRoundTrip(t *testing.T) {
	var original ParsedToolUse
	v := reflect.ValueOf(&original).Elem()
	for i := range v.NumField() {
		v.Field(i).Set(distinctValue(t, v.Field(i).Kind(), i))
	}

	stored := ToolUseFromParsed(original, "turn-1", "session-1", time.Unix(1700000000, 0).UTC())
	back := ParsedToolUseFromModel(stored)

	if !reflect.DeepEqual(original, back) {
		t.Errorf("round trip lost data:\n original = %+v\n      got = %+v", original, back)
	}
}

// TestToolUseFromParsedStampsIdentity covers the fields that come from the
// turn rather than the tool use, which the adapter contract carries one level
// up.
func TestToolUseFromParsedStampsIdentity(t *testing.T) {
	ts := time.Unix(1700000000, 0).UTC()
	got := ToolUseFromParsed(ParsedToolUse{ToolName: "Bash"}, "turn-1", "session-1", ts)

	if got.TurnID != "turn-1" || got.SessionID != "session-1" || !got.Timestamp.Equal(ts) {
		t.Errorf("got %+v, want turn-1 / session-1 / %v", got, ts)
	}
}

// TestModelToolUseCoversEveryParsedField pins the two structs to the same
// payload surface. A field added to models.ToolUse that ParsedToolUse has no
// way to supply would be a column no adapter can ever populate.
func TestModelToolUseCoversEveryParsedField(t *testing.T) {
	modelFields := make(map[string]bool)
	mt := reflect.TypeOf(models.ToolUse{})
	for i := range mt.NumField() {
		modelFields[mt.Field(i).Name] = true
	}

	pt := reflect.TypeOf(ParsedToolUse{})
	for i := range pt.NumField() {
		name := pt.Field(i).Name
		if !modelFields[name] {
			t.Errorf("ParsedToolUse.%s has no counterpart on models.ToolUse, so nothing stores it", name)
		}
	}
}
