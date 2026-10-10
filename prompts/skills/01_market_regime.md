# Skill: 市況分類 (market_regime)

> ⚠️ **regime の方向判定の主軸は
> summary_6h(主)+ summary_24h(大局)**。summary_1h は「6hと同方向か・逆行が浅い押し目/戻りか」の
> 確認に使い、summary_15m/5m は regime の方向判定には使わない(scalp/probe lane の値幅・レンジ端の判断に使う。下記)。**6h が unclear/方向不明なら regime も unclear 寄り**に
> 倒し、no_trade を促す(下位足だけのトレンドで trend 判定しない)。

Input JSON の **全フィールド** を読み、現在の相場状態を 1 つに分類する。
1h だけ、24h だけで判断せず、必ず複数の時間軸 + 補助情報をクロスチェックする。

## 読むべきフィールド一覧

### 価格 window (`summary_15m / summary_1h / summary_6h / summary_24h`)

この bot は同じ symbol でも **daytrade lane** と **scalp/event/probe lane** を分ける。
`summary_6h / summary_24h` は daytrade の主軸、`summary_15m / summary_1h` は
scalp/probe の主軸として読む。1 つの時間軸だけで全ポジションの方針を決めない。

各 window で以下を確認:

| フィールド | 何を意味するか | 使い方 |
|---|---|---|
| `trend_direction` | "up" / "down" / "flat" | 3 window の一致度を見る |
| `range_pips` | 期間内の高値-安値幅 | 値幅の絶対量。trend 強度の指標 |
| `realized_volatility` | log return の標準偏差ベースの実現ボラ | 動きの激しさ |
| `high` / `low` | 期間内の高値/安値 | 現値との位置関係 (上限近い? 下限近い?) |
| `support` / `resistance` | サポート/レジスタンス水準 (もしあれば) | レンジ境界の特定。breakout 判定の鍵 |
| `avg_spread_pips` / `max_spread_pips` | 期間内のスプレッド平均/最大 | 流動性の状態。普段との比較 |
| `num_candles` | window 内の足本数 | データ不足判定 (1h で 30 本未満なら不信頼) |

### 現在値 (`current_rate`)

- `bid` / `ask` / `spread_pips` — 現在のスプレッド状況
- 1h の `avg_spread_pips` と比較して 2 倍以上なら異常

### Bot 状態 (`bot_state`)

- `consecutive_losses` — 2 以上なら confidence を下げる
- `daily_pnl_jpy` — 当日マイナス幅が大きいほど慎重に
- `trades_in_current_window` — 既に取引済み → 過剰売買を避ける

### 補助情報

- `recent_trades` — 直近の close_reason を見て stop_loss が連発していないか
- `recent_rejections` — どの理由で reject されているか (max_spread_pips 起因なら市況不適)

## 出力する market_regime.type の判定フロー

**順に判定し、最初に該当した type を採用する**:

1. **データ不足 → `unclear`**
   - `summary_1h.num_candles < 30` または `summary_6h.num_candles < 30`
   - confidence は 0.3〜0.4

2. **異常スプレッド → `volatile`**
   - `current_rate.spread_pips > summary_1h.avg_spread_pips * 2`
   - または `summary_1h.max_spread_pips > hard_limits.max_spread_pips`
   - confidence 0.6〜0.8 (異常さの度合いに応じて)

3. **高ボラ → `volatile`**
   - `summary_1h.realized_volatility > summary_24h.realized_volatility * 2.0`
   - または `summary_1h.range_pips > summary_24h.range_pips * 0.5` (短時間で 1 日値幅の半分以上動いた)
   - confidence 0.6〜0.8

4. **上昇トレンド → `trend_up`** (デイトレでは 6h を主軸、24h は補強情報)
   - 必須: `summary_6h.trend_direction == "up"`
   - 24h も "up" → confidence 0.70〜0.85 (両軸一致で強い)
   - 24h が "flat" → confidence 0.50〜0.65 (新興トレンド、momentum_pullback の主戦場)
   - 24h が "down" → confidence 0.35〜0.50 (反転 or 短期戻り、慎重に momentum_pullback)
   - **補強**: 1h も "up" → +0.05、1h が "flat/down" は押し目で許容
   - **補強**: 現在値が `summary_24h.resistance` を上抜けていれば +0.05

