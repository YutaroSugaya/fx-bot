package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// EventCalendar holds the manually-maintained list of economic-event windows.
//
// 各 event は (at, pre_minutes, post_minutes, policy) で表現し、窓は
// [at - pre_minutes, at + post_minutes] (両端含む)。
//   - policy=freeze (既定): worker / admission snapshot が InFreezeWindow を
//     問い合わせ、risk.Gate が InEventFreeze=true で entry を reject する。
//   - policy=breakout: freeze せず、advisor が event_context を見て breakout を
//     arm し攻める (InFreezeWindow は false を返す)。
//
// どちらの policy でも ActiveOrUpcoming 経由で event_context として Claude に渡る。
//
// 運用方針: 月 1 で運用者が configs/event_calendar.yaml を更新する。
// ファイルが無いときは空 calendar (= freeze 無効) で起動する。
type EventCalendar struct {
	Events []CalendarEvent `yaml:"events"`
}

// イベントの取り扱い方針。
const (
	// EventPolicyFreeze: 窓内は entry 禁止 (守り、既定)。未指定もこれ。
	EventPolicyFreeze = "freeze"
	// EventPolicyBreakout: 窓内は freeze せず breakout_follow を arm して攻める
	// (攻めモード)。発表後の初動ブレイクに乗る。
	EventPolicyBreakout = "breakout"
)

// CalendarEvent は 1 件の経済指標イベント。
type CalendarEvent struct {
	Name        string    `yaml:"name"`
	At          time.Time `yaml:"at"`           // ISO8601 UTC (例: "2026-06-05T21:30:00Z")
	PreMinutes  int       `yaml:"pre_minutes"`  // 発表前 窓 分数
	PostMinutes int       `yaml:"post_minutes"` // 発表後 窓 分数
	// Policy は窓内の取り扱い。"" / "freeze" = 守り (entry 禁止)、
	// "breakout" = 攻め (freeze せず breakout を arm)。
	Policy string `yaml:"policy,omitempty"`
}

// isFreeze reports whether this event freezes trading in its window.
// 未指定 policy は freeze 扱い (back-compat)。
func (e CalendarEvent) isFreeze() bool {
	return e.Policy == "" || e.Policy == EventPolicyFreeze
}

// contains reports whether now is inside [At-Pre, At+Post] (両端含む)。
func (e CalendarEvent) contains(now time.Time) bool {
	start := e.At.Add(-time.Duration(e.PreMinutes) * time.Minute)
	end := e.At.Add(time.Duration(e.PostMinutes) * time.Minute)
	return !now.Before(start) && !now.After(end)
}

// InFreezeWindow returns true when `now` is inside a FREEZE-policy event's
// [At-PreMinutes, At+PostMinutes] interval. Nil receiver and empty Events
// always return false so missing config files don't accidentally enable
// freeze (= fail-open for observability, not for safety — the gate has
// other layers like emergency_stop and daily_loss).
//
// breakout-policy のイベントは freeze しない (攻めモード)。
func (c *EventCalendar) InFreezeWindow(now time.Time) bool {
	if c == nil {
		return false
	}
	for _, e := range c.Events {
		if e.isFreeze() && e.contains(now) {
			return true
		}
	}
	return false
}

// InFreezeWindowConsideringAdvisor extends InFreezeWindow: when the advisor is
// DISABLED, breakout-policy events have no one to arm or manage the breakout, so
// they fall back to freeze (otherwise other entry paths would keep deciding
// trades inside an unmanaged high-impact window such as NFP). When the advisor
// is enabled, breakout events stay non-freezing (attack mode). Freeze-policy
// events always freeze.
func (c *EventCalendar) InFreezeWindowConsideringAdvisor(now time.Time, advisorEnabled bool) bool {
	if c == nil {
		return false
	}
	if advisorEnabled {
		return c.InFreezeWindow(now)
	}
	// Advisor off: ANY in-window event freezes (breakout falls back to freeze).
	for _, e := range c.Events {
		if e.contains(now) {
			return true
		}
	}
	return false
}

// ActiveOrUpcoming は now に最も関連するイベントを返す。
//   - now が窓内のイベントがあれば、それを inWindow=true で返す (複数なら最初)。
//   - 窓前で at が [now, now+horizon] に入るイベントがあれば、最も近いものを
//     inWindow=false で返す (= これから来る仕込み対象)。
//   - minutes は at までの分数 (窓内で at 前なら正、at 後なら負)。
//   - 該当なしは ok=false。
//
// prompt の event_context と breakout arm 判定の gate の両方が使う。
func (c *EventCalendar) ActiveOrUpcoming(now time.Time, horizon time.Duration) (ev *CalendarEvent, minutes int, inWindow bool, ok bool) {
	if c == nil {
		return nil, 0, false, false
	}
	minsTo := func(e CalendarEvent) int { return int(e.At.Sub(now).Round(time.Minute) / time.Minute) }

	// 1. 窓内を優先。
	for i := range c.Events {
		if c.Events[i].contains(now) {
			return &c.Events[i], minsTo(c.Events[i]), true, true
		}
	}
	// 2. horizon 内で最も近い upcoming。
	var best *CalendarEvent
	var bestMins int
	for i := range c.Events {
		at := c.Events[i].At
		if at.After(now) && !at.After(now.Add(horizon)) {
			m := minsTo(c.Events[i])
			if best == nil || m < bestMins {
				best, bestMins = &c.Events[i], m
			}
		}
	}
	if best != nil {
		return best, bestMins, false, true
	}
	return nil, 0, false, false
}

// LoadEventCalendar parses configs/event_calendar.yaml. Missing file is
// treated as empty calendar (+ nil error) so monthly maintenance gaps don't
// prevent bot startup. Invalid YAML or wrong shape returns a real error.
func LoadEventCalendar(path string) (*EventCalendar, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &EventCalendar{}, nil
		}
		return nil, fmt.Errorf("read event calendar: %w", err)
	}
	var cal EventCalendar
	if err := yaml.Unmarshal(raw, &cal); err != nil {
		return nil, fmt.Errorf("parse event calendar: %w", err)
	}
	return &cal, nil
}
