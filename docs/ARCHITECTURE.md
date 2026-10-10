# fx-bot Architecture Rules

**このファイルとリンク先 docs はすべて設計契約である。**
新規実装・既存リファクタはこの規約を満たすこと。詳細は各リンク先を SoT とし、本ファイルは
全体図 + 依存方向 + 禁止事項の凝縮版として保つ。

目的別の索引: [README.md](README.md) を参照。

---

## レイヤー全体図

```
cmd/bot/main.go (wiring のみ)
   ↓
Handler (app/handler/)
   ↓
[Controller (app/controller/) — 任意]
   ↓
Usecase (usecase/command/, usecase/query/) ← CQRS
   ↓
Domain (domain/<aggregate>/) ← 純粋ロジック / I/O 禁止
   ↓
Port (port/) ← interface のみ
   ↑
Adapter (adapter/broker, adapter/repository, ...)

Safety (safety/) ← 全層から依存可 (cross-cutting)
```

各レイヤーの責務・命名・テスト方針は [architecture/layers/](architecture/layers/) を参照:

- [architecture/layers/handler.md](architecture/layers/handler.md)
- [architecture/layers/usecase.md](architecture/layers/usecase.md)
- [architecture/layers/domain.md](architecture/layers/domain.md)
- [architecture/layers/port.md](architecture/layers/port.md)
- [architecture/layers/adapter.md](architecture/layers/adapter.md)
- [architecture/layers/safety.md](architecture/layers/safety.md)

---

## 依存方向のルール

- **上から下にだけ依存** (handler → [controller →] usecase → domain) — controller は任意
- usecase は **port** (interface) を経由してのみ I/O する。adapter を直接知らない
- adapter は domain / port を import する (逆は禁止)
- safety はリーフ依存。他から自由に import 可
- 循環依存は絶対禁止

---

## 禁止事項

- ❌ `cmd/bot/main.go` に `buildXxx` のような業務ロジックヘルパーを置く
- ❌ `usecase` が `*broker.GmoBroker` などの具体型を知る (interface 経由のみ)
- ❌ `handler` が直接 repository を呼ぶ
- ❌ `domain` が `pgx`, `net/http`, `slog`, `os` を import する
- ❌ `port` が `config` を import する (R1 guardrail。[layers/port.md](architecture/layers/port.md))

---

## 横串トピック (各 SoT)

| トピック | ファイル |
|---|---|
| 起動 / goroutine / mutex / counters / runtime files | [runtime/RUNTIME.md](runtime/RUNTIME.md) |
| 設定ファイル + validator + DB SoT | [runtime/CONFIG.md](runtime/CONFIG.md) |
| Claude prompt + skill + YAML 出力契約 | [integrations/PROMPTS.md](integrations/PROMPTS.md) |
| テスト戦略 (**strict t_wada 流 R-G-R** + 古典派 + table-driven + integration tag) — Refactor 段の省略禁止 | [workflows/TESTING.md](workflows/TESTING.md) |
| Rollback > Fallback / Tx / mutex / 失敗時の絶対ルール | [architecture/FAILURE_MODES.md](architecture/FAILURE_MODES.md) |
| Counters / logs / `/api/status` / audit tables / runtime files | [runtime/OBSERVABILITY.md](runtime/OBSERVABILITY.md) |
| Backtest 前提 / candle 入力 / same-bar policy / slice | [workflows/BACKTEST.md](workflows/BACKTEST.md) |
| Migration 命名 / cleanup / DATA_MODEL 更新義務 | [workflows/MIGRATIONS.md](workflows/MIGRATIONS.md) |
| Backend DTO ↔ Frontend 型同期契約 | [integrations/API_CONTRACT.md](integrations/API_CONTRACT.md) |
| **merge 前必須チェック** | [architecture/PR_CHECKLIST.md](architecture/PR_CHECKLIST.md) |

運用フロー全体は [runtime/SYSTEM_DESIGN.md](runtime/SYSTEM_DESIGN.md)、DB スキーマは [runtime/DATA_MODEL.md](runtime/DATA_MODEL.md)。

---

## 関連ドキュメント

- [runtime/OPERATIONS_RUNBOOK.md](runtime/OPERATIONS_RUNBOOK.md) — 不変条件 / ハマりポイント / ロールバック手順
