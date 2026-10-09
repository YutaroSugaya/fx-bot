package safety

import (
	"sync"
	"testing"
)

// PendingPositions は port.PendingPositionTracker の in-memory 実装。
// 並行安全性と Mark/Resolve の対称性を中心に検証する。

func TestPendingPositions_AddAndQuery(t *testing.T) {
	p := NewPendingPositions()
	if p.IsPending("X") {
		t.Errorf("empty tracker must not report X as pending")
	}
	p.MarkPending("X")
	if !p.IsPending("X") {
		t.Errorf("after MarkPending(X), IsPending(X) must be true")
	}
	if p.IsPending("Y") {
		t.Errorf("unrelated id Y must still be non-pending")
	}
}

func TestPendingPositions_Resolve(t *testing.T) {
	p := NewPendingPositions()
	p.MarkPending("X")
	p.MarkResolved("X")
	if p.IsPending("X") {
		t.Errorf("after MarkResolved(X), IsPending(X) must be false")
	}
}

func TestPendingPositions_EmptyIDIsNoOp(t *testing.T) {
	p := NewPendingPositions()
	// 空文字は entry saga の早期 critical-failure (positionId resolve できず) を
	// 表すことがあるので、tracker 操作も IsPending も常に false で安全に流す。
	p.MarkPending("")
	if p.IsPending("") {
		t.Errorf(`IsPending("") must always be false`)
	}
	p.MarkResolved("") // panic しないこと
}

func TestPendingPositions_DoubleResolveIsSafe(t *testing.T) {
	p := NewPendingPositions()
	p.MarkPending("X")
	p.MarkResolved("X")
	p.MarkResolved("X") // 二重 Remove で panic しないこと
	if p.IsPending("X") {
		t.Errorf("X must remain non-pending after double Resolve")
	}
}

func TestPendingPositions_ConcurrentAccess(t *testing.T) {
	p := NewPendingPositions()
	var wg sync.WaitGroup
	const N = 200
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "id-" + string(rune('A'+i%26))
			p.MarkPending(id)
			_ = p.IsPending(id)
			p.MarkResolved(id)
		}(i)
	}
	wg.Wait()
	// race detector + final IsPending=false で並行安全性を担保
	for c := 'A'; c <= 'Z'; c++ {
		if p.IsPending("id-" + string(c)) {
			t.Errorf("post-concurrent: id-%c should be resolved", c)
		}
	}
}
