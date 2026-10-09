# Subagents — どの LLM 経路がどの agent を使うか

`.claude/agents/` には 11 の subagent 定義がある。Go bot は `claude -p` をサブプロセス起動し、
stdin のプロンプトで特定 subagent を名指しして使う。どれが実際に走るかは bot config
(`configs/bot_config.yaml`、live 用は `configs/bot_config.live.example.yaml` からコピーする
`configs/bot_config.live.yaml`)の **feature flag** で決まる
(配線は `backend/cmd/bot/loops.go`)。

**LLM を定期的に呼ぶ経路(`llm_decision` / `ai_advisor` / `advisor_v2`)はすべて opt-in。**
tracked の `configs/bot_config.yaml` では全部 off で(`backend/internal/config/tracked_bot_config_test.go`
が固定)、既定の bot は claude CLI を定期起動しない。

> 定義ファイルは、flag を立てた経路から名指しされたときだけ使われる。subagent のメタデータ
> (name / description)はセッション開始時にロードされるだけなので、使わない定義を置いておく
> 常時コストはほぼゼロ。機能が重なる定義(下表の「重なり」)を編集するときは、どの経路の定義かを
> 確かめてから触る。

## 自律 LLM 判断ループ(`llm_decision.enabled`)

`runLLMDecisionScheduler`(判断)/ `runReflectionScheduler`(反省)。判断結果は既存の発注経路
(`OnSignal` → risk Gate + veto → broker OCO)に渡り、LLM が直接発注することはない。

### 判断サイクル

| 設定 | 使う agent |
|---|---|
| `decision_single_agent` 省略 / `true`(既定) | **subagent なし**。`llm_decision_cli.go` の `BuildSingleAgentDecisionPayload`(Go 内テンプレート + playbook + MarketSummary JSON)を 1 回の `claude -p --tools Read` で実行する。Task ツールを渡さないので subagent は起動できない |
| `decision_single_agent: false`(パネルモード) | `market-regime` → `trade-decider` の 2 段(`BuildDecisionPayload`、`--tools Read,Task`) |

| subagent | 役割 | 起動元 |
|---|---|---|
| `market-regime` | 相場レジーム分類(trend_up / trend_down / range / volatile / unclear) | `llm_decision_cli.go`(パネル Stage 1) |
| `trade-decider` | playbook のチェックリストを数値で判定 → side + TP/SL か no_trade | `llm_decision_cli.go`(パネル Stage 2) |

### 反省(Reflexion)サイクル(`llm_decision.reflection_enabled`)

省略時は `true`(`llm_decision` が on のとき)。`false` なら reflection scheduler 自体が起動せず、
playbook は自動で書き換わらない。

`reflection_cli.go` の inline プロンプト(`BuildReflectionPayload`)が orchestrator 役を担い、
Task で 3 体を呼んで改訂版 playbook を 1 本にまとめる:

| subagent | 役割 | 起動元 |
|---|---|---|
| `reflection-regime` | 反省パネル: どの相場で勝ち / 負けかを分析 | `reflection_cli.go` |
| `reflection-risk` | 反省パネル: RR・SL 幅・コスト床・サイジングの監査 | `reflection_cli.go` |
| `reflection-strategy` | 反省パネル: 欠陥を塞ぐ条件改訂案 | `reflection_cli.go` |

`reflection-analyst` は反省役を 1 体で行う定義。orchestrator 役は `reflection_cli.go` に inline 化
されているため、現在の bot からは名指しされない。

## advisor v1 パネル(`ai_advisor.enabled`)

`runAdvisorScheduler` 経由。[prompts/generate_strategy_config.md](../../prompts/generate_strategy_config.md)
が Task で 4 体を呼び、strategy config の YAML を 1 本生成する(プロンプトと出力契約は
[PROMPTS.md](../integrations/PROMPTS.md))。

| subagent | 役割 | 判断ループ側との重なり |
|---|---|---|
| `regime-classifier` | 市況分類 | `market-regime` と同じ 5 クラス分類 |
| `strategy-selector` | 4 戦略を採点し最良を選ぶ | `trade-decider` の戦略選択 |
| `tpsl-designer` | TP/SL/MaxHold 設計 | `trade-decider` が TP/SL を内包 |
| `risk-auditor` | 強制 no_trade トリガー監査 | `trade-decider` の除外条件 |

## advisor v2(`advisor_v2.enabled`)

| subagent | 役割 | 備考 |
|---|---|---|
| `breakout-advisor` | 決定論の検出器が出したチャートブレイク候補を 8 軸で採点し go / no-go を返す judge(古典的チャートブレイクモデル) | `advisor_v2.deterministic: true` のときは呼ばれず、検出器の合致だけで建てる(守りは risk Gate) |

## ミラー: `.codex/agents/`

v1 パネル 4 体(`regime-classifier` / `strategy-selector` / `tpsl-designer` / `risk-auditor`)の
`.toml` 複製が別ツール(Codex)用に存在する。**Claude Code 用の正本は `.claude/agents/*.md`** 側。
v1 の定義を改訂するときは両者の drift に注意する。

## 有効化

- 自律 LLM 判断ループ: `llm_decision.enabled: true`
- advisor v1: `ai_advisor.enabled: true`(`llm_decision` と同時に on にしない = 同じ symbol に 2 系統の OnSignal が入る)
- advisor v2: `advisor_v2.enabled: true`(`deterministic: false` なら `breakout-advisor` の LLM judge を使う)

どれも有効にすると claude CLI の利用料・利用枠を消費する。live の TP/SL は経路によらず broker 側 OCO に置かれる。
