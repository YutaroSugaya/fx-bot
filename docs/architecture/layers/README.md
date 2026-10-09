# Layers Index

このディレクトリは fx-bot の各レイヤーごとの設計契約を分割保存したもの。
全ファイルは [ARCHITECTURE.md](../../ARCHITECTURE.md) と同じく **設計契約の正本** として扱う。

---

## 階層全体図

```
┌────────────────────────────────────────────────────────┐
│ cmd/bot/main.go ─ wiring のみ                          │
└────────────────────────────────────────────────────────┘
                       ↓
┌────────────────────────────────────────────────────────┐
│ Handler  (app/handler/)              — HTTP 入口        │
└────────────────────────────────────────────────────────┘
                       ↓
┌────────────────────────────────────────────────────────┐
│ Controller (app/controller/)         — 任意 / 抽出条件 │
└────────────────────────────────────────────────────────┘
                       ↓
┌────────────────────────────────────────────────────────┐
│ Usecase  (usecase/command/, usecase/query/) ← CQRS     │
└────────────────────────────────────────────────────────┘
                       ↓
┌────────────────────────────────────────────────────────┐
│ Domain   (domain/<aggregate>/)        — 純粋ロジック   │
└────────────────────────────────────────────────────────┘
                       ↓
┌────────────────────────────────────────────────────────┐
│ Port     (port/)                      — interface のみ │
└────────────────────────────────────────────────────────┘
                       ↑
┌────────────────────────────────────────────────────────┐
│ Adapter  (adapter/broker, adapter/repository, etc.)    │
└────────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────────┐
│ Safety   (safety/)                    — cross-cutting  │
└────────────────────────────────────────────────────────┘
```

---

## 各レイヤー (詳細は個別ファイル)

| Layer | ファイル | 一言 |
|---|---|---|
| Handler | [handler.md](handler.md) | HTTP 入口。リクエスト → usecase → JSON レスポンス |
| Usecase | [usecase.md](usecase.md) | CQRS で Command / Query に分離。ビジネスフロー調整 |
| Domain | [domain.md](domain.md) | 純粋 Go。Entity / VO / Domain Service。I/O 禁止 |
| Port | [port.md](port.md) | Repository / Broker / Notifier の interface |
| Adapter | [adapter.md](adapter.md) | port interface の具体実装 (Postgres / GMO 等) |
| Safety | [safety.md](safety.md) | emergency_stop / timeouts / 定数。全層から参照可 |

---

## 依存方向のルール (再掲)

- **上から下にだけ依存** (handler → [controller →] usecase → domain)
- usecase は **port** (interface) を経由してのみ I/O する。adapter を直接知らない
- adapter は domain / port を import する (逆は禁止)
- safety はリーフ依存 (= ほぼ純粋関数)。他から自由に import 可
- 循環依存は絶対禁止

---

## 関連 docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — 全体図と禁止事項の凝縮版 (本ディレクトリへの導線)
- [SYSTEM_DESIGN.md](../../runtime/SYSTEM_DESIGN.md) — 何がどんな順番で動くか (運用入口)
- [FAILURE_MODES.md](../FAILURE_MODES.md) — Rollback > Fallback の絶対ルール
- [PR_CHECKLIST.md](../PR_CHECKLIST.md) — merge 前必須チェック
