// Package app holds the runtime loops that drive the bot: scheduler (hourly
// advisor trigger), worker (price/minute loops), api_server (REST endpoints
// for the dashboard).
//
// app does no business logic itself — it composes usecase functions on
// timers and IO callbacks. Tests for the loops live next to each loop file.
package app

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"fx-bot/backend/internal/config"
)

// Scheduler fires advisor cycles. Default interval comes from bot_config
// (interval_minutes). After each fire, the actual interval to the next fire
// is determined by NextIntervalFn — typically reading the active strategy
// config's `next_advisor_run_in_minutes` so イベント帯では短く (10 分〜)、
// 平時は ai_advisor.interval_minutes になる。
//
// MVP behaviour:
//   - weekday-only when SchedulerSection.WeekdaysOnly = true (Mon..Fri JST)
//   - skip ticks before StartTime or after EndTime on the corresponding day
//   - first tick fires on the next default-interval-aligned boundary
type Scheduler struct {
	Interval     time.Duration // default interval (used at startup + as fallback)
	Logger       *slog.Logger
	Now          func() time.Time
	WeekdaysOnly bool
	StartHour    int // 0-23
	EndHour      int // 0-23, can be larger than 24 to represent "next day"
	Timezone     *time.Location

	// NextIntervalFn は前回 fire 完了後に呼ばれ、次の fire までの待ち時間を返す。
	// 戻り値 0 以下は「次回 fire を Interval (default) でスケジュール」扱い。
	// 0 を返したい (= 一時的に停止) 場合は nil で良い。
	NextIntervalFn func() time.Duration

	// HeartbeatTimeout は heartbeat watchdog の閾値。
	// 最終 fire (RecordFire) から HeartbeatTimeout 以上経過したら、Run 内の
	// watchdog goroutine が強制 fire する (scheduler が長時間無音で止まるのを
	// 防ぐ)。0 = 無効 (= 既存挙動)。デフォルト推奨値は 35 分
	// (= 30 分間隔の 1 サイクル超過)。
	HeartbeatTimeout time.Duration

	// fireMu と lastFiredAt は RecordFire / HeartbeatTick の共有状態。
	// HeartbeatTick が watchdog 発火前に lastFiredAt を更新することで、
	// 短間隔ポーリングでも重複 fire を回避する。
	fireMu      sync.Mutex
	lastFiredAt time.Time
}

// RecordFire は scheduler が fire したタイミングを記録する。HeartbeatTick が
// この値からの経過時間で watchdog の発火可否を判定する。
func (s *Scheduler) RecordFire(now time.Time) {
	s.fireMu.Lock()
	s.lastFiredAt = now
	s.fireMu.Unlock()
}

// HeartbeatTick は watchdog の純粋判定 + 発火点。Run の watchdog goroutine が
// 定期的にこれを呼ぶ。テストからも直接呼べる:
//   - HeartbeatTimeout == 0 → no-op (watchdog 無効)
//   - lastFiredAt がゼロ (= 起動直後) → no-op (= 通常 fire を待つ)
//   - now - lastFiredAt >= HeartbeatTimeout → fire() を呼んで lastFiredAt 更新
//     (更新は fire の前にやるので、polling 間隔が短くても重複 fire しない)
func (s *Scheduler) HeartbeatTick(now time.Time, fire func()) {
	if s.HeartbeatTimeout <= 0 {
		return
	}
	// Watchdog must honour the same weekday/trading-hours gate as the normal
	// tick. Without this it force-fires advisor cycles through the whole
	// weekend / daily off-window — even with WeekdaysOnly=true — because the
	// skipped normal ticks never RecordFire, so lastFiredAt goes stale and the
	// watchdog keeps tripping (dozens of advisor cycles per off-day, all via
	// this path). Returning early here
	// leaves lastFiredAt untouched so the first in-hours tick fires promptly.
	if !s.shouldFire(now) {
		return
	}
	s.fireMu.Lock()
	last := s.lastFiredAt
	if last.IsZero() || now.Sub(last) < s.HeartbeatTimeout {
		s.fireMu.Unlock()
		return
	}
	s.lastFiredAt = now
	s.fireMu.Unlock()
	if s.Logger != nil {
		s.Logger.Warn("scheduler_heartbeat_watchdog_fired",
			"elapsed", now.Sub(last).String(),
			"timeout", s.HeartbeatTimeout.String())
	}
	fire()
}

