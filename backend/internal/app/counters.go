package app

import "sync/atomic"

// Counters holds atomic counters for Live-critical observability.
//
// 全フィールドは atomic.Int64。Incr / Load は go-routine safe。
// /api/status から Snapshot() 経由で値を取り出して JSON 出力する。
//
// 既存の ticker_errors は APIServer.tickerErrors に残してあるが、新規追加
// 分はここに集約 (operator が一箇所で見られるように)。
type Counters struct {
	TickerErrors        atomic.Int64 // worker の Broker.GetTicker 失敗
	EmergencyTrips      atomic.Int64 // emergency_stop.flag を発火した回数
	ResolveTimeouts     atomic.Int64 // ResolveExecution が timeout した回数
	CloseRaces          atomic.Int64 // CloseAndRecord ok=false (二重 close 検出) 回数
	NakedPositions      atomic.Int64 // reconcile で naked_broker_position を検出した回数
	TradingCycleMissing atomic.Int64 // priceTick: Worker.TradingCycle == nil (配線抜け検知)
}

// CountersSnapshot is the JSON shape for /api/status.
type CountersSnapshot struct {
	TickerErrors        int64 `json:"ticker_errors"`
	EmergencyTrips      int64 `json:"emergency_trips"`
	ResolveTimeouts     int64 `json:"resolve_timeouts"`
	CloseRaces          int64 `json:"close_races"`
	NakedPositions      int64 `json:"naked_positions"`
	TradingCycleMissing int64 `json:"trading_cycle_missing"`
}

func (c *Counters) Snapshot() CountersSnapshot {
	return CountersSnapshot{
		TickerErrors:        c.TickerErrors.Load(),
		EmergencyTrips:      c.EmergencyTrips.Load(),
		ResolveTimeouts:     c.ResolveTimeouts.Load(),
		CloseRaces:          c.CloseRaces.Load(),
		NakedPositions:      c.NakedPositions.Load(),
		TradingCycleMissing: c.TradingCycleMissing.Load(),
	}
}

// Incr* are the trading-path increment hooks (usecase/command bumps these via a
// small interface so it need not import the app layer). go-routine safe.
func (c *Counters) IncrNakedPositions()  { c.NakedPositions.Add(1) }
func (c *Counters) IncrCloseRaces()      { c.CloseRaces.Add(1) }
func (c *Counters) IncrResolveTimeouts() { c.ResolveTimeouts.Add(1) }
