// ABOUTME: Tests for the pi source adapter that parses pi coding agent session files.
// ABOUTME: Validates interface compliance, Name(), registration, Discover() and Parse().

package pi

import (
	"testing"

	"github.com/2389-research/ccvault/pkg/adapter"
)

func TestAdapter_Name(t *testing.T) {
	a := New()
	if got := a.Name(); got != "pi" {
		t.Errorf("Name() = %q, want %q", got, "pi")
	}
}

func TestAdapter_ImplementsInterface(t *testing.T) {
	var _ adapter.SourceAdapter = New()
}

// TestAdapter_IsATranscriptReparser pins the half of the
// adapter.TranscriptReparser contract that pi's format forces: every pi
// assistant message carries its own usage block, so a pi session that could
// not be re-read from its stored lines would be a session whose token
// counters could never be repaired. test/integration/reparsers_test.go holds
// the cross-source version of the same table.
func TestAdapter_IsATranscriptReparser(t *testing.T) {
	var _ adapter.TranscriptReparser = New()
}

func TestAdapter_RegistersItself(t *testing.T) {
	a, err := adapter.Get("pi")
	if err != nil {
		t.Fatalf("adapter.Get(%q): %v", "pi", err)
	}
	if got := a.Name(); got != "pi" {
		t.Errorf("registered adapter Name() = %q, want %q", got, "pi")
	}
}
