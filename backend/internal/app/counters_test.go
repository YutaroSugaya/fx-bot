package app

import (
	"sync"
	"testing"
)

func TestCounters_Snapshot_ReflectsIncrement(t *testing.T) {
	c := &Counters{}
	c.TickerErrors.Add(2)
	c.EmergencyTrips.Add(1)
	c.ResolveTimeouts.Add(5)
	c.CloseRaces.Add(0)
	c.NakedPositions.Add(3)

	snap := c.Snapshot()
	if snap.TickerErrors != 2 {
		t.Errorf("TickerErrors: got %d want 2", snap.TickerErrors)
	}
	if snap.EmergencyTrips != 1 {
		t.Errorf("EmergencyTrips: got %d want 1", snap.EmergencyTrips)
	}
	if snap.ResolveTimeouts != 5 {
		t.Errorf("ResolveTimeouts: got %d want 5", snap.ResolveTimeouts)
	}
	if snap.CloseRaces != 0 {
		t.Errorf("CloseRaces: got %d want 0", snap.CloseRaces)
	}
	if snap.NakedPositions != 3 {
		t.Errorf("NakedPositions: got %d want 3", snap.NakedPositions)
	}
}

func TestCounters_ConcurrentIncrementIsSafe(t *testing.T) {
	c := &Counters{}
	var wg sync.WaitGroup
	const N = 1000
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.EmergencyTrips.Add(1)
		}()
	}
	wg.Wait()
	if got := c.EmergencyTrips.Load(); got != int64(N) {
		t.Errorf("concurrent increment: got %d want %d", got, N)
	}
}
