# Skill: 戦略選択 (strategy.name) — lane 分離版

市況分類 (skill 01) の結果と入力 JSON から、4 戦略のうち 1 つを選ぶ。
**range_reversion は廃止済み**。レンジ相場で逆張りはしない。
ただし range / 弱 trend でも、レンジ端で抜け前に小さく入る `range_breakout_probe` は probe lane として使う。

> **③ range_breakout_probe の適用範囲 (最重要)**
> `range_breakout_probe` は **range / 圧縮 (unclear で低 vol) かつ 1h range 8〜24pip の明確なレンジ端**専用。
> **6h/24h が明確な trend_up / trend_down のときは probe を chosen にしない** — トレンドでは
> `momentum_pullback` (押し目/戻り) か `breakout_follow` を選ぶ。trend 地合いで probe を選ぶと、
> 押し戻りのチョップに飲まれて churn 負けしやすい。トレンドで他に確信が持てなければ `no_trade`。

## lane の前提

| lane | 戦略の意味 | 使う戦略 |
|---|---|---|
| `daytrade` | 1 ポジションを 6h/24h の明確な方向で伸ばす | momentum_pullback / breakout_follow |
| `scalp` | 15m/1h の短い値幅で小さく何度も試す | momentum_pullback |
| `event` | 経済イベントの初動だけ短時間で取る | breakout_follow |
| `probe` | レンジ端・圧縮後に抜け前で小さく試す | range_breakout_probe |

## 選択フロー (上から順に判定)

1. **emergency / unsafe** な状況なら必ず `no_trade`
   - bot_state.emergency_stop = true
   - bot_state.consecutive_losses >= 設定値
   - bot_state.daily_pnl_jpy が当日上限損失に達している
   - current_rate.spread_pips > hard_limits.max_spread_pips
   - 市況 `unclear` または `volatile`。ただし `event_context.policy=breakout` かつ spread 正常なら event lane の `breakout_follow` は検討可

2. **trend_up / trend_down で勢いが極めて強い** → `breakout_follow` (daytrade/event)
   - 6h/24h の trend_direction が一致 (1h は許容)
   - **1h `range_pips` > 24h `range_pips` × 50%** (= 1 時間で日中レンジの半分を消化する強さ)
     - 勢いが弱いと require_breakout=true + 短 TTL では空振りしやすいため、閾値は 50%。
       条件未達なら 3 (momentum_pullback) に倒す。
   - require_breakout = true で運用
   - direction は trend に合わせる (trend_up → buy_only, trend_down → sell_only)

3. **trend_up / trend_down で押し目/戻りが期待できる** → `momentum_pullback`
   - 6h/24h で方向が一致しているが、直近 1h で反対方向の戻り (押し目) が入った、または
     1h `range_pips` が 24h `range_pips` の 50% 以下に収束 (= 押し目整理のサイン)
   - **6h up / down が出ていて、1h が低ボラ整理 (上記レンジ収束) なら momentum_pullback
     を必ず採用する** (no_trade に倒さない)。デイトレで最も EV が高い局面。
   - direction は基本トレンド方向のみ (trend_up → buy_only / trend_down → sell_only)

4. **range 相場** → daytrade は `no_trade`、probe は `range_breakout_probe`
   - 6h/24h ともに trend_direction が `flat` の場合
   - 1h range_pips が 8〜24pips、current_rate が 1h support/resistance 近辺、
     spread が正常なら `range_breakout_probe` を **probe lane** として推す。
   - `require_breakout=false`。Go 側は 1h range 8〜24pips かつ上端/下端 2pips 以内で entry し、
     既に抜けた後は追わない。これは「抜け前に小さく仕込み、逆なら SL、抜けるまで何度も試す」用途。
   - 値幅が 8pips 未満、support/resistance が曖昧、spread が広い場合は `no_trade`

5. **event_context.policy=breakout** → `breakout_follow` (event lane)
   - freeze ではなく breakout policy のイベントだけ。
   - direction は直近 15m/1h の強い方向。方向が読めなければ `both` で上下どちらのブレイクも待てる。
     ただし spread が広い、値幅不足、イベントが freeze policy の場合は `no_trade`。
   - `require_breakout=true`、短い TP/SL、10分 recheck を前提にする。

6. **どれにも自信が持てない** → `no_trade`

## direction の決め方