// NewSchedulerFromConfig builds a Scheduler from the bot_config Scheduler
// section. Falls back to UTC when timezone can't be loaded.
func NewSchedulerFromConfig(cfg config.SchedulerSection, intervalMinutes int, logger *slog.Logger, timezone string) *Scheduler {
	loc, err := time.LoadLocation(timezone)
	if err != nil || loc == nil {
		loc = time.UTC
	}
	sh, _ := parseHour(cfg.StartTime)
	eh, _ := parseHour(cfg.EndTime)
	return &Scheduler{
		Interval:     time.Duration(intervalMinutes) * time.Minute,
		Logger:       logger,
		Now:          func() time.Time { return time.Now().In(loc) },
		WeekdaysOnly: cfg.WeekdaysOnly,
		StartHour:    sh,
		EndHour:      eh,
		Timezone:     loc,
	}
}

// parseHour extracts the hour from an "HH:MM" string (e.g. "07:00" → 7).
// Strings shorter than 2 chars (= unset) yield 0 with no error so an empty
// start/end time falls back to hour 0.
func parseHour(s string) (int, error) {
	if len(s) < 2 {
		return 0, nil
	}
	tm, err := time.Parse("15:04", s)
	if err != nil {
		return 0, err
	}
	return tm.Hour(), nil
}

// Run blocks until ctx is cancelled, calling fn on each tick where the
// weekday/hour guard passes. After each fire, NextIntervalFn (if set) decides
// the next tick. fn is given a boolean — true when this fire used the
// default interval (= "auto"), false when it used a shorter overridden
// interval (= "event"). The caller uses this to tag the AdvisorRun source.
//
// HeartbeatTimeout > 0 のとき別 goroutine で watchdog が走り、
// 最終 fire から timeout を超えると強制 fire する (長時間のサイレント停止を
// 防ぐ)。
func (s *Scheduler) Run(ctx context.Context, fn func(context.Context, bool)) {
	if s.Interval <= 0 {
		s.Interval = time.Hour
	}
	if s.Now == nil {
		s.Now = time.Now
	}
	// Sleep until the next default-interval-aligned boundary for the first fire.
	first := s.nextAligned(s.Now())
	if s.Logger != nil {
		s.Logger.Info("scheduler_started", "next_fire", first.Format(time.RFC3339), "interval", s.Interval.String())
	}
	timer := time.NewTimer(first.Sub(s.Now()))
	defer timer.Stop()

	// Heartbeat watchdog: timeout > 0 なら起動。fn は event 扱い (wasDefault=false)。
	if s.HeartbeatTimeout > 0 {
		go s.runWatchdog(ctx, func() { fn(ctx, false) })
	}

	wasDefault := true
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		now := s.Now()
		if s.shouldFire(now) {
			s.RecordFire(now)
			fn(ctx, wasDefault)
		}
		// Decide next interval. NextIntervalFn override is honored for any
		// positive value (including > Interval — Claude may legitimately ask
		// for a longer-than-default cooldown after a no_trade or losing streak).
		// 0 以下はデフォルト挙動 (fallback). 短い (< Interval) ときは "event" として
		// AdvisorRunSource にタグ付けする。
		nextInterval := s.Interval
		wasDefault = true
		if s.NextIntervalFn != nil {
			if d := s.NextIntervalFn(); d > 0 {
				nextInterval = d
				if d < s.Interval {
					wasDefault = false
					if s.Logger != nil {
						s.Logger.Info("scheduler_event_interval", "next_in", d.String())
					}
				} else if d > s.Interval && s.Logger != nil {
					s.Logger.Info("scheduler_extended_interval", "next_in", d.String())
				}
			}
		}
		next := s.Now().Add(nextInterval)
		timer.Reset(next.Sub(s.Now()))
	}
}

// runWatchdog は heartbeat ループ。ctx cancel まで HeartbeatTimeout/5
// 間隔 (最小 1 分) で HeartbeatTick を呼ぶ。fire は event 扱いの fn closure。
func (s *Scheduler) runWatchdog(ctx context.Context, fire func()) {
	poll := s.HeartbeatTimeout / 5
	if poll < time.Minute {
		poll = time.Minute
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		s.HeartbeatTick(s.Now(), fire)
	}
}

// nextAligned returns the next clock boundary aligned to Interval, in the
// scheduler's timezone.
func (s *Scheduler) nextAligned(now time.Time) time.Time {
	return now.Truncate(s.Interval).Add(s.Interval)
}

// shouldFire applies weekday + start/end gating. Returns false when the call
// happens outside trading hours, in which case fn is skipped.
func (s *Scheduler) shouldFire(t time.Time) bool {
	if s.WeekdaysOnly {
		switch t.Weekday() {
		case time.Saturday, time.Sunday:
			return false
		}
	}
	if s.StartHour == 0 && s.EndHour == 0 {
		// Unset → allow all hours.
		return true
	}
	h := t.Hour()
	if s.StartHour <= s.EndHour {
		return h >= s.StartHour && h < s.EndHour
	}
	// Window wraps midnight (e.g. 07:00..06:00 next day)
	return h >= s.StartHour || h < s.EndHour
}
