// ABOUTME: Tests for issue #112 — a retired SyncingModel's in-flight tick must not re-arm its replacement's chain.
// ABOUTME: Drives Model.Update with the messages directly, so nothing here depends on wall-clock timing.

package tui

import "testing"

// TestRetiredSyncingModelsTickDoesNotMultiplyTheChain covers issue #112.
//
// SyncingModel keeps itself alive by returning its own tick, so liveness is
// owned by a model that can be replaced underneath the message already
// scheduled for it. When a second sync replaces the first, the retired
// model's tick is still in flight; it lands on the replacement, which re-arms
// — and now two chains run, each re-arming itself every 200ms.
//
// Neither model here is Init'd. A tick chain's identity is settled at
// construction, and leaving the sync goroutines unstarted is what keeps the
// assertion about ticks rather than about which sync happened to finish
// first: with no goroutine there is nothing in doneCh, so a non-nil command
// back from Update can only be the next tick.
func TestRetiredSyncingModelsTickDoesNotMultiplyTheChain(t *testing.T) {
	database := openTUITestDB(t)
	m := New(database, t.TempDir(), nil)

	retired := NewSyncingModel(database, nil)
	m.syncing = retired
	m.view = SyncingView
	m.viewStack = []View{DashboardView, SyncingView}

	// The replacement a second push of the syncing view mints.
	live := NewSyncingModel(database, nil)
	m.syncing = live

	if retired.generation == live.generation {
		t.Fatalf("both models carry generation %d, so a tick cannot say which one scheduled it", live.generation)
	}

	// The retired model's in-flight tick arrives alongside the replacement's
	// own. Exactly one of the two may re-arm.
	liveChains := 0
	for _, tick := range []syncTickMsg{retired.tickMsg(), live.tickMsg()} {
		if _, cmd := m.Update(tick); cmd != nil {
			liveChains++
		}
	}
	if liveChains != 1 {
		t.Errorf("%d tick chains are live after the replacement, want 1 (#112)", liveChains)
	}
}

// TestPushingTheSyncingViewRetiresTheOldGeneration drives the real path that
// produces the duplication: pushView replaces m.syncing, and the model it
// dropped still has a tick on the way.
func TestPushingTheSyncingViewRetiresTheOldGeneration(t *testing.T) {
	database := openTUITestDB(t)
	m := New(database, t.TempDir(), nil)
	defer m.shutdownSync()

	m.pushView(SyncingView, nil)
	retired := m.syncing

	m.pushView(SyncingView, nil)
	if m.syncing == retired {
		t.Fatal("pushing the syncing view reused the old model; the replacement path is not what this test thinks it is")
	}
	if m.syncing.generation == retired.generation {
		t.Fatalf("the replacement reused generation %d", retired.generation)
	}

	if _, cmd := m.Update(retired.tickMsg()); cmd != nil {
		t.Error("a retired model's tick re-armed the chain, so the replacement now runs two (#112)")
	}
	if _, cmd := m.Update(m.syncing.tickMsg()); cmd == nil {
		t.Error("the live model's own tick did not re-arm: the sync's only chain is dead (#99)")
	}
}
