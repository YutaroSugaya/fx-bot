package handler

import (
	"net/http"
	"sync"
	"time"

	"fx-bot/backend/internal/usecase/query"
)

// MarketHandler serves GET /api/market/state — the multi-timeframe snapshot
// shown above the manual-trade UI. Wraps GetMarketStateQuery with a small
// TTL cache so dashboard polling does not hammer GMO's klines endpoint
// (the 1D/1M/3M lookups round-trip the public API).
//
// Dashboard polls every 5s so the live spread next to the manual-trade
// button refreshes in near-real-time (the operator decides on the basis
// of the displayed spread under the override rule). 5s × ~4 klines per
// request = ~0.8 GMO public GET/s, well inside the 6 GET/s limit even
// when every poll misses the cache. Most polls hit the cache.
type MarketHandler struct {
	Query    *query.GetMarketStateQuery
	Symbol   string        // default symbol when ?symbol= is absent
	CacheTTL time.Duration // 0 → defaults to 5s

	mu    sync.Mutex
	cache map[string]cachedMarketState
}

type cachedMarketState struct {
	at   time.Time
	view *query.MarketStateView
}

// State は GET /api/market/state。
//
// ?symbol=USD_JPY 省略時は h.Symbol を使う。同じ symbol への連続アクセスは
// CacheTTL の範囲でキャッシュを返す (Live 化したときの GMO 連打防止)。
func (h *MarketHandler) State(w http.ResponseWriter, r *http.Request) {
	if h.Query == nil {
		WriteError(w, http.StatusServiceUnavailable, "market state query not configured")
		return
	}
	symbol := r.URL.Query().Get("symbol")
	if symbol == "" {
		symbol = h.Symbol
	}
	if symbol == "" {
		WriteError(w, http.StatusBadRequest, "symbol required")
		return
	}
	ttl := h.CacheTTL
	if ttl == 0 {
		ttl = 5 * time.Second
	}

	if cached, ok := h.readCache(symbol, ttl); ok {
		WriteJSON(w, http.StatusOK, cached)
		return
	}
	view, err := h.Query.Execute(r.Context(), symbol)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.writeCache(symbol, view)
	WriteJSON(w, http.StatusOK, view)
}

func (h *MarketHandler) readCache(symbol string, ttl time.Duration) (*query.MarketStateView, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, ok := h.cache[symbol]
	if !ok {
		return nil, false
	}
	if time.Since(c.at) > ttl {
		return nil, false
	}
	return c.view, true
}

func (h *MarketHandler) writeCache(symbol string, v *query.MarketStateView) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cache == nil {
		h.cache = map[string]cachedMarketState{}
	}
	h.cache[symbol] = cachedMarketState{at: time.Now(), view: v}
}
