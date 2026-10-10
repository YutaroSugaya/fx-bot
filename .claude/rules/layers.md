---
paths:
  - "backend/**"
---

# 層アーキテクチャ規約(backend 編集時のみロード)

> このルールは `backend/**` を触るときだけ文脈に入る(無関係な docs/frontend 作業では消費しない)。
> **決定論的な強制**は二段で別に存在する:
> - `.claude/hooks/pre-stop-checks.sh`(Step 5)が turn 終了時に grep で違反検出 → exit 2(事後catch)
> - 詳細・例外・理由は `docs/architecture/layers/{domain,port,usecase,adapter,handler,safety}.md`(SSOT)
> ここに書くのは「コードを書く前」に守るべき不変条件の索引(事前reminder)。

## 依存方向(下位は上位を知らない)
- **domain** は I/O フレームワークを import しない(`pgx` / `net/http` / `log/slog` など)→ `layers/domain.md`
- **domain / port** は上位層(`adapter` / `app` / `usecase`)を import しない → `layers/{domain,port}.md`
- **port** は `config` を import しない(R1 ガードレール)→ `layers/port.md §config 依存の禁止`
- **usecase** の production コードは具象 `adapter` を直 import しない(port 経由)→ `layers/usecase.md`
- **handler** は `adapter/repository` を import しない(usecase 経由)→ `layers/handler.md`

## CQRS / スキーマ
- Command と Query を分離(`usecase/command` ↔ `usecase/query`)→ `layers/usecase.md`
- スキーマ DDL は `backend/migrations/` のみ。repository/port のコードに `CREATE/ALTER/DROP TABLE` を書かない → `docs/workflows/MIGRATIONS.md`
