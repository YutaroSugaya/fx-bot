# Prompts — Claude Skill 集と出力契約

> **本書 §1〜§7 は advisor v1(`ai_advisor.enabled`、既定 off)のプロンプトと出力契約。**
> 自律 LLM 判断ループ(`llm_decision.enabled`、既定 off)のプロンプトは本書でなく次にある:
> - 発注判断 = [backend/internal/adapter/advisor/llm_decision_cli.go](../../backend/internal/adapter/advisor/llm_decision_cli.go)
>   (既定の単一 agent モードは `BuildSingleAgentDecisionPayload`・Go 内テンプレート + playbook `runtime/playbook_*.jsonl`
>   + MarketSummary JSON。`--tools Read` で起動するため subagent は使わない)
> - 反省 = [reflection_cli.go](../../backend/internal/adapter/advisor/reflection_cli.go) + `.claude/agents/reflection-{regime,risk,strategy}.md`
> - 出力パーサ = [llm_decision_parse.go](../../backend/internal/adapter/advisor/llm_decision_parse.go)
> - 監査ログ = `runtime/logs/llm_decisions.jsonl`
>
> どの経路がどの subagent を使うかは [SUBAGENTS.md](../architecture/SUBAGENTS.md)。

Claude advisor が `strategy_config.yaml` を生成するときに使う prompt 群の正本ドキュメント。

実プロンプトは [prompts/](../../prompts/) ディレクトリにある。

---

## 1. メインプロンプト

[prompts/generate_strategy_config.md](../../prompts/generate_strategy_config.md):

- bot から渡される `latest_summary.json` を入力に取る
- root は 4 つの subagent (`risk-auditor` / `regime-classifier` / `strategy-selector` / `tpsl-designer` = skill 01〜04) を Task ツールで並列起動し、その結果を統合したあと、自分で skill 07 (コスト監査) → 05 (出力スキーマ) → 06 (再評価間隔) を Read して最終 YAML を組む (Claude 内部での並列、claude_cli.go から見れば single subprocess)
- skill 08 / 09 は advisor の毎回のフローからは呼ばれない (手動で使う)
- 最終出力は YAML 1 本 (= 1 つの `strategy_config`)

### スタンドアロンプロンプト (advisor cycle 外部、運用補助用)

| ファイル | 役割 |
|---|---|
| [prompts/analyze_trading_logs.md](../../prompts/analyze_trading_logs.md) | 過去 N 日の trades + signal_rejections を渡して日次/週次サマリを生成 |
| [prompts/improve_strategy_rules.md](../../prompts/improve_strategy_rules.md) | reject 上位理由から `prompts/skills/*` の改善提案を出す |
| [prompts/review_rejected_config.md](../../prompts/review_rejected_config.md) | validator が reject した config を Claude に説明させる (debug 用) |

これらは scheduler から自動で呼ばれない。手動 `claude -p < prompts/xxx.md` で運用補助に使う。

---

## 2. Skill 一覧

[prompts/skills/](../../prompts/skills/) 配下。

| # | ファイル | 役割 |
|---|---|---|
| 01 | `01_market_regime.md` | 市場状態判定 (range / trend_up / trend_down / volatile / unclear) |
| 02 | `02_strategy_selection.md` | 上記 regime から strategy 選択 (momentum_pullback / breakout_follow / range_breakout_probe / no_trade) |
| 03 | `03_tp_sl_rules.md` | TP / SL pips の決定ロジック (ATR / 直近 high low / volatility) |
| 04 | `04_risk_rules.md` | 強制 no_trade のトリガー判定と risk セクションの推奨値 |
| 05 | `05_output_format.md` | YAML 出力スキーマ + hard_limit 範囲 |
| 06 | `06_recheck_cadence.md` | `next_advisor_run_in_minutes` の決定 |
| 07 | `07_execution_cost.md` | spread / slippage を考慮した entry 抑止 |
| 08 | `08_performance_review.md` | 週次 / 月次の成績レビュー (手動で使う。advisor のフローからは呼ばれない) |
| 09 | `09_symbol_selection.md` | 追加する通貨ペアの評価 (手動で使う。advisor のフローからは呼ばれない) |

---

## 3. 出力契約 (= validator が読む YAML スキーマ)

Claude が返す YAML は以下のスキーマに従う。違反は `validator.schema` で reject:

[StrategyConfig](../../backend/internal/config/strategy_config.go) struct と一致する nested YAML:

