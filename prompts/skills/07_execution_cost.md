# Skill: 実行コスト監査 (execution cost audit)

選んだ `take_profit_pips` / `stop_loss_pips` が **コスト (スプレッド + スリッページ + 手数料 + 税)**
を控除しても期待値プラスかを最終確認する。skill 03 が TP/SL の **絶対値範囲** を決め、
この skill が **コスト比** で「割に合うか」を見る。

`generate_strategy_config.md` の統合フローで skill 05 (output_format) の **直前** に
適用する。コスト割れの設計が出た場合は `no_trade` に降格させる。

## コスト構成 (USD/JPY 1000通貨 = 0.1 lot 想定)

| 要素 | 1 trade あたりの目安 (1000通貨) | ソース |
|---|---|---|
| **スプレッド** | 平常 ≈0.5 pips ≒ 5 円 | API 経由の平常値。イベント帯・東京早朝は拡大する (Input JSON の現在値を使う) |
| **スリッページ** | 0.5 pips ≒ 5 円 | 計画用の保守的な仮定。paper の `simulated_slippage_pips`=0.0 とは別 |
| **API 手数料** | 約定金額 × 0.002% / 片道 (往復 ≈ 6〜8 円 ≒ 0.6 pips) | GMO 外為 FX の API 手数料 (paper でも close 時に推定計上) |
| **税 (申告分離)** | 利益 × 20.315% | FX デイトレ。損失年は 0 |

**ラウンドトリップ (入退場 2 回)**:
- フリクション friction_rt = (spread + slippage) × 2 pips
- 通常時: (0.5 + 0.5) × 2 = **2.0 pips**
- 実際の往復コスト床は ≈1.1 pips (手数料 ≈0.6 + スプレッド ≈0.5)。上の式は spread を 2 回数え、
  仮定スリッページも含むので、この床より保守的 (手数料はこの余裕に含まれるとみなし、別に足さない)

## 監査チェック (順に判定、1 つでも fail なら no_trade 降格)

1. **TP > 損益分岐 pips の 3 倍以上**:
   - 必要: `take_profit_pips >= (spread + slippage) * 2 * 3`
   - 例: 通常時 TP >= 2.0 × 3 = 6.0 pips。scalp TP 8-12 / daytrade TP 16-20 はコスト条件を満たす
   - イベント帯 (spread 1.0pips: (1.0+0.5) × 2 × 3 = 9 pips) でも TP 20pips は OK
   - **spread を広げるときは friction=(spread+slip)×2 で TP>=3×friction を再計算**。
     例: spread 1.5 → friction (1.5+0.5)×2 = 4 → TP >= 12 が必須 (scalp の TP8-10 は不可)。
     spread 3.0 (hard_limits 上限) → friction 7 → TP >= 21 が必須。
   - 失敗 → TP 不足、上げるか no_trade

2. **TP/SL リスクリワード比 >= 1.3**:
   - 必要: `take_profit_pips / stop_loss_pips >= 1.3`
   - 例: TP 30 / SL 20 = 1.5 ✓ / TP 25 / SL 25 = 1.0 ✗ → no_trade
   - 1.3 を下回ると勝率 55% でも長期で負ける

3. **期待値の概算が pip ベースでプラス**:
   - 仮想勝率を 50% として: `EV = 0.5 * TP - 0.5 * SL - friction_rt`
   - 通常時 (friction_rt=2.0pips): TP 12 / SL 6 → EV = 6 - 3 - 2.0 = 1.0 pips ✓
   - 例: TP 16 / SL 12 / friction_rt 2.0 → EV = 8 - 6 - 2.0 = 0 pips。RR 1.33 は通っても EV > 0 を満たさない → no_trade

4. **税考慮 (年間ベース、参考のみ)**:
   - gross PnL から 20.315% 引く
   - 月 +10,000 JPY → 手取り ≒ 7,968 JPY
   - **個別 trade 単位では税を引かない** (損失通算があるため)
   - 「年間ターゲットを 1.25 倍で見る」を運用ルールに

## 出力 (skill 単独の判定結果)

skill 05 の YAML 直前に、main が以下のサブブロックを内部判断する:

```
execution_cost_check:
  spread_pips: <現在の spread>
  slippage_pips: 0.5 (固定の仮定)
  friction_rt_pips: <(spread + slippage) * 2>
  rr_ratio: <TP / SL>
  expected_value_pips: <0.5*TP - 0.5*SL - friction_rt_pips>
  verdict: pass | downgrade_to_no_trade
  reason_jp: <数値引用付き 100 文字以内>
```

verdict が `downgrade_to_no_trade` なら、それ以降の出力 (skill 05) は `no_trade`
スキーマで書く。

## 既存 skill との分担

- **skill 03 (tp_sl_rules)**: TP/SL の **絶対値範囲** (戦略別 hard_limits)
- **skill 04 (risk_rules)**: スプレッドの**絶対値 cap** (`max_spread_pips` 超で no_trade)
- **skill 07 (これ)**: TP/SL/スプレッドの **比率** がコスト構造的に成立するか

→ 03/04 は「数値レンジの上下限」、07 は「期待値の正負」を見る。重複しない。

## アンチパターン

- ❌ TP < friction RT (例: TP 1.0pip で friction 2.0pip RT) → ラウンドトリップで必ず負ける
- ❌ RR < 1.0 (例: TP 10 / SL 20) → 勝率 67% 以上必要、現実的でない
- ❌ 税を gross の 100% で計算 → 損失通算を無視している
- ❌ スプレッド異常時 (event 帯で 1.5pips など) でも friction 計算を平常値で → 過大評価

## サブエージェント呼び出しは不要

skill 07 は 4 つの check を数値で判定するだけ。メインが skill 05 出力前に直接適用する。
