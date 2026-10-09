package app

import (
	"sync/atomic"
	"testing"
	"time"
)

// scheduler heartbeat watchdog
//
// scheduler が原因不明のまま長時間サイレントになるケースに備え、
// HeartbeatTimeout=35 分で「30 分間隔の 1 サイクル超過」を検知し、watchdog
// goroutine が強制 fire する。HeartbeatTick はその純粋判定。

func TestScheduler_HeartbeatTick_NoOpBeforeFirstFire(t *testing.T) {
	s := &Scheduler{HeartbeatTimeout: 35 * time.Minute}
	var fired int32
	s.HeartbeatTick(time.Now(), func() { atomic.AddInt32(&fired, 1) })
	if got := atomic.LoadInt32(&fired); got != 0 {
		t.Errorf("must not fire before any RecordFire (= lastFiredAt zero), got %d", got)
	}
}

func TestScheduler_HeartbeatTick_NoOpWithinTimeout(t *testing.T) {
	s := &Scheduler{HeartbeatTimeout: 35 * time.Minute}
	base := time.Date(2026, 5, 28, 10, 0, 0, 0, time.UTC)
	s.RecordFire(base)
	var fired int32
	s.HeartbeatTick(base.Add(10*time.Minute), func() { atomic.AddInt32(&fired, 1) })
	if got := atomic.LoadInt32(&fired); got != 0 {
		t.Errorf("within timeout (10m < 35m), must not fire; got %d", got)
	}
}

func TestScheduler_HeartbeatTick_FiresPastTimeoutAndUpdatesLastFired(t *testing.T) {
	s := &Scheduler{HeartbeatTimeout: 35 * time.Minute}
	base := time.Date(2026, 5, 28, 10, 0, 0, 0, time.UTC)
	s.RecordFire(base)
	stale := base.Add(40 * time.Minute) // 35 < 40 → fire
	var fired int32
	s.HeartbeatTick(stale, func() { atomic.AddInt32(&fired, 1) })
	if got := atomic.LoadInt32(&fired); got != 1 {
		t.Fatalf("past timeout: expected 1 fire, got %d", got)
	}
	// 直後の HeartbeatTick は lastFiredAt が更新されているので追い fire しない。
	s.HeartbeatTick(stale.Add(time.Minute), func() { atomic.AddInt32(&fired, 1) })
	if got := atomic.LoadInt32(&fired); got != 1 {
		t.Errorf("watchdog must not double-fire while lastFiredAt is fresh; got %d", got)
	}
}

func TestScheduler_HeartbeatTick_DisabledWhenTimeoutZero(t *testing.T) {
	s := &Scheduler{HeartbeatTimeout: 0}
	s.RecordFire(time.Now().Add(-2 * time.Hour))
	var fired int32
	s.HeartbeatTick(time.Now(), func() { atomic.AddInt32(&fired, 1) })
	if got := atomic.LoadInt32(&fired); got != 0 {
		t.Errorf("HeartbeatTimeout=0 must disable watchdog; got %d fires", got)
	}
}

// weekdays_only=true は通常 tick (auto) を抑止するが、watchdog が shouldFire を
// 通さず event を強制 fire すると、土日に advisor サイクルが数十回/日 走って
// しまう (auto=0, event のみ)。watchdog も通常 tick と同じ
// weekday/trading-hours ゲートを尊重しなければならない。

var jst = time.FixedZone("JST", 9*3600)

func TestScheduler_HeartbeatTick_SkipsWeekendWhenWeekdaysOnly(t *testing.T) {
	s := &Scheduler{HeartbeatTimeout: 35 * time.Minute, WeekdaysOnly: true}
	sat := time.Date(2026, 5, 30, 14, 0, 0, 0, jst) // Saturday 14:00 JST
	s.RecordFire(sat.Add(-40 * time.Minute))        // overdue (>35m)
	var fired int32
	s.HeartbeatTick(sat, func() { atomic.AddInt32(&fired, 1) })
	if got := atomic.LoadInt32(&fired); got != 0 {
		t.Errorf("watchdog must not fire on Saturday when WeekdaysOnly=true; got %d", got)
	}
}

func TestScheduler_HeartbeatTick_SkipsOutsideTradingHours(t *testing.T) {
	// start 07:00, end 06:00 (wraps midnight) → 06:00-06:59 is the daily gap.
	s := &Scheduler{HeartbeatTimeout: 35 * time.Minute, WeekdaysOnly: true, StartHour: 7, EndHour: 6}
	gap := time.Date(2026, 5, 29, 6, 30, 0, 0, jst) // Friday 06:30 JST (off-hours)
	s.RecordFire(gap.Add(-40 * time.Minute))
	var fired int32
	s.HeartbeatTick(gap, func() { atomic.AddInt32(&fired, 1) })
	if got := atomic.LoadInt32(&fired); got != 0 {
		t.Errorf("watchdog must not fire inside the daily off-window; got %d", got)
	}
}

func TestScheduler_HeartbeatTick_FiresOnWeekdayWithinHours(t *testing.T) {
	// Regression guard: the weekday/hours gate must NOT defeat the watchdog's
	// purpose (catching a silent scheduler) during real trading windows.
	s := &Scheduler{HeartbeatTimeout: 35 * time.Minute, WeekdaysOnly: true, StartHour: 7, EndHour: 6}
	fri := time.Date(2026, 5, 29, 10, 0, 0, 0, jst) // Friday 10:00 JST (in-hours)
	s.RecordFire(fri.Add(-40 * time.Minute))
	var fired int32
	s.HeartbeatTick(fri, func() { atomic.AddInt32(&fired, 1) })
	if got := atomic.LoadInt32(&fired); got != 1 {
		t.Errorf("watchdog must still fire on a weekday within hours when overdue; got %d", got)
	}
}
