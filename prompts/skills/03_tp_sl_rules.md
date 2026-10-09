# Skill: TP / SL / MaxHold ルール (デイトレ版)

> ⚠️⚠️ **現行の実値はこの行が最優先: TP30 / SL10・ratchet arm14/give8。**
> SL10 の根拠 = MAE 分析(勝ちトレードの大半は MAE<10 に収まり、フル SL に達した玉はほぼ戻らない)。
> 以下の TP50/SL25・arm30/give12 はデイトレ初期値(参考。上の行が優先)。
>
> ⚠️ **デイトレ版 — この冒頭ブロックが下記スキャル記述に優先する。**
> スキャル(TP8-20・小pips・回転重視)は撤退。**狙う値幅は数十pips(TP50/SL25・RR2が中心)**。
> 下の「lane 連動 tier / Adaptive TP テーブル / スキャ tier」等の **8〜20pips 系の数値は旧スキャル用で無効**。
> 値の指針はこのブロックに従うこと(下の詳細はトレーリング/early_exit の機構説明としてのみ読む)。
>
> **デイトレの土俵**:
> - 方向は summary_6h(主)+ 24h(大局)で決める。1hだけで決めない。6hに逆らう side は取らない。
> - TP は上位足の次の節目(6h/24h高安・ラウンドナンバー・前日終値)目安。既定 **TP50**、近ければ節目・遠ければ最大100。
> - SL は **25**(押し/戻りの構造の外。1hノイズに耐える)。RR ≈ 2.0。
> - MaxHold は最大 1440分(24h・日跨ぎ許容)。利確は固定TP(broker OCO)+ ratchet(arm30/give12)が伸ばしつつ吐き出し防止。
> - 5m/15m は入りのタイミングだけ。小pipsで利確しない。
>
> **デイトレ戦略別レンジ(下の hard_limits と一致)**:
>
> | strategy.name | take_profit_pips | stop_loss_pips | 使い分け |
> |---|---|---|---|
> | momentum_pullback | 30 〜 80 | 15 〜 40 | 上位足トレンドの押し目を数十pips取る |
> | breakout_follow | 40 〜 100 | 20 〜 45 | 上位足方向のブレイク追随(measured move) |
> | range_breakout_probe | 20 〜 50 | 15 〜 30 | デイトレでは原則使わない(節目間が広い時のみ) |
> | no_trade | 0(必ず) | 0(必ず) | エントリーしない |

---

(以下はスキャル用の詳細。**数値はデイトレ版では無効**。機構の挙動説明としてのみ参照する。)

選んだ strategy.name に応じて、`exit.take_profit_pips` と `exit.stop_loss_pips` の
許容範囲が決まっている。**この範囲を外すと Go 側 validator が config を reject する。**

## 戦略別レンジ (configs/hard_limits.yaml の strategy_limits と完全一致)

| strategy.name      | take_profit_pips | stop_loss_pips | 想定使い分け |
|--------------------|------------------|----------------|---|
| momentum_pullback  | 30.0 〜 80.0      | 15.0 〜 40.0    | 上位足トレンドの押し目(デイトレ) |
| breakout_follow    | 40.0 〜 100.0     | 20.0 〜 45.0    | 上位足方向のブレイク追随(デイトレ) |
| range_breakout_probe | 20.0 〜 50.0    | 15.0 〜 30.0    | デイトレでは原則 no_trade |
| no_trade           | 0 (必ず)         | 0 (必ず)        | エントリーしない |

## lane 連動 tier — まずこれで方針を決める

`market_regime.type` と選んだ strategy.name だけでなく、root が決める lane で tier を切り替える。
range/scalp/probe は「短く薄利を数こなす」、daytrade は「広い SL で耐えて 1 発を伸ばす」。
trend_up と正しく判定しても scalp tier で戦うと、通常の押しで SL を狩られ、勝ちは早期 exit で
薄利撤退する churn 負けになる。その対策。

