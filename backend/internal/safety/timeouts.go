package safety

import "time"

// ResolveExecutionTimeout は GMO の /v1/executions ポーリングで positionId を
// 取得する待ち時間。MARKET fill は通常 1 秒以内に返るが、API 過負荷時の
// バッファを含めて 10 秒。
//
// この値を超えても約定が解決できない場合、entry 側 (LiveExitProtector) と
// close saga (close_saga.go) が emergency_stop を発火する。
const ResolveExecutionTimeout = 10 * time.Second

// DefaultManualMaxHoldMinutes はダッシュボードの手動売買フォームで
// 「保有上限 (分)」が未指定 / 0 のときに使うフォールバック値。
const DefaultManualMaxHoldMinutes = 240

// DefaultQuantity は signal が quantity を持たない/0 のときのデフォルト
// 通貨数。GMO 外為 FX で実証済みの最小発注単位 = 1,000 通貨 (= 0.1 lot)
// に揃える。Live は hard_limits.quantity.min/max でさらに上書きされる。
//
// production code path では manual_trade.HardLimits != nil が常に成立する
// (起動時に hard_limits.yaml を読み込む) ため、この const は HardLimits を
// 渡さないテスト経路 + backtest engine の ultimate fallback でのみ参照される。
const DefaultQuantity = 1000