| strategy | direction の選択肢 |
|---|---|
| momentum_pullback | `buy_only` or `sell_only` (トレンド方向のみ) |
| breakout_follow | `buy_only` or `sell_only` or `both` (event は both で上下ブレイク待ち可) |
| range_breakout_probe | `buy_only` or `sell_only` or `both` (range 端の上下どちらも試すなら both) |
| no_trade | `none` |

`trend_up` + `sell_only`、`trend_down` + `buy_only` のような矛盾は禁止。Validator が reject する。

## require_breakout

| strategy | require_breakout |
|---|---|
| momentum_pullback | `false` |
| breakout_follow | `true` |
| range_breakout_probe | `false` |
| no_trade | `false` |

## max_chase_pips / chase_lookback_candles (追いかけ防止 — momentum_pullback 専用)

trend_up で正しく momentum_pullback を選んでも、急騰が走り切った後 (直近 60 分で大きく上昇した後)
の天井で買うと、即 SL / 早期撤退で churn 負けしやすい。
これを防ぐため momentum_pullback では **「もう走り切った後の順張り」を見送る**フィルタを設定する。

- **momentum_pullback**: `max_chase_pips: 12` / `chase_lookback_candles: 12` (=直近60分) を推奨デフォルト。
  - ボラが高い日は max_chase をやや広め (15)、低い日は狭め (8〜10)。
  - 直近 N 本の安値(買い)/高値(売り)ベースから entry がこの pips 以上離れていたら entry を見送る。
  - **狙い**: 初動や浅い押し目だけ拾い、天井追い・底追いを構造的に禁止する。
- **breakout_follow / range_breakout_probe**: **設定しない (0 / 省略)**。breakout は仕様上レンジ拡張方向、
  probe はレンジ端の近接条件で入る戦略なので
  追いかけ防止と矛盾する。初動ブレイクはむしろ歓迎。
- **no_trade**: 省略。

※ 一方通行で押し目が来ない強トレンドはこのフィルタで取り逃すが、その初動は breakout_follow /
イベント攻めモード (event_calendar policy:breakout) 側で取る役割分担。

## 確信度の閾値 (データ蓄積フェーズ)

- 確信度 **55% 以上** の戦略があれば実行
- 確信度 **35-55%** でも emergency (Step 1 該当) でなければ momentum_pullback で push。デイトレは中長期トレンドに乗る価値が高い
- 確信度 **< 35%** または emergency (Step 1) のみ no_trade

過剰売買は避けるが、**取引機会ゼロの方が長期 EV はマイナス** (データが溜まらない / 戦略改善できない / 撤退判断もできない)。
現フェーズの目標は「PF を上げる」より「サンプル数を増やして失敗パターンを学習する」こと。
ただし daytrade と scalp/probe を混ぜない。弱い trend で TP16〜20 / SL13〜15 の
daytrade パラメータを使うのは避け、scalp/probe lane に落として短く試す。

## allowed_hours_jst (時間帯フィルタ)

**データ蓄積フェーズ: 原則 `allowed_hours_jst: []` (= 全時間帯許可) を デフォルトとする。**

### 過去の知見 (現フェーズではデフォルトに採用しない)

過去の backtest で以下が判明:
- 3 ヶ月 (USD/JPY 1m) で momentum_pullback / breakout_follow ともに **PF<1.0** で net 負け
- **時間帯別 PF はフィルタ変更で大きく反転する** (例: momentum_pullback の 08 JST が無フィルタで +117 JPY/trade → hour filter 入れると -1 JPY/trade)
- 原因: `max_open_positions=1` による position state coupling = selection bias。「ある時間帯の trade が良かった」のは「直前まで position を持ってたから良いタイミングだけ拾えた」だけ

→ **時間帯フィルタで trade を絞るより、全時間帯で trade して per-trade の結果を直接観察する方が学習効率が高い**。

### 設定ガイドライン

- **momentum_pullback / breakout_follow / range_breakout_probe ともに `allowed_hours_jst: []` をデフォルト**
- 例外: 流動性が極端に低い時間帯や、ユーザー側で明確に判断材料があるときのみ配列で除外
- 除外する場合は **必ず `market_regime.reason` に理由を書く** (例: "04-05 JST はオセアニア時間で spread 拡大しがちなため除外")
- **no_trade**: フィールド省略 OK
