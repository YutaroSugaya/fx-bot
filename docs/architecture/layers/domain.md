# Layer: Domain

## 役割 (1 行)

純粋 Go のビジネスモデル。Entity / Value Object / Domain Service を持ち、外部 I/O 一切なし。

---

## やること (do)

- ビジネスルールを **入力 → 出力** の純粋関数で表現する
- Entity と Value Object を定義する (`Position`, `Order`, `Signal`, `Ticker`, `Candle`)
- Domain Service として **同じ入力に対し同じ出力を返す** 計算を提供する (`ComputeTPSLPrices`, `Engine.Evaluate`, `Gate.EvaluateSignal`)
- 単体テストは fake / mock を使わず純粋単体で書く

---

## やらないこと (don't)

- ❌ `pgx`, `net/http`, `log/slog`, `os` を import する
- ❌ time.Now() を直接呼ぶ (`Clock func() time.Time` を引数で受ける、または caller が `now` を渡す)
- ❌ rand を直接呼ぶ (deterministic ID は caller が注入する) — `strategy.Engine.SignalIDFn` は wiring 層 (`cmd/bot/loops.go`) から `crypto/rand` 実装を注入する
- ❌ goroutine / channel / mutex を持つ (= 状態を持つ純粋関数の集合に留める)
- ❌ Repository interface (`port.PositionRepository` 等) を import する

### 例外: domain/market の stateful buffer (許容)

`domain/market/aggregator.go` の `Aggregator` と `domain/market/candle.go` の
`RingBuffer` は **ローカル mutex を保持する例外**。これらは「複数の goroutine
(price loop / advisor cycle / handler) から共有される時系列バッファ」であり、
本質的に stateful 共有データ。これを `app` 層に移すと:

- `app` が candle resample / window aggregate の domain 計算を抱えることになり
  domain 純度がさらに崩れる
- 全 goroutine が同じ buffer を見るための同期は結局必要 (= mutex を別の場所に
  置き直すだけで責務分離にはならない)

ルール: **domain/market のこの 2 型に限り mutex を許容**。
他の domain サブパッケージ (position / order / risk / strategy) は引き続き
mutex 禁止。新規 stateful buffer を作る場合は、まず本ファイルに新例外として
明記してからにする (= 暗黙の蓄積を防ぐ)。

---

## 命名 / 配置

| 種別 | 場所 | ファイル名 | 例 |
|---|---|---|---|
| Aggregate ディレクトリ | `domain/<aggregate>/` | — | `domain/position/` |
| Entity | `domain/<aggregate>/` | `<entity>.go` | `domain/position/position.go` |
| Value Object | `domain/<aggregate>/` | `<vo>.go` | `domain/market/tick.go` (pip size 写像) |
| Domain Service | `domain/<aggregate>/` | `<service>.go` | `domain/position/price.go` |

### Aggregate 一覧

- [domain/market/](../../../backend/internal/domain/market/) — Tick / Candle / Aggregator / Stats / Summary (pip size は tick.go 内)
- [domain/strategy/](../../../backend/internal/domain/strategy/) — Engine + 各戦略 (momentum_pullback / breakout_follow / range_breakout_probe / mtf_pullback / ma_pullback (+v2) / trend_follow / daily_trend / london_breakout / gotobi_fix / signature_breakout / exhaustion_fade / llm_decision / no_trade)。Signal を返す純粋関数 (実在一覧は同パッケージのファイルが正)
- [domain/position/](../../../backend/internal/domain/position/) — ComputeTPSLPrices / ComputeClosePnL / State 遷移 ([STATE_MACHINE.md](../../runtime/STATE_MACHINE.md))
- [domain/order/](../../../backend/internal/domain/order/) — PlaceOrderRequest / Order / Execution
- [domain/risk/](../../../backend/internal/domain/risk/) — Gate (cooldown / max_open_positions / daily_loss / consecutive_losses 判定)
- [domain/clock/](../../../backend/internal/domain/clock/) — `Clock` interface (testability。`time.Now()` 直書き禁止の代替)