| 項目 | **scalp/probe tier** | **daytrade trend tier** |
|---|---|---|
| TP | 8〜14 (回転重視) | **16〜20** (伸ばす余地) |
| SL | 6〜10 (タイト) | **13〜15** (通常の押し/戻りに耐える) |
| early_exit | **ON** (window 15 / target -2) | **OFF** (window 0 / target 0) ← 勝ちを早降りしない |
| ratchet | arm ≈ TP×0.6 | **主体。arm を RR floor 下限 (≈ TP×0.5〜0.55) まで下げて早めにトレール開始** |
| max_hold | 30〜60 分 | **90〜120 分** (+ extension で勝ち継続を待つ) |

**daytrade trend tier の要点**:
- early_exit を **OFF** にできるのは ratchet が ON のときだけ。daytrade trend tier は必ず ratchet ON。
- 「ratchet 前倒し」の限界: RR floor (`arm − give ≥ TP×0.5`) があるので **arm を TP×0.5 未満には置けない**
  (例 TP20 なら arm 12 / give 2 = locked 10 ≥ 10 ✓ が下限寄り)。これより早くは armできない。
- SL を広げる分、ダマシ時の 1 発損失は大きくなる。だから **daytrade trend tier を使うのは
  「confidence >= 0.70 かつ summary_24h.range_pips >= 80 (高ボラ帯)」の強トレンド時だけ**。
  弱い trend_up/down (conf < 0.70) や中低ボラ (range < 80) は scalp/probe tier に倒す。
  迷ったら scalp/probe tier か no_trade。

## lane 別の使い分け

| lane | strategy | TP | SL | MaxHold | early_exit | extension |
|---|---|---:|---:|---:|---|---|
| daytrade | momentum_pullback / breakout_follow | 16〜20 | 13〜15 | 90〜120 | 0 / 0 | 30〜45 / SL÷2 |
| scalp | momentum_pullback | 8〜12 | 6〜8 | 30〜45 | 10〜15 / -2 | 0 / 0 |
| probe | range_breakout_probe | 8〜14 | 6〜10 | 30〜45 | 10 / -2 | 0 / 0 |
| event | breakout_follow | 12〜16 | 8〜12 | 30〜60 | 0〜10 / -2〜0 | 0 / 0 |

`probe` は range 内で逆張りする戦略ではない。`range_breakout_probe` は `require_breakout=false` で
1h range 8〜24pips の上端/下端 2pips 以内だけ entry し、既に抜けた後は追わない。
これにより「レンジ抜けそうな時に小さく仕込み、逆なら SL。抜けるまで何度も試す」を実現する。

以下の個別ルール (Adaptive TP / SL / early_exit / ratchet) は、上の tier 方針の中で値を詰めるための詳細。

## TP の決め方の指針

- 同じ戦略でも、市況の `realized_volatility` が高いほど TP を大きめ、低いほど小さめにする
- 例: momentum_pullback でトレンド強 → TP 15〜20、トレンド弱 → TP 8〜12
- 例: breakout_follow でブレイク幅が大きい → TP 18〜20、控えめなら TP 12〜15
- 24h `range_pips` の 15〜30% を目安に。スキャ寄りなので欲張らず回転重視

### Adaptive TP テーブル (スキャ tier)

`summary_1h.realized_volatility` または 24h `range_pips` でボラ帯を判定し、TP を相場に合わせる:

| ボラ帯 | 判定 (24h range_pips) | momentum_pullback TP | breakout_follow TP | 備考 |
|---|---|---:|---:|---|
| 高ボラ | > 80 pips | **16〜20** | **18〜20** | 上限寄りで伸ばす |
| 中ボラ | 40〜80 pips | **12〜16** | **15〜18** | 標準スキャ TP |
| 低ボラ | < 40 pips | **8〜12** | **12〜15** | hard_limits 最低値に寄せる。届かないなら no_trade も検討 |