5. **下降トレンド → `trend_down`** (デイトレでは 6h を主軸、24h は補強情報)
   - 必須: `summary_6h.trend_direction == "down"`
   - 24h も "down" → confidence 0.70〜0.85 (両軸一致で強い)
   - 24h が "flat" → confidence 0.50〜0.65 (新興トレンド、momentum_pullback の主戦場)
   - 24h が "up" → confidence 0.35〜0.50 (反転 or 短期戻り、慎重に momentum_pullback)
   - **補強**: 1h も "down" → +0.05、1h が "flat/up" は戻りで許容
   - **補強**: 現在値が `summary_24h.support` を下抜けていれば +0.05

6. **レンジ → `range`**
   - 必須: `summary_6h.trend_direction == "flat"` かつ `summary_24h.trend_direction == "flat"`
   - かつ `summary_6h.range_pips <= summary_24h.range_pips * 0.5` (動きが落ち着いている)
   - 補強: `support` と `resistance` が両方定義されていて、現在値がその間にある → confidence 0.6〜0.75
   - **重要**: daytrade lane では range は原則 `no_trade`。ただし probe lane では
     `summary_1h.range_pips` が 8〜24pips 程度あり、現在値が support/resistance 近辺に
     寄っているなら、抜け前に小さく試す候補になる。range_reversion は廃止済みなので逆張りではなく
     `range_breakout_probe` (require_breakout=false) を使う (skill 02 のフロー 4)。

7. **上記いずれにも当てはまらない → `unclear`** (滅多に発火しないはず)
   - 6h が "flat" かつ Step 6 (range) の range_pips 条件も満たさない、というレアケースのみ
   - **6h と 24h の方向不一致は Step 4/5 で trend として扱う** (= unclear に倒さない)
   - **1h と 6h の方向不一致は押し目/戻りのサインなので Step 4/5 で momentum_pullback 候補として扱う**
   - confidence 0.3〜0.5

## confidence の最終調整

判定後、以下で confidence を上下させる:

- `bot_state.consecutive_losses >= 2` → **0.10 減**
- `bot_state.consecutive_losses >= 3` → **追加で 0.10 減**(合計 -0.20)
- `current_rate.spread_pips > hard_limits.max_spread_pips * 1.5` → **0.15 減**
- `recent_trades` の直近 5 件中 `stop_loss` が 3 件以上 → **0.10 減**
- 最終 confidence は **0.0〜1.0 に clamp**

`confidence < 0.3` のときは、`type` が `unclear` / `volatile` 以外でも **`unclear` に上書き** する。
(0.3〜0.5 の "weak trend" は分類として残し、daytrade にするか scalp/probe で小さく試すかは
skill 02 と root の lane 判定に委ねる。0.3 未満はノイズとして扱う。なお enabled config は
confidence < 0.35 だと Go validator が reject する。)

## lane_hint の出し方

subagent 出力では `lane_hint` を付ける。最終 YAML には含めない。

| lane_hint | 条件 |
|---|---|
| `daytrade` | trend_up/down かつ confidence >= 0.70 かつ 24h range_pips >= 80 |
| `scalp` | 弱 trend、または 1h range_pips 8〜24 で 15m/1h に短期方向がある |
| `event` | event_context.policy=breakout がある |
| `probe` | range かつ 1h support/resistance 近辺でブレイク待ちに向く |
| `none` | unclear / volatile / spread 異常 / 値幅不足 |

## reason の書き方 (150 文字以内、日本語)

「**どの window のどの数値からこう判断したか**」を具体的な数字付きで書く。
「総合判断」「総合的に見て」のような曖昧表現は禁止。

良い例:
```
6h/24h trend_direction=up一致、1hで押し目 (down) 形成中。24h range_pips=85に対し6h range_pips=42 (50%)。realized_volatility 6h=0.12と24hの0.10と整合的、ノイズではなく構造的トレンド。momentum_pullback 適。confidence 0.72。
```

悪い例:
```
全体的に上昇傾向。confidence 0.7。
```

## アンチパターン (やりがちなミス)

- ❌ 1h だけ見て trend_up と判断 → デイトレは 6h/24h を主軸に
- ❌ 24h だけ見て range と判断 → デイトレで range と出すと no_trade になるので慎重に
- ❌ trend_direction だけ見て range_pips を見ない → 値幅が極端な場合は volatile
- ❌ support/resistance を無視 → ブレイク or 押し目判断の鍵
- ❌ recent_trades の close_reason を無視 → 連敗中なら同じ方向のエントリーは避ける
- ❌ 1h と 6h/24h の方向不一致を即 unclear に倒す → デイトレでは押し目/戻りのサインなので momentum_pullback を検討
