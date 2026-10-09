package advisor

import (
	"context"
	"testing"
	"time"
)

// applyTimeout は context.Deadline と TimeoutSeconds の短い方を採用する。
// context.WithTimeout で TimeoutSeconds を一方的に適用すると、呼出側が
// 30s deadline を渡しても 120s に上書きされてしまうため。
//
// テストは a.Clock() を固定して deadline 計算の決定性を保つ。

func TestApplyTimeout_NoParentDeadline_UsesConfigured(t *testing.T) {
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	a := &ClaudeCLIAdvisor{
		TimeoutSeconds: 30,
		Clock:          func() time.Time { return now },
	}
	cctx, cancel := a.applyTimeout(context.Background())
	defer cancel()

	deadline, ok := cctx.Deadline()
	if !ok {
		t.Fatal("expected deadline to be set")
	}
	want := now.Add(30 * time.Second)
	if !deadline.Equal(want) {
		t.Errorf("deadline: got %v want %v", deadline, want)
	}
}

func TestApplyTimeout_ParentDeadlineShorter_HonorsParent(t *testing.T) {
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	a := &ClaudeCLIAdvisor{
		TimeoutSeconds: 120,
		Clock:          func() time.Time { return now },
	}
	parentDeadline := now.Add(2 * time.Second) // 親の方が短い (cfg より前)
	parent, parentCancel := context.WithDeadline(context.Background(), parentDeadline)
	defer parentCancel()

	cctx, cancel := a.applyTimeout(parent)
	defer cancel()

	deadline, ok := cctx.Deadline()
	if !ok {
		t.Fatal("expected deadline to be set")
	}
	// 親 deadline がそのまま残るはず (子側で延長してはいけない)。
	if !deadline.Equal(parentDeadline) {
		t.Errorf("should honor parent deadline %v; got %v", parentDeadline, deadline)
	}
}

func TestApplyTimeout_ConfiguredShorter_AppliesConfigured(t *testing.T) {
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	a := &ClaudeCLIAdvisor{
		TimeoutSeconds: 10,
		Clock:          func() time.Time { return now },
	}
	// 親 deadline はかなり遠い → configured 10s が勝つ
	parent, parentCancel := context.WithDeadline(context.Background(), now.Add(time.Hour))
	defer parentCancel()

	cctx, cancel := a.applyTimeout(parent)
	defer cancel()

	deadline, ok := cctx.Deadline()
	if !ok {
		t.Fatal("expected deadline to be set")
	}
	want := now.Add(10 * time.Second)
	if !deadline.Equal(want) {
		t.Errorf("configured 10s should apply; got %v want %v", deadline, want)
	}
}

func TestApplyTimeout_ZeroConfigured_FallsBackTo120s(t *testing.T) {
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	a := &ClaudeCLIAdvisor{
		TimeoutSeconds: 0, // unset
		Clock:          func() time.Time { return now },
	}
	cctx, cancel := a.applyTimeout(context.Background())
	defer cancel()

	deadline, ok := cctx.Deadline()
	if !ok {
		t.Fatal("expected deadline to be set")
	}
	want := now.Add(120 * time.Second)
	if !deadline.Equal(want) {
		t.Errorf("default 120s: got %v want %v", deadline, want)
	}
}
