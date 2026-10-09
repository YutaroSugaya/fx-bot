package safety

import (
	"testing"
	"time"
)

// A pending entry whose saga crashed before
// MarkResolved would stay pending FOREVER, so reconcile would skip that broker
// position indefinitely — masking a genuinely naked position. StaleIDs surfaces
// pendings older than a TTL so reconcile can stop skipping them.
func TestPendingPositions_StaleIDs(t *testing.T) {
	t0 := time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC)
	cur := t0
	p := NewPendingPositionsWithClock(func() time.Time { return cur })

	p.MarkPending("pos-1")
	cur = t0.Add(5 * time.Minute)
	p.MarkPending("pos-2")

	// At t0+5m: pos-1 is 5m old, pos-2 is 0 → none exceed a 10m TTL.
	if got := p.StaleIDs(10 * time.Minute); len(got) != 0 {
		t.Errorf("no pending should be stale yet; got %v", got)
	}

	cur = t0.Add(11 * time.Minute)
	// pos-1 is 11m (> 10m TTL → stale); pos-2 is 6m (not).
	stale := p.StaleIDs(10 * time.Minute)
	if len(stale) != 1 || stale[0] != "pos-1" {
		t.Fatalf("StaleIDs = %v, want [pos-1]", stale)
	}

	// Resolving removes it from the stale set.
	p.MarkResolved("pos-1")
	if got := p.StaleIDs(10 * time.Minute); len(got) != 0 {
		t.Errorf("resolved id must not be stale; got %v", got)
	}
}

func TestPendingPositions_DefaultConstructorStillWorks(t *testing.T) {
	// Back-compat: NewPendingPositions() (no clock) still tracks pending state.
	p := NewPendingPositions()
	p.MarkPending("X")
	if !p.IsPending("X") {
		t.Error("MarkPending/IsPending must still work via the default constructor")
	}
	p.MarkResolved("X")
	if p.IsPending("X") {
		t.Error("MarkResolved must clear")
	}
}