```yaml
config_id: "<YYYYMMDD-HHMMSS-symbol>"
generated_at: <RFC3339 UTC>
valid_from: <RFC3339 UTC>
valid_until: <RFC3339 UTC>
symbol: USD_JPY
enabled: true | false

market_regime:
  type: range | trend_up | trend_down | volatile | unclear
  confidence: 0.0-1.0
  reason: "<free text>"

strategy:
  name: momentum_pullback | breakout_follow | range_breakout_probe | no_trade
  timeframe: "5m"
  trend_timeframe: "1h"

entry:
  max_spread_pips: <float>
  min_volatility_pips_5m: <float>          # omitempty。5m range の下限ガード
  max_volatility_pips_5m: <float>          # omitempty。5m range の上限ガード (急変避け)
  require_breakout: true | false
  direction: buy_only | sell_only | both | none
  allowed_hours_jst: [<int 0-23>, ...]   # omitempty

exit:
  take_profit_pips: <float, hard_limits 範囲内>
  stop_loss_pips: <float, hard_limits 範囲内>
  max_hold_minutes: <int, hard_limits 範囲内>
  extension_max_minutes: <int>             # omitempty
  extension_unrealized_pips_threshold: <float>  # omitempty
  early_exit_window_minutes: <int>         # omitempty。MaxHold 手前の早期 exit 窓
  early_exit_target_pips: <float>          # omitempty。早期 exit 発火の PnL pips
  ratchet_arm_pips: <float>                # omitempty。trailing TP の arm 閾値 (0=無効)
  ratchet_giveback_pips: <float>           # omitempty。peak から retrace この幅で確定

risk:
  quantity: <int, hard_limits 範囲内>
  max_open_positions: <int>
  max_trades_in_this_window: <int>
  max_loss_in_this_window_jpy: <int>

no_trade:
  enabled: true | false                    # true なら strategy.name=no_trade も必須
  reason: "<free text>"

next_advisor_run_in_minutes: <int>          # omitempty
```

詳細スキーマは [05_output_format.md](../../prompts/skills/05_output_format.md)、構造体の SoT は [strategy_config.go](../../backend/internal/config/strategy_config.go)。

---

## 4. 入力フォーマット (= bot が Claude に渡す JSON)

`runtime/ai_input/latest_summary.json` (bot_config の symbols が 2 つ以上なら `latest_summary_<SYMBOL>.json`) — [MarketSummary](../../backend/internal/domain/market/summary.go) struct の JSON シリアライズ:

- `symbol` / `time` / `next_valid_from`
- `current_rate`: bid / ask / spread / mid
- `summary_15m` / `summary_1h` / `summary_6h` / `summary_24h`: 各 window の high / low / range / trend / spread 統計
- `bot_state`: EmergencyStop / CurrentPosition (optional) / OpenPositionsCount / DailyPnLJPY / ConsecutiveLosses / TradesToday / TradesInCurrentWindow
- `recent_trades`: 直近 N 件の PnL (skill 01 / 04 と root の統合判断が読む)
- `recent_rejections`: 直近の signal_rejections (skill 01 と root の統合判断が読む)
- `hard_limits`: 現行 HardLimits の各レンジ (Claude に渡してレンジ内出力させる)
- `allowed_strategies`: validator が受け付ける strategy 名のホワイトリスト

SoT は [domain/market/summary.go](../../backend/internal/domain/market/summary.go)、ビルド処理は [usecase/build_market_summary.go](../../backend/internal/usecase/build_market_summary.go)。

---

## 5. Claude CLI subprocess の制約

[backend/internal/adapter/advisor/claude_cli.go](../../backend/internal/adapter/advisor/claude_cli.go):

- timeout: `bot_config.ai_advisor.claude_cli_timeout_seconds` (未指定時 120s。tracked の `configs/bot_config.yaml` は 300s)
- 並列実行: claude_cli.go から見れば **single subprocess** (`claude -p` 1 回呼び出し)。skill の並列は `generate_strategy_config.md` が Claude 内部で Task ツールを使って実現する
- 失敗時の挙動: timeout / parse_error / cli_error を `ai_advisor_runs.status` に記録、bot は元の active config を維持
- 入出力は `ai_advisor_runs` テーブルに input_json + output_yaml で保存 (デバッグ可能)

---

## 6. プロンプト改善ループ

1. `signal_rejections` テーブルから「最近 reject されてる reason トップ N」を抽出
2. 該当する skill (`04_risk_rules.md` / `07_execution_cost.md` 等) を編集
3. 決定論側 (hard_limits / validator / 戦略コード) の変更は backtest で確認する ([BACKTEST.md](../workflows/BACKTEST.md))。
   LLM の出力そのものは backtest できない (BACKTEST.md §1)
4. paper mode で、事前に決めた件数または期間・判定基準 (BACKTEST.md §6 の `cmd/edge-judge`) のもとで条件を動かさずに回す
5. 基準を満たさなければ live には進めない。満たしても最小ロットから始める

---

## 7. アンチパターン

- ❌ skill の指示を更新したが対応する validator (hard_limits / semantic) を更新し忘れた
- ❌ Claude に YAML の代わりに JSON で返すように指示した (parser が壊れる)
- ❌ skill 間で矛盾する指示を出した (例: `04_risk_rules.md` で qty 100, `08_performance_review.md` で qty 500)
- ❌ プロンプトに bot 内部の private field 名を直接書いた (refactor で壊れる)

---

## 8. 関連 docs

- [ARCHITECTURE.md](../ARCHITECTURE.md)
- [CONFIG.md](../runtime/CONFIG.md) — validator / hard_limits.yaml
- [DATA_MODEL.md](../runtime/DATA_MODEL.md) — `ai_advisor_runs` / `config_validation_events`
- [layers/usecase.md](../architecture/layers/usecase.md) — AdvisorCycle / Promoter