**狙い (スキャ寄り)**: TP を小さめ (8〜20) にして 1 日のトレード回数を増やす。spread0.5 に対し
TP の ~3〜6% なので回数を増やしてもコスト負けしにくい。TP<8 は spread 比率が悪化するため
hard_limits で下限 8 に固定。回数増は薄いエッジを増幅するので、勝率/RR の悪化には注意。

**Adaptive TP は上限として常に優先する**: daytrade trend tier の「TP16-20」も上の帯を超えない。
中ボラ(40-80)なら最大16、低ボラ(<40)なら最大12。さらに **TP は `summary_1h.range_pips × 1.3` を
超えない** こと — 当該保有時間 (30〜120分) で届かない TP は max_hold/SL で減衰するだけ。
TP16-20 を 1h range 5〜15pips の中低ボラ相場に置くと、**TP にほぼ届かない**
churn 負けになる。届く TP にできないほど 1h range が小さい dead market は `no_trade`。

## ② RR 対称性 + ratchet 到達性 (最重要)

含み益の peak が ratchet arm に一度も届かない相場では、勝ちは early_exit の薄利・負けは SL フルとなり、
実効 RR が大きく崩れた churn 負けになる。
「利確機構 (ratchet/TP) は届かない場所にあるのに SL だけフル」の非対称が原因。これを防ぐ:

- **ratchet arm を届く値に置く**: `arm` (= TP×0.6〜0.65) が `summary_1h.range_pips` を超えるなら
  ratchet は arm しない。arm > 1h range なら TP を下げて arm を 1h range 内に収める。収まらなければ `no_trade`。
- **SL は到達可能な利益で割り戻して RR を確保**: 到達可能利益 ≒ arm。`arm / SL >= 1.2` を満たすこと。
  満たさない (= SL が arm に対し大きすぎ非対称) なら SL を arm×0.8 程度まで下げる。戦略レンジ下限でも
  無理なら `no_trade`。「上は arm 頭打ち・下は SL フル」を作らない。
- **early_exit は損切り側の救済**であって勝ちの利確口ではない (詳細は下の early_exit セクション)。

## SL の決め方の指針

- **6〜15 pips** が基本。スキャ tier の短時間保有 (30〜120 分) に合わせた幅
- スプレッドは平常 ≤1.0 pips を目安。**明確なチャンス時は `entry.max_spread_pips` を 1.5 まで
  許容してよい** (hard_limits 上限も 1.5)。ただし spread1.5 はコスト比が悪化するので
  skill 07 の friction を満たす TP (目安 TP>=12) のときだけ。根拠が薄い・低ボラなら従来通り見送り。
  異常拡大 (普段の 2 倍以上、東京早朝 ~10pips 等) は root の相対ガードで no_trade。
- リスクリワード比 (TP/SL) は **1.2 以上** を目標。1.0 を切るなら no_trade

## max_hold_minutes

- すべての戦略で **30〜120 分** (スキャ寄り、回転重視)
- momentum_pullback: 30〜90 分が目安 (押し目から短時間で TP)
- breakout_follow: 30〜120 分 (ブレイク初動の伸びを短時間で取る)
- range_breakout_probe: 30〜45 分 (抜け前の試行なので長く粘らない)
- no_trade: `0`

**スキャ tier の利点**: 保有が短いので経済指標発表帯を跨ぐ確率が下がり、長時間ホールド事故も縮小。

### extension_max_minutes (daytrade trend tier の勝ち継続)

daytrade trend tier では `extension_max_minutes` を 30〜60、`extension_unrealized_pips_threshold` を
SL の 1/2 程度に設定する。非対称な延長ルールとして、**soft deadline 後も勝ち/フラットの間は延長**して
トレンドの続伸を待ち、負けが -threshold を超えたときだけ損切り close する (利確側は ratchet が担当)。
scalp/probe/event tier では 0 (無効) でよい。これが「保有時間を当初より伸ばす」の実装。

## TP > SL の関係 (推奨)

