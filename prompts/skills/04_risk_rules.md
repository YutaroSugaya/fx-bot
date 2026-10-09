# Skill: リスクルール (no_trade トリガー / risk セクション)

## 強制 no_trade のトリガー

以下のいずれかが該当する場合、**他の判断より優先して** `no_trade` を出力する:

| 条件 | 入力 JSON のフィールド | 判定 |
|---|---|---|
| 緊急停止フラグ | `bot_state.emergency_stop` | true なら no_trade |
| 当日の損失が上限到達 | `bot_state.daily_pnl_jpy` | `bot_config.risk.max_daily_loss_jpy` 以上の損失なら no_trade (例: qty=1000 で 2,000 JPY) |
| 連敗中 (4 以上) | `bot_state.consecutive_losses` | `bot_config.risk.max_consecutive_losses` (= 4) 以上なら no_trade |
| ポジション枠が埋まった | `bot_state.open_positions_count` | この symbol の open 数。**lane 枠 (daytrade 1 / event 2 / scalp・probe 3) 以上**なら同一 symbol の新規は no_trade。枠未満なら同サイズ追加可 (④) |
| スプレッド異常 | `current_rate.spread_pips` | hard_limits の max_spread_pips 超なら no_trade |
| データ不足 | `summary_1h.num_candles` 等 | 1h で 30 本未満なら no_trade |

### graduated 連敗 cooldown (Go 側で自動適用)

3 連敗以下では `no_trade` を返さなくて良い。Go の risk.Gate / Executor が以下を自動適用する:

| 連敗数 | Go 側の挙動 |
|---:|---|
| 2 | entry 許可 (`qty` は減らさない) |
| 3 | 直近 SL の closed_at から 30 分 freeze (gate が reject) |
| 4 | 当日 `no_trade` 強制 (binary cap = `max_consecutive_losses`) |

prompt 側は **連敗 0-3 では通常通り戦略選択**してよい。4 以上のみ強制 no_trade。

### 経済指標 freeze (Go 側自動適用)

`configs/event_calendar.yaml` に列挙された経済指標 (NFP / FOMC / 日銀政策決定会合 等)
の発表前後 freeze 窓内では、risk.Gate が **emergency_stop と同列の hard gate** として
entry を `event_freeze` で reject する (operator override 不可)。

prompt 側の振る舞い:
- `time` フィールド (UTC) を JST に変換した時刻が、よく知られている指標時間帯
  (FOMC 22:00-24:00 JST など) に当たっている場合は `no_trade` を出すのが望ましい
- ただし freeze の最終判定は Go の `EventCalendar.InFreezeWindow` が行うので、
  prompt 側で確実に拾えなくても安全側に倒れる
- 月 1 でカレンダーを更新する想定。重要発表が漏れたら運用バグとして検知

### 同方向リトライ禁止 (Go 側自動適用)

当日 (bot timezone) に **同方向で SL を 2 回踏んだ** 時点で、その方向は当日 `no_trade`
強制 (gate が direction_buy_blocked_after_2_sl_today / direction_sell_... で reject)。
SL hit 限定 (= `close_reason="stop_loss"`)。早期 exit の小損は数えない。

prompt 側の振る舞い:
- BUY 側が 2 SL 済みなら trend_up でも SELL 方向を取れる戦略以外は `no_trade`
- 同様に SELL 側が 2 SL 済みなら trend_down でも `no_trade`
- 当日の `recent_trades` で SL by side を観察して、明らかに同方向が詰まっているなら
  反対方向の `momentum_pullback` を優先する

## no_trade のときの出力規約

`no_trade` のときは以下を**必ず**満たす:

```yaml
enabled: false
strategy:
  name: no_trade
entry:
  direction: none
exit:
  take_profit_pips: 0
  stop_loss_pips: 0
  max_hold_minutes: 0
risk:
  quantity: 0
no_trade:
  enabled: true
  reason: "<具体的な理由を日本語で>"
```

これらの整合性は Go validator がチェックする。1 つでも欠けると reject される。

## 通常エントリー時の risk セクション

`no_trade` 以外のとき:

```yaml
risk:
  quantity: 1000                       # 必ず 1000 で固定 (= 0.1 lot)
  max_open_positions: 3                # lane で決める (daytrade 1 / event 2 / scalp・probe 3)。④
  max_trades_in_this_window: 4         # 2〜5 の範囲 (スキャ 60-120分窓)。連敗中は 2 に
  max_loss_in_this_window_jpy: 1000    # 750〜1500 の範囲 (SL 6-15pips × 1000通貨 = 60-150円/trade × ~5 trades)
```

## ポジション枠の考え方 (④ cap 3 を活かす)

bot_config は per-symbol / account-wide とも cap 3。**`risk.max_open_positions` は lane に応じて
daytrade 1 / event 2 / scalp・probe 3 を出す** (一律 1 にすると
`open_positions 1 >= cap 1` の reject が大量に出て回転が死ぬ)。

- `open_positions_count < lane 枠`: この symbol で同サイズ (1000 固定) の追加 entry 可。
  probe/scalp は「抜け/初動を枠まで並べて何度も試す」設計なので複数同時保有が正。
- `open_positions_count >= lane 枠`: この symbol は no_trade。別 symbol は account-wide gate が管理。
- **枠を増やしてもナンピンにはしない**: ロットは 1000 固定、負け後の建て増し・averaging down は禁止
  (下の「ナンピン厳禁」参照)。許可するのは同サイズの並列試行だけ。
- 同方向 SL が 2 回出た方向は、その日その方向を止める (これは枠とは別の損失ガード、従来通り維持)。

## 過剰売買防止

- daytrade lane は 1〜2 トレードを基本ライン
- scalp/event/probe lane は 3〜5 トレードを基本ライン
- 直近で連敗していれば 2 に絞る
- ボラが高い (`volatile`) ときは max_trades を上げない。むしろ no_trade を選ぶ
- cap に達して reject が多発しているときは 4〜5 も許容 (Paper 段階は edge 収集優先)

## ナンピン / マーチンゲール 厳禁 (④ で「枠まで並列試行」を許可した上での線引き)

- **ロットを増やす提案 → 出さない** (`risk.quantity` は常に 1000 固定 = 0.1 lot)
- **損失後に取り返すための建て増し / averaging down → 禁止**
- 一方で **同サイズ (1000) の probe/scalp を lane 枠 (最大 3) まで並べる**のは許可 (= ナンピンではない)。
  これは「抜け/初動の試行回数を増やす」probe lane の設計であって、ロット増でも損失補填でもない。
- `risk.quantity` は常に 1000 固定 (= 0.1 lot。絶対に変えない)
