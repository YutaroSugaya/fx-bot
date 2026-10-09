# Skill: 出力フォーマット (YAML スキーマ)

最終出力は YAML のみ。Markdown コードフェンス (```yaml `````) や説明文・コメントは禁止。

## スキーマ (全フィールド必須)

```yaml
config_id: string                      # 形式 "YYYYMMDD-HHMMSS-<symbol_lower_no_underscore>" (USD_JPY → usdjpy, EUR_JPY → eurjpy)。HHMMSS は valid_from ではなく Input JSON の `time` フィールド (= summary 生成時刻、秒精度) を使う。手動トリガで同一時間内に複数回走る可能性があるので必ずユニークにすること
generated_at: string                   # 裸の RFC3339 のみ (例: "2026-05-15T10:00:00+09:00")。末尾に "(JST)"/"(UTC)" 等のラベルを付けない (time parse が落ちて却下される)
valid_from: string                     # ISO8601。Input JSON の next_valid_from を必ず使う
valid_until: string                    # valid_from + 60 分 (= 必ず 1 時間。許容 60-120)

symbol: USD_JPY                        # Input JSON の `symbol` をそのままコピー (例: USD_JPY / EUR_JPY)。出力で別 symbol にすると Go 側 symbol-mismatch reject
enabled: boolean                       # no_trade 時は false、それ以外は true

market_regime:
  type: range | trend_up | trend_down | volatile | unclear
  confidence: number                   # 0.0〜1.0
  reason: string                       # 日本語で 100 文字程度

strategy:
  name: momentum_pullback | breakout_follow | range_breakout_probe | no_trade

entry:
  max_spread_pips: number              # 0.3〜1.5 (hard_limits)。平常 1.0、明確なチャンスのみ最大 1.5 (skill 03/07)
  require_breakout: boolean            # breakout_follow のみ true、momentum_pullback / range_breakout_probe / no_trade は false
  direction: buy_only | sell_only | both | none
  allowed_hours_jst: [array of int]    # 任意。JST hour 0-23 の whitelist。空 or 省略=全時間帯許可
                                       # 時間帯で絞るかは skill 02 に従う (既定は全時間帯。
                                       # 時間帯別の backtest 成績は選別バイアスで反転しやすい)
  max_chase_pips: number               # 任意。0=無効 (default)。1〜100 (sanity)。momentum_pullback 専用。
                                       # 直近 chase_lookback_candles 本の 5m ベースから entry が
                                       # この pips 以上離れていたら「追いかけ」として見送る (chasing_extended)。
  chase_lookback_candles: integer      # 任意。0=無効 (default)。1〜288 (=24h)。max_chase_pips とペアで有効。
                                       # 例: 12 (=60分)。breakout_follow / range_breakout_probe には設定しない。

exit:
  take_profit_pips: number             # skill 03 の戦略別レンジ厳守。no_trade は 0
  stop_loss_pips: number               # skill 03。no_trade は 0
  max_hold_minutes: integer            # 30〜120 (スキャ寄り)。no_trade は 0
  extension_max_minutes: integer       # 任意。0=無効 (default)。1..240 で MaxHold 後の grace。
                                       # 非対称ルール: soft deadline 後、勝ち/フラット
                                       # (unrealized_pips ≥ -threshold) の間は延長して続伸を待つ。
                                       # 負けが -threshold を超えたら soft deadline で損切り close。
                                       # daytrade trend tier で「勝ち継続を伸ばす」のに使う (利確は ratchet)。
  extension_unrealized_pips_threshold: number  # 任意。extension_max_minutes>0 のとき必須 (>0)。
                                       # = 延長中に許容する含み損の上限 (pips)。推奨 SL の 1/2 程度。
  early_exit_window_minutes: integer   # 任意。0=無効 (default)。1..max_hold_minutes (上限 240)。
                                       # MaxHold soft deadline の手前の window 内で PnL が
                                       # early_exit_target_pips 以上に達したら即 close。
                                       # MaxHold 強制 close の最悪損失を浅くする (skill 03 参照)。
                                       # 推奨デフォルト: 15 (スキャ。MaxHold 以下必須)
  early_exit_target_pips: number       # 任意。-100..100 (sanity)。通常はマイナス値 (例: -2)。
                                       # window 内で「これ以上悪化させずに諦める」閾値。
                                       # early_exit_window_minutes>0 のとき意味を持つ。
                                       # 推奨デフォルト: -2.0

risk:
  quantity: 1000                       # 1000 固定 (= 0.1 lot)。no_trade は 0
  max_open_positions: 1                # 1 固定
  max_trades_in_this_window: integer   # 1〜5 (スキャで回転増)。no_trade は 0 でも OK
  max_loss_in_this_window_jpy: integer # 750〜1500。no_trade は 0 でも OK

no_trade:
  enabled: boolean                     # strategy=no_trade なら true。それ以外は false
  reason: string                       # enabled=true のとき必須。日本語で具体的に

next_advisor_run_in_minutes: integer   # 15〜480。skill 06 のテーブル参照。0 は fallback (= auto 60min)
```

