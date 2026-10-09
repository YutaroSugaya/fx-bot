package safety

import (
	"sort"
	"sync"
	"time"
)

// PendingPositions は port.PendingPositionTracker の in-memory 実装。
//
// entry saga (PlaceOrder → ResolveExecution → PlaceSettleOCO → DB INSERT) の
// 数秒間、broker には position が存在するが DB にはまだ INSERT されていない
// race-window を持つ。reconcile loop がこの間に走ると「裸 broker position」と
// 誤検出するため、ResolveExecution で positionId を取った時点から DB INSERT
// 終端 (成功 / 失敗どちらも) までを MarkPending / MarkResolved で囲い、その間
// reconcile に skip させる。
//
// 並行安全 (sync.Mutex)。空 id は no-op (上流の critical failure 等で空が
// 来てもこの層で防御)。
type PendingPositions struct {
	mu  sync.Mutex
	ids map[string]time.Time // brokerPositionID → MarkPending 時刻
	now func() time.Time
}

// NewPendingPositions は空の tracker を返す (clock = time.Now)。
func NewPendingPositions() *PendingPositions {
	return NewPendingPositionsWithClock(time.Now)
}

// NewPendingPositionsWithClock は clock 注入版 (StaleIDs テスト用)。
func NewPendingPositionsWithClock(now func() time.Time) *PendingPositions {
	if now == nil {
		now = time.Now
	}
	return &PendingPositions{ids: map[string]time.Time{}, now: now}
}

// MarkPending は brokerPositionID を pending として登録する (時刻を記録)。空文字は no-op。
func (p *PendingPositions) MarkPending(brokerPositionID string) {
	if brokerPositionID == "" {
		return
	}
	p.mu.Lock()
	p.ids[brokerPositionID] = p.now()
	p.mu.Unlock()
}

// StaleIDs は MarkPending から ttl 以上経過しても MarkResolved されていない
// brokerPositionID を返す。entry saga がクラッシュして終端
// defer が呼ばれなかった場合、その id は永久 pending になり reconcile が当該 broker
// position を無期限に skip = 裸ポジションを隠す。reconcile はこれを使って「TTL 超過の
// pending はもう pending 扱いしない」と判断できる。返り値は決定的順序 (sorted)。
func (p *PendingPositions) StaleIDs(ttl time.Duration) []string {
	cutoff := p.now().Add(-ttl)
	p.mu.Lock()
	var stale []string
	for id, markedAt := range p.ids {
		if markedAt.Before(cutoff) {
			stale = append(stale, id)
		}
	}
	p.mu.Unlock()
	sort.Strings(stale)
	return stale
}

// MarkResolved は brokerPositionID を pending から除外する。
// 未登録 id / 二重呼び出しは no-op (entry saga の終端 defer で常に呼ばれる
// 想定なので、エラーパスでも安全に流す)。
func (p *PendingPositions) MarkResolved(brokerPositionID string) {
	if brokerPositionID == "" {
		return
	}
	p.mu.Lock()
	delete(p.ids, brokerPositionID)
	p.mu.Unlock()
}

// IsPending は brokerPositionID が現在 pending か返す。
func (p *PendingPositions) IsPending(brokerPositionID string) bool {
	if brokerPositionID == "" {
		return false
	}
	p.mu.Lock()
	_, ok := p.ids[brokerPositionID]
	p.mu.Unlock()
	return ok
}
