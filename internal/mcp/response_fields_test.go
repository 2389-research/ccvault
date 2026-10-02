// ABOUTME: Test helpers that read MCP response fields through a fatal, field-naming guard.
// ABOUTME: A dropped or retyped response field reports as a test failure instead of panicking.

package mcp

import (
	"sort"
	"testing"
)

// resultMap narrows a handler's interface{} return to the map shape every
// MCP tool emits. A handler returning something else stops the test here
// with what it actually returned, rather than panicking on an unchecked
// type assertion and taking the rest of the package's output with it.
func resultMap(t *testing.T, result interface{}) map[string]interface{} {
	t.Helper()

	m, ok := result.(map[string]interface{})
	if !ok {
		t.Fatalf("handler result is %T (%#v), want map[string]interface{}", result, result)
	}
	return m
}

// mustField reads key from m as a T. Absent or wrongly-typed fields are
// fatal and name both the field and what was found in its place — these
// are the response-shape fields MCP consumers depend on, so a test that
// notices one changed must explain it.
func mustField[T any](t *testing.T, m map[string]interface{}, key string) T {
	t.Helper()

	var zero T
	raw, present := m[key]
	if !present {
		t.Fatalf("field %q is missing from the response; it has %v", key, sortedKeys(m))
		return zero
	}
	v, ok := raw.(T)
	if !ok {
		t.Fatalf("field %q is %T (%#v), want %T", key, raw, raw, zero)
		return zero
	}
	return v
}

func mustInt(t *testing.T, m map[string]interface{}, key string) int {
	t.Helper()
	return mustField[int](t, m, key)
}

func mustString(t *testing.T, m map[string]interface{}, key string) string {
	t.Helper()
	return mustField[string](t, m, key)
}

// mustRefs reads a list of projectref-shaped objects — list_sessions'
// `sessions`, list_projects' `projects`.
func mustRefs(t *testing.T, m map[string]interface{}, key string) []map[string]any {
	t.Helper()
	return mustField[[]map[string]any](t, m, key)
}

func sortedKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