---

## テスト方法 (この層特有)

- **fake / mock 不要**。値の入出力だけで完結
- 同じ動作軸の複数ケースは **table-driven** で書く

```go
func TestComputeTPSLPrices(t *testing.T) {
    cases := []struct {
        name              string
        side              order.Side
        entry, tp, sl, pip float64
        wantTP, wantSL    float64
    }{
        {"BUY",  order.SideBuy,  100.00, 20, 15, 0.01, 100.20, 99.85},
        {"SELL", order.SideSell, 100.00, 20, 15, 0.01,  99.80, 100.15},
        {"zero pips returns entry", order.SideBuy, 100.00, 0, 0, 0.01, 100.00, 100.00},
    }
    for _, tc := range cases {
        t.Run(tc.name, func(t *testing.T) { ... })
    }
}
```

詳細は [TESTING.md](../../workflows/TESTING.md) を参照。

---

## 既存実装の代表例

- [backend/internal/domain/market/aggregator.go](../../../backend/internal/domain/market/aggregator.go) — 1m/5m/15m/1h の rolling candle buffer
- [backend/internal/domain/market/tick.go](../../../backend/internal/domain/market/tick.go) — symbol → pip size の写像 (JPY-quote=0.01 / USD-quote=0.0001)
- [backend/internal/domain/market/quote.go](../../../backend/internal/domain/market/quote.go) — symbol → 決済通貨 (`QuoteCurrency`) と quote→JPY 換算倍率 (`QuoteJPYRate`)。USD-quote ペア (EUR_USD 等) の USD 建て損益を JPY に直すための純粋写像。`usecase/command/resolve_quote_jpy_rate.go` が broker から引いた USD/JPY と組み合わせて使い、`position.ComputeClosePnL` が倍率を受け取る
- [backend/internal/domain/strategy/engine.go](../../../backend/internal/domain/strategy/engine.go) — strategy registry + Evaluate
- [backend/internal/domain/strategy/momentum_pullback.go](../../../backend/internal/domain/strategy/momentum_pullback.go) — 個別 strategy 実装例
- [backend/internal/domain/position/price.go](../../../backend/internal/domain/position/price.go) — `ComputeTPSLPrices` (side / entry / TP pips / SL pips / pip size → TP/SL 価格)
- [backend/internal/domain/risk/gate.go](../../../backend/internal/domain/risk/gate.go) — 10 ゲートのリスク判定 (cooldown / max_open / daily_loss / consec_losses / allowed_hours_jst …)

---

## 既知の例外 / 注意点

### `domain` → `config` 依存

現状 `domain/strategy` と `domain/risk` は `config.StrategyConfig` / `config.HardLimits` を import している。
これは domain 純粋性の弱い違反だが、`config` package は strategy schema と value object の正本でもあるため
**現時点では許容**。

### `strategy.Engine` の SignalID 生成

`strategy.Engine.SignalIDFn` (type `SignalIDGenerator`) を wiring 層
(`cmd/bot/loops.go:newSignalIDGenerator`) から注入する。
backtest / replay は決定的 ID generator を注入できる。

---

## アンチパターン (= 過去にやってしまった失敗)

- ❌ `domain/position/price.go` で `log.Printf` を呼んだ — slog import に汚染するので caller に返す型を増やした
- ❌ `Engine.Evaluate` の中で `time.Now()` を直接呼んだ — `now time.Time` 引数注入に直した
- ❌ Position entity 内に `gorm` tag を付与した (DB マッピング混入) — Repository record (`port.PositionRecord`) と分離した

---

## 関連 docs

- [usecase.md](usecase.md) — domain を呼ぶ層
- [port.md](port.md) — domain が依存する interface
- [TESTING.md](../../workflows/TESTING.md) — 古典派 TDD のドメインテスト
- [BACKTEST.md](../../workflows/BACKTEST.md) — domain logic の deterministic replay
