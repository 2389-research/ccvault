// ABOUTME: Tests for the adapter registry, verifying registration and retrieval of source adapters.
// ABOUTME: Uses a stubAdapter to exercise the registry without depending on real adapter implementations.

package adapter

import (
	"encoding/json"
	"testing"
	"time"
)

// stubAdapter is a minimal SourceAdapter implementation for testing.
type stubAdapter struct{}

func (s *stubAdapter) Name() string { return "stub" }

func (s *stubAdapter) Discover(root string) ([]SessionFile, error) {
	return []SessionFile{
		{
			Path:        root + "/session.jsonl",
			ProjectPath: root,
			ModTime:     time.Now(),
		},
	}, nil
}

func (s *stubAdapter) Parse(path string) (*ParsedSession, error) {
	return &ParsedSession{
		ID:          "test-session",
		ProjectPath: "/test",
		Turns: []ParsedTurn{
			{
				ID:        "turn-1",
				Type:      "user",
				Timestamp: time.Now(),
				Content:   "hello",
				RawJSON:   json.RawMessage(`{}`),
			},
		},
		SourceName: "stub",
		Metadata:   map[string]any{},
	}, nil
}

func TestRegistry_RegisterAndGet(t *testing.T) {
	// Reset global registry state for test isolation.
	resetRegistry()

	Register("stub", func() SourceAdapter {
		return &stubAdapter{}
	})

	adapter, err := Get("stub")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if adapter.Name() != "stub" {
		t.Errorf("expected adapter name %q, got %q", "stub", adapter.Name())
	}
}

// TestRegistry_RegisteredNames covers what the registry could not answer
// before: which sources a build actually has. Tests that have to say something
// about every source — which of them can be reparsed, for one — were reading
// their own hand-written list, so a source added without touching that list
// went unchecked.
func TestRegistry_RegisteredNames(t *testing.T) {
	resetRegistry()

	Register("beta", func() SourceAdapter { return &stubAdapter{} })
	Register("alpha", func() SourceAdapter { return &stubAdapter{} })

	got := RegisteredNames()
	if len(got) != 2 || got[0] != "alpha" || got[1] != "beta" {
		t.Errorf("RegisteredNames() = %v, want [alpha beta] sorted", got)
	}
}

func TestRegistry_GetUnknown(t *testing.T) {
	// Reset global registry state for test isolation.
	resetRegistry()

	_, err := Get("nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown adapter type, got nil")
	}
}
