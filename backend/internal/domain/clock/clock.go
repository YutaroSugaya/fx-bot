// Package clock は usecase / handler 層に注入する時刻ソースを提供する。
//
// 動機: production で `time.Now()` を直接呼ぶと、early_exit / ratchet TP /
// cooldown など時刻依存の取引判定を clock mock できずテストできない。
// このパッケージは小さい「足場」で、ClosePositionCommand などから
// 段階的に Clock 注入を広げる。
//
// 既存 `Now func() time.Time` 注入 (scheduler / GmoBroker / rate limiter 等)
// は壊さない。FromFunc アダプタで両方式を橋渡しできる。
package clock

import (
	"sync"
	"time"
)

// Clock は注入用の時刻 interface。usecase / handler はこれを保持する。
type Clock interface {
	Now() time.Time
}

// System は production 用のシングルトン clock (= time.Now のラッパ)。
//
//	cmd := &command.ClosePositionCommand{Clock: clock.System, ...}
var System Clock = systemClock{}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// Fake は test 用のコントローラ。Now() は内部の固定 t を返し、
// Advance(d) / Set(t) で進められる。複数 goroutine から安全。
type Fake struct {
	mu sync.RWMutex
	t  time.Time
}

// NewFake は初期時刻 t を持つ Fake を返す。
func NewFake(t time.Time) *Fake { return &Fake{t: t} }

// Now は現在の固定時刻を返す。
func (f *Fake) Now() time.Time {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.t
}

// Advance は内部時計を d だけ進める。
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

// Set は内部時計を t に書き換える (時間を巻き戻すテストにも使える)。
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = t
}

// FromFunc は既存の `Now func() time.Time` フィールド (scheduler /
// GmoBroker など) を Clock interface に橋渡しするアダプタ。
// 既存 wiring を壊さず、段階移行で使う。
func FromFunc(now func() time.Time) Clock { return funcClock(now) }

type funcClock func() time.Time

func (f funcClock) Now() time.Time { return f() }
