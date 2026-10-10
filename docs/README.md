# fx-bot Docs — 目的別の索引

「何を知りたいか」で入口を選ぶ。ここにある docs は**設計契約**(コードが従うべき規約)か**現行仕様**(今の動き)のどちらか。
リポジトリ全体の入口は [../README.md](../README.md)。

## まず読む

| 知りたいこと | ファイル |
|---|---|
| 全体像・層と依存方向・禁止事項 | [ARCHITECTURE.md](ARCHITECTURE.md) |
| 何がどんな順番で動くか(起動・発注・決済・advisor) | [runtime/SYSTEM_DESIGN.md](runtime/SYSTEM_DESIGN.md) |
| PR を merge してよいか | [architecture/PR_CHECKLIST.md](architecture/PR_CHECKLIST.md) |

## コードを書く前 / レビュー時 → `architecture/`

| ファイル | 内容 |
|---|---|
| [architecture/layers/](architecture/layers/) | 各層(handler / usecase / domain / port / adapter / safety)の責務・命名・テスト方針 |
| [architecture/FAILURE_MODES.md](architecture/FAILURE_MODES.md) | 失敗時の反応(Rollback > Fallback・Tx・mutex・緊急停止に倒す経路) |
| [architecture/PR_CHECKLIST.md](architecture/PR_CHECKLIST.md) | merge 前の必須チェック |
| [architecture/SUBAGENTS.md](architecture/SUBAGENTS.md) | `.claude/agents/` の subagent と、どの LLM 経路が使うか |

## bot の動き → `runtime/`

| ファイル | 内容 |
|---|---|
| [runtime/SYSTEM_DESIGN.md](runtime/SYSTEM_DESIGN.md) | 全体図と起動 / 発注 / 決済 / advisor のフロー |
| [runtime/RUNTIME.md](runtime/RUNTIME.md) | 常駐 goroutine・mutex・カウンタ・`runtime/` のファイル |
| [runtime/CONFIG.md](runtime/CONFIG.md) | bot_config / hard_limits / strategy config と、active config を DB に入れる手順 |
| [../configs/README.md](../configs/README.md) | `configs/` の各ファイル (戦略 config のファミリーと用途・config-check の可否) |
| [runtime/STATE_MACHINE.md](runtime/STATE_MACHINE.md) | Position の状態遷移と position_state_events |
| [runtime/DATA_MODEL.md](runtime/DATA_MODEL.md) | PostgreSQL の全テーブルと不変条件 |
| [runtime/OBSERVABILITY.md](runtime/OBSERVABILITY.md) | カウンタ・`/api/status`・監査テーブル・ログ |
| [runtime/OPERATIONS_RUNBOOK.md](runtime/OPERATIONS_RUNBOOK.md) | 運用の不変条件・ハマりどころ・ロールバック手順 |
| [../deploy/launchd/README.md](../deploy/launchd/README.md) | macOS での常駐化(launchd)・死活監視 |

## 開発作業の手順 → `workflows/`

| ファイル | こんな時に開く |
|---|---|
| [workflows/TESTING.md](workflows/TESTING.md) | テストを書く / テスト方針(strict TDD・integration タグの扱い)を確認する |
| [workflows/MIGRATIONS.md](workflows/MIGRATIONS.md) | DB スキーマを変える / migration を足す |
| [workflows/BACKTEST.md](workflows/BACKTEST.md) | ヒストリカルデータを入れて戦略を backtest / エッジ判定する |

## 外部との接合 → `integrations/`

| ファイル | 内容 |
|---|---|
| [integrations/API_CONTRACT.md](integrations/API_CONTRACT.md) | `/api/*` の一覧と、backend DTO ↔ frontend の型の同期契約 |
| [integrations/PROMPTS.md](integrations/PROMPTS.md) | advisor のプロンプト / skill と YAML 出力スキーマ |

## ディレクトリ

```
docs/
├── README.md            ← この索引
├── ARCHITECTURE.md      ← 設計契約の凝縮版
├── architecture/        ← コードを書く前に読む(layers/・FAILURE_MODES・PR_CHECKLIST・SUBAGENTS)
├── runtime/             ← bot の動き(SYSTEM_DESIGN・RUNTIME・CONFIG・STATE_MACHINE・DATA_MODEL・OBSERVABILITY・OPERATIONS_RUNBOOK)
├── workflows/           ← 開発作業の手順(TESTING・MIGRATIONS・BACKTEST)
└── integrations/        ← 外部との契約(API_CONTRACT・PROMPTS)
```
