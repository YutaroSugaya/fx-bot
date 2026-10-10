# Skill: パフォーマンスレビュー (weekly / monthly performance audit)

`generate_strategy_config.md` の hourly フローからは **呼ばれない**。
週次 / 月次の独立スクリプトから手動 (or cron 経由) で呼び出して、過去 N 日の
取引履歴を集計してレポート出力する。`analyze_trading_logs.md` と並列の役割。

## 入力 (期待するもの)

呼び出し側 (人間 or shell script) が以下を渡す:
- 期間: `from` / `to` (ISO8601, JST)
- `trades` テーブルの該当期間レコード (JSON or CSV)
- `signal_rejections` テーブルの該当期間 (理由別 count)
- `strategy_configs` テーブルの該当期間 (config_id → strategy_name の map)

## 出力する 6 セクション (markdown、200-400 行)

### 1. 期間サマリ

| Metric | 値 | 評価 |
|---|---|---|
| Sample size | <total trades> | <50 未満なら ⚠️ サンプル不足、200 以上で意味あり> |
| Total PnL (gross) | <JPY> | 符号と桁 |
| Total PnL (税後 = ×0.797) | <JPY> | 確定申告ベース |
| Profit Factor | <sum_wins / sum_losses_abs> | <1.3 以上 ✓ / 1.1-1.3 微妙 / <1.0 ✗> |
| Win Rate | <winners/total> | <%> |
| Expectancy per trade | <gross PnL / sample> | JPY/trade |
| Max Drawdown | <peak-trough JPY> | <資金比 10% 未満 ✓> |
| Avg Win / Avg Loss | <win avg> / <loss abs avg> | RR ratio = avg_win/avg_loss |

### 2. 戦略別内訳

| Strategy | trades | PF | Win Rate | Expectancy | 評価 |
|---|---|---|---|---|---|
| momentum_pullback | <n> | <PF> | <%> | <JPY> | <↑> |
| breakout_follow | <n> | <PF> | <%> | <JPY> | <↑> |
| no_trade (発火数) | <n> | — | — | — | reject されずに済んだ件数 |

**判定**: 戦略間で PF が 0.3 以上開いていれば不調戦略を disable 候補に。

### 3. 時間帯別内訳 (JST)

| 時間帯 | trades | PF | Win Rate | 備考 |
|---|---|---|---|---|
| 東京オープン 08:30-10:00 | <n> | <PF> | <%> | 仲値前後 |
| 東京昼 10:00-15:00 | <n> | <PF> | <%> | |
| ロンドンオープン 16:00-17:30 | <n> | <PF> | <%> | 欧州勢 |
| NY オープン 22:30-24:00 | <n> | <PF> | <%> | 米経済指標多 |
| 早朝 03:00-07:00 | <n> | <PF> | <%> | 流動性低、要注意 |

**判定**: 特定帯だけ PF<1.0 なら、そこを skill 06 で `no_trade` 強制に。

### 4. Close reason 内訳

| Reason | count | 平均 pips | 平均 JPY |
|---|---|---|---|
| take_profit | <n> | <+> | <+> |
| stop_loss | <n> | <-> | <-> |
| max_hold | <n> | <±> | <±> |

上の 3 つ以外の close_reason (early_exit / ratchet_takeprofit / ratchet_stoploss / session_flatten /
broker_close / manual / reconcile_cold_close) も件数があれば行を足す。

**判定**:
- TP 比率が 30% 未満 → エントリーの質が悪い (= 戦略の edge 不足)
- max_hold 比率が 40% 超 → TP 設定が遠すぎる (skill 03 で TP 下げ検討)
- SL 比率 > 50% → リスクテイクが過剰 (skill 04 で max_consecutive_losses 強化)

### 5. Reject 上位理由

```
最頻 reject 上位 5 件:
1. <reason>: <count> 件 — <提案する prompt 改善 1 行>
2. ...
```

代表的な reject reason と対応:
- `cooldown after_loss until ...`: 連敗後の cooldown 発火 → 想定通り、変更不要
- `spread X > cap Y`: スプレッド拡大帯の見送り → 基本は想定通り。cap (`entry.max_spread_pips`) を広げるなら hard_limits.max_spread_pips (0.3〜3.0) の範囲内で、skill 07 の TP>=3×friction を満たす場合だけ検討
- `consecutive_losses N >= cap M`: 連敗ストップ発火 → 想定通り
- `cooldown after_take_profit until ...`: TP 直後の再エントリー試行が多すぎる → max_trades_in_window 下げ
- `direction_buy_only_blocks_short`: Claude が config と逆方向 signal を出した → prompt 改善
- `unregistered_strategy: X`: skill 02 が未知名出した → semantic validation で reject されているはず確認

### 6. 改善提案 (次週への持ち越し)

具体的に prompt or config の **1 ヶ所だけ** 変更を提案 (1 週間 1 変更ルール):

```
推奨改善: <ファイル名> の <セクション>
理由: <データ引用>
期待効果: <連敗率を下げる / win rate を上げる ではなく、コスト控除後 PF を上げる>
副作用: <あれば>
測定方法: <次週レビューで PF / MDD 変化を確認>
```

## 禁止事項

- 「もっとロットを上げれば利益が出る」系の提案は **禁止**
- 「過去結果のチェリーピック」(= 上位 3 件だけ見て改善案を作る) は禁止
- サンプル数 < 30 で確定的な判断を下さない (必ず「サンプル不足」と注記)

## 推奨実行頻度

- **週次** (毎週日曜): 直近 7 日。reject 上位の prompt 改善 1 件を抽出
- **月次** (月初): 直近 30 日。戦略別 / 時間帯別の集計で disable 候補を出す
- **四半期**: Walk Forward Test と組み合わせて、戦略の継続/書き直し判断

## アンチパターン

- ❌ 期間が短すぎる (<7 日) のに PF を絶対視 → ノイズ
- ❌ Win Rate だけ見て PF / Expectancy を無視 → 大負け 1 回で台無し
- ❌ 時間帯別集計で 1 セグメント当たりサンプル < 10 で disable 判断 → 偶然
