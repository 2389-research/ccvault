// ABOUTME: Tests for wire protocol message encoding
// ABOUTME: Ensures round-trip stability of ingest and query messages

package protocol

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/2389-research/ccvault/pkg/models"
)

func TestIngestMessageRoundTrip(t *testing.T) {
	orig := IngestMessage{
		Kind: KindSession,
		Session: &models.Session{
			ID:        "sess-1",
			StartedAt: time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC),
			EndedAt:   time.Date(2026, 7, 21, 10, 5, 0, 0, time.UTC),
			Source:    "claude-code",
		},
	}
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(orig); err != nil {
		t.Fatal(err)
	}

	var got IngestMessage
	if err := json.NewDecoder(&buf).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Kind != KindSession || got.Session.ID != "sess-1" {
		t.Fatalf("round trip: %+v", got)
	}
}

func TestSessionEndMessage(t *testing.T) {
	orig := IngestMessage{Kind: KindSessionEnd, SessionID: "sess-1"}
	b, _ := json.Marshal(orig)
	var got IngestMessage
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Kind != KindSessionEnd || got.SessionID != "sess-1" {
		t.Fatalf("got %+v", got)
	}
}
