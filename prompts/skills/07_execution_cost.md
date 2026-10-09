# Skill: 実行コスト監査 (execution cost audit)

選んだ `take_profit_pips` / `stop_loss_pips` が **コスト (スプレッド + スリッページ + 手数料 + 税)**
を控除しても期待値プラスかを最終確認する。skill 03 が TP/SL の **絶対値範囲** を決め、
この skill が **コスト比** で「割に合うか」を見る。

`generate_strategy_config.md` の統合フローで skill 05 (output_format) の **直前** に
適用する。コスト割れの設計が出た場合は `no_trade` に降格させる。

## コスト構成 (USD/JPY 1000通貨 = 0.1 lot 想定)

| 要素 | 1 trade あたりの目安 (1000通貨) | ソース |
|---|---|---|
| **スプレッド** | 0.2-0.5 pips ≒ 2-5 円 | GMO USD/JPY 平常時 0.2銭 / イベント帯 0.5-1.0銭 |
| **スリッページ** | 0.5 pips ≒ 5 円 | `hard_limits.yaml` の `paper.simulated_slippage_pips` |
| **API 手数料** | 約定金額 × 0.002% / 片道 (往復 ≈ 6〜8 円) | GMO 外為 FX の API 手数料 (paper でも close 時に推定計上) |
| **税 (申告分離)** | 利益 × 20.315% | FX デイトレ。損失年は 0 |

**ラウンドトリップ (入退場 2 回)**:
- フリクション = (spread + slippage) × 2 pips
- 通常時: (0.2 + 0.5) × 2 = **1.4 pips** が「最低限稼がないと損益ゼロ」のライン

## 監査チェック (順に判定、1 つでも fail なら no_trade 降格)

1. **TP > 損益分岐 pips の 3 倍以上**:
   - 必要: `take_profit_pips >= (spread + slippage) * 2 * 3`
   - 例: 通常時 TP >= 1.4 × 3 = 4.2 pips。scalp TP 8-12 / daytrade TP 16-20 はコスト条件を満たす
   - イベント帯 (spread 1.0pips × 2 × 3 = 6 pips) でも TP 20pips は OK
   - **spread 1.5 を許容するチャンス時: friction = (1.5+0.5)×2 = 4 → TP >= 12 が必須。
     よって spread1.5 の entry は TP>=12 (daytrade/probe 帯) のみ成立し、scalp の TP8-10 は不可。**
   - 失敗 → TP 不足、上げるか no_trade

2. **TP/SL リスクリワード比 >= 1.3**:
   - 必要: `take_profit_pips / stop_loss_pips >= 1.3`
   - 例: TP 30 / SL 20 = 1.5 ✓ / TP 25 / SL 25 = 1.0 ✗ → no_trade
   - 1.3 を下回ると勝率 55% でも長期で負ける

3. **期待値の概算が pip ベースでプラス**:
   - 仮想勝率を 50% として: `EV = 0.5 * TP - 0.5 * SL - friction*2`
   - 通常時 (friction=1.4pips RT): EV = 0.5×30 - 0.5×20 - 1.4 = 3.6 pips ✓
   - 例: TP 20 / SL 18 / friction 1.4 → EV = 0.6 pips。ギリギリ → 慎重 (no_trade 推奨)

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
  slippage_pips: <hard_limits.paper.simulated_slippage_pips>
  friction_rt_pips: <(spread + slippage) * 2>
  rr_ratio: <TP / SL>
  expected_value_pips: <0.5*TP - 0.5*SL - friction*2>
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

- ❌ TP < friction RT (例: TP 1.0pip で friction 1.4pip RT) → ラウンドトリップで必ず負ける
- ❌ RR < 1.0 (例: TP 10 / SL 20) → 勝率 67% 以上必要、現実的でない
- ❌ 税を gross の 100% で計算 → 損失通算を無視している
- ❌ スプレッド異常時 (event 帯で 1.5pips など) でも friction 計算を平常値で → 過大評価

## サブエージェント呼び出しは不要

skill 07 は 4 つの check を数値で判定するだけ。メインが skill 05 出力前に直接適用する。
