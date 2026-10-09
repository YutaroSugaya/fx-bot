package clock

import (
	"testing"
	"time"
)

// Clock は usecase / handler 層が `time.Now()` を直に呼ぶのを避けるための
// 注入用 interface。production は System (= time.Now のラッパ)、test は
// Fake (= 任意の固定値、Advance/Set で進めるコントローラ) を使う。
//
// 既存の `Now func() time.Time` フィールド方式 (scheduler / GmoBroker /
// rate limiter 等) はそのまま残し、新規 usecase は Clock interface 経由で
// 統一していく方針。conversion ヘルパ (FromFunc) で両者を橋渡しできる。

func TestSystemClock_ReturnsNonZeroAndMonotonic(t *testing.T) {
	t1 := System.Now()
	if t1.IsZero() {
		t.Fatal("System.Now() returned zero time")
	}
	time.Sleep(2 * time.Millisecond)
	t2 := System.Now()
	if !t2.After(t1) {
		t.Errorf("expected t2 (%v) after t1 (%v)", t2, t1)
	}
}

func TestFake_NowReturnsConfiguredTime(t *testing.T) {
	want := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	c := NewFake(want)
	got := c.Now()
	if !got.Equal(want) {
		t.Errorf("Fake.Now: got %v want %v", got, want)
	}
}

func TestFake_Advance(t *testing.T) {
	c := NewFake(time.Date(2026, 5, 26, 0, 0, 0, 0, time.UTC))
	c.Advance(90 * time.Minute)
	want := time.Date(2026, 5, 26, 1, 30, 0, 0, time.UTC)
	if got := c.Now(); !got.Equal(want) {
		t.Errorf("after Advance(90m): got %v want %v", got, want)
	}
}

func TestFake_Set(t *testing.T) {
	c := NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	c.Set(time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC))
	want := time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC)
	if got := c.Now(); !got.Equal(want) {
		t.Errorf("after Set: got %v want %v", got, want)
	}
}

func TestFake_IsRaceSafe(t *testing.T) {
	// Fake を複数 goroutine から触っても data race にならないこと
	// (-race flag 下でこの test が cleanだけ通れば OK)。
	c := NewFake(time.Date(2026, 5, 26, 0, 0, 0, 0, time.UTC))
	done := make(chan struct{}, 2)
	go func() { _ = c.Now(); done <- struct{}{} }()
	go func() { c.Advance(time.Second); done <- struct{}{} }()
	<-done
	<-done
}

func TestFromFunc_WrapsClassicNowFunc(t *testing.T) {
	// 既存 `Now func() time.Time` 注入と新 Clock interface を橋渡しする
	// アダプタ。レガシー code 側を一気に書き換えずに済む。
	fixed := time.Date(2026, 5, 26, 9, 0, 0, 0, time.UTC)
	c := FromFunc(func() time.Time { return fixed })
	if got := c.Now(); !got.Equal(fixed) {
		t.Errorf("FromFunc: got %v want %v", got, fixed)
	}
}