リスクリワード比 (TP/SL) が 1.0 を切ると統計的に勝てない。
**可能なら TP/SL >= 1.2〜1.5 を目指す** (例: TP 15 / SL 10 → RR 1.5)。
ただし戦略別レンジの上限を超えてまで TP を伸ばすことはしない。

## early_exit_window_minutes / early_exit_target_pips (MaxHold 早期 exit)

MaxHold (例: 45〜90 分) が経過した瞬間に MARKET 強制 close すると、最悪のタイミングで
損失を確定させる事故がありうる。これを和らげるため、
**MaxHold ソフト deadline の手前で「マイナスが浅い瞬間」を捕まえて早期 close する** policy
を提供する。

挙動 (`ManageOpenPositions.evaluateExit` 内):
- 残り時間 ≤ `early_exit_window_minutes` の窓に入ったら毎 tick で PnL を判定
- `unrealized_pips ≥ early_exit_target_pips` を満たした最初の tick で close (close_reason=`early_exit`)
- 窓内で一度も満たさなければ既存の MaxHold 動作 (soft → extension → hard、close_reason=`max_hold`) のまま
- `early_exit_window_minutes: 0` で feature OFF。**early_exit OFF は
  ratchet が ON のときだけ許可** (early_exit / ratchet の少なくとも一方で最悪損失を緩和)。
  scalp/probe/event tier は early_exit ON、daytrade trend tier は early_exit OFF + ratchet 主体。両方 OFF は reject。

### 推奨デフォルト値 (②: 損切り救済に限定し、勝ちを刈らない)

early_exit は窓の **最初** に `unrealized ≥ target` を満たした tick で発火する。窓を広く取ると
(例: 35 分 hold で window 15 → 分 20 で発火) target -2 ではほぼ毎回 window 頭で close し、
ratchet (利確側) に届く前に**勝ちポジションまで薄利で刈ってしまう**。
そこで **window は MaxHold の 1/4 程度に短く**し、deadline 直前の救済だけに使う。target は breakeven 以下。

| 状況 | early_exit_window_minutes | early_exit_target_pips |
|---|---:|---:|
| 通常 | **round(MaxHold×0.25)** (例 35→9, 45→11) | **-2** |
| ボラ低めで TP 到達期待薄 | round(MaxHold×0.33) | -3 |
| ボラ高めで TP 到達ありそう | round(MaxHold×0.2) | -2 |
| 機能 OFF (daytrade trend tier) | 0 | (無視される) |

**役割**: early_exit = 損切り側の救済 (deadline で最悪値を踏まないため)。利確側は ratchet が担当。
勝ちを伸ばすのは ratchet/TP であって early_exit ではない。

※ MaxHold が短い (例 30 分) ときは window をさらに小さく (例 10)。**window ≤ max_hold_minutes 必須**。

### ガイドライン

- `early_exit_target_pips` は **マイナスでも OK** (むしろ通常はマイナス)。「最悪 -X pips で諦める」上限
- ただし負けすぎを許容する設計ではない → 通常 -5 pips 以上はマイナスにしない (= -5 〜 +5 が実用域)
- `early_exit_window_minutes` は **max_hold_minutes 以下** 必須 (Validator が enforce)
- 24 時間相場が安定してそうな日は window 大きめ (45 min)、急変リスクある日 (経済指標前) は window 小さめ (15 min)
- breakout_follow と momentum_pullback で同じデフォルト値を使って OK (戦略差を出すのは backtest で edge 確認後)

## ratchet_arm_pips / ratchet_giveback_pips (trailing take-profit)

未実現損益の peak を OnTick で追跡し、peak が `ratchet_arm_pips` に達して以降、
peak から `ratchet_giveback_pips` だけ戻ったら **MARKET close で利確**する。
「伸びている間は持ち、勢いが死んだら確定」を機械化する政策。