## 整合性ルール (Go validator が必ずチェック)

- `enabled=false` または `no_trade.enabled=true` のとき、以下はすべて 0 か none:
  - `strategy.name = no_trade`
  - `entry.direction = none`
  - `exit.*` すべて 0
  - `risk.quantity = 0`
- `valid_until - valid_from` は **必ず 60 分** (許容: 60〜120 分)
- `confidence` は 0.0〜1.0
- `direction` と `market_regime` の組み合わせ整合 (trend_up + sell_only は禁止)

## 出力例 (正常エントリー / momentum_pullback)

```yaml
config_id: "20260515-103247-usdjpy"
generated_at: "2026-05-15T10:00:00+09:00"
valid_from: "2026-05-15T10:00:00+09:00"
valid_until: "2026-05-15T11:00:00+09:00"
symbol: USD_JPY
enabled: true
market_regime:
  type: trend_up
  confidence: 0.72
  reason: "6h/24hでtrend_directionがupで一致、1hで押し目形成 (range_pipsが24hの30%以下に収束)"
strategy:
  name: momentum_pullback
entry:
  max_spread_pips: 0.5
  require_breakout: false
  direction: buy_only
exit:
  take_profit_pips: 12.0
  stop_loss_pips: 8.0
  max_hold_minutes: 60
  early_exit_window_minutes: 15
  early_exit_target_pips: -2.0
  ratchet_arm_pips: 8.0
  ratchet_giveback_pips: 2.0
risk:
  quantity: 1000
  max_open_positions: 1
  max_trades_in_this_window: 4
  max_loss_in_this_window_jpy: 1000
no_trade:
  enabled: false
  reason: ""
next_advisor_run_in_minutes: 60
```

## 出力例 (no_trade)

```yaml
config_id: "20260515-103247-usdjpy"
generated_at: "2026-05-15T10:00:00+09:00"
valid_from: "2026-05-15T10:00:00+09:00"
valid_until: "2026-05-15T11:00:00+09:00"
symbol: USD_JPY
enabled: false
market_regime:
  type: unclear
  confidence: 0.4
  reason: "1hがdown、6hがup、24hがflatで方向感が一致せず、ボラも低位で判断不能"
strategy:
  name: no_trade
entry:
  max_spread_pips: 0.5
  require_breakout: false
  direction: none
exit:
  take_profit_pips: 0
  stop_loss_pips: 0
  max_hold_minutes: 0
risk:
  quantity: 0
  max_open_positions: 1
  max_trades_in_this_window: 0
  max_loss_in_this_window_jpy: 0
no_trade:
  enabled: true
  reason: "trend_directionが1h/6h/24hで一致せず方向感がない"
next_advisor_run_in_minutes: 60
```

## 出力時の注意

- 必ず YAML プレーンテキストのみを stdout に出す。
- 先頭・末尾の空白行、説明文、Markdown フェンスは絶対に付けない。
- 数値はクォートで囲まない (`take_profit_pips: 30.0` であり `"30.0"` ではない)。