挙動 (`ManageOpenPositions.evaluateExit` / `OnTick`):
- 毎 tick で BUY なら bid、SELL なら ask を使って unrealized_pips を算出
- `unrealized_pips > 既存 peak` なら peak を更新 (DB に UpdatePositionRatchetState)
- `peak >= ratchet_arm_pips` で armed フラグ ON (一度 ON になったら OFF にならない)
- armed && `peak - 現 unrealized_pips >= ratchet_giveback_pips` で close_reason=`ratchet_takeprofit`
- TP (`take_profit_pips`) より先に発火する (ratchet は exit-side 価格基準、TP より早く触れる)
- **Live でも有効** (TP/SL は GMO OCO に任せるが ratchet は bot OnTick で判定 → cancel→MARKET)
- `0 / 0` で feature OFF (= 既存挙動)

### 推奨デフォルト値 (RR floor 準拠)

**重要**: ratchet は「TP の手前で利を確保するトレーリング」であって「薄利で即利確する仕組み」ではない。
arm を小さく置く (例 arm=5/give=3) と、勝ちは +2〜4pips 程度・負けは SL フルの
**逆RR** になる。これを潰すため、**確保利益 `locked = arm − giveback` は
`take_profit_pips × 0.5` 以上**でないと Go validator が reject する (RR floor)。
= 「ratchet で利確するなら最低でも TP の半分は確保してから」。

arm は **scalp/probe/event tier では TP の 0.6〜0.7 倍**、**daytrade trend tier では RR floor 下限 (≈ TP×0.5〜0.55) まで
下げてよい** (早めにトレール開始して伸びを拾う)。giveback は **arm の 15〜25%** に抑える (確保利益を削らない)。
※ RR floor (`arm − give ≥ TP×0.5`) があるので arm を TP×0.5 未満には置けない (reject される)。

| TP 帯 | ratchet_arm_pips | ratchet_giveback_pips | locked (arm−give) | floor (TP×0.5) |
|---|---:|---:|---:|---:|
| TP 10 (低ボラ) | **7** | **2** | 5 | 5 ✓ |
| TP 15 (中ボラ) | **10** | **2** | 8 | 7.5 ✓ |
| TP 20 (高ボラ) | **13** | **3** | 10 | 10 ✓ |
| TP 50 (高ボラ breakout) | **33** | **6** | 27 | 25 ✓ |
| 機能 OFF (no_trade のみ) | 0 | 0 | — | — |

- トレンドが強く大きく伸ばしたいときは arm を TP の 0.7× まで上げ、giveback を arm の 15% 程度に。
- **arm を TP の半分未満に置く旧設計 (arm=5/give=3 等) は廃止**。逆RR の温床。

### Hard limits (Validator が enforce)

- 0 / 0 (OFF) または arm/give 両方 > 0 のいずれか。片方だけ > 0 は **rejected**
- **enabled config では 0/0 (OFF) も禁止 — Go validator が reject する。
  no_trade のみ 0/0 が可。**
- **RR floor: `arm − giveback ≥ take_profit_pips × 0.5` 必須。割ると reject。** 逆RR 防止。
- `arm >= 3`, `give >= 1`, `arm > give` (= 同値や逆転は禁止)
- `arm <= 50`, `give <= 50` (sanity cap; 50 pips 超は TP の領域)

### ガイドライン

- **TP の手前**で確定させる設計。TP=30 でも peak 25 → ratchet 22 で確定がよくある (それで OK)
- arm は **TP の 0.6〜0.7×** に置く。小さすぎると逆RR (RR floor 違反で reject)、
  TP に近すぎると ratchet 発火前に TP / MaxHold へ到達してしまう
- give は **arm の 15〜25%** を目安。arm=20 なら give=3-5 (確保利益 arm-give を削らない)
- momentum_pullback / breakout_follow / range_breakout_probe で異なる値を使って良い (例: breakout は伸びるので arm 大きめ)
- no_trade のときは必ず 0 / 0
- `early_exit_window_minutes` (MaxHold 早期 exit) と併用可能。役割が違う:
  早期 exit は **損切り側**の最悪化抑止、ratchet は **利確側**の機会取り
