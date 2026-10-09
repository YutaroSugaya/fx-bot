# PR Checklist

merge 前に **必ず** 全項目を満たすことを確認する。

このリストを全て満たすまで merge しない。

---

## レイヤー / 依存

- [ ] `cmd/bot/main.go` に business logic を追加していない (wiring のみ)
- [ ] Handler が直接 repository / broker を呼んでいない
- [ ] Usecase は port 経由でしか I/O していない
- [ ] Domain layer に `pgx`, `net/http`, `slog`, `os` の import がない
- [ ] 循環依存を作っていない (`go build` が通る = OK)

## CQRS

- [ ] 状態変更を含む処理は `usecase/command/` に置いた
- [ ] 読み取り専用処理は `usecase/query/` に置いた
- [ ] 1 つの usecase が Command + Query を兼任していない
- [ ] Command の戻り値は副作用 ID + 最小情報 (大きな読み取りデータは Query に分けた)

## エラー処理

- [ ] エラー時に「log + continue」していない (= fallback ではなく rollback or trip)
- [ ] 複数テーブル更新は 1 Transaction で囲んだ
- [ ] Broker / DB の同期失敗時は emergency_stop を発火している
- [ ] 共有リソース (positions, advisor cycle) は mutex で直列化されている

## テスト

- [ ] Domain layer のテストは fake / mock を使わず純粋単体
- [ ] Usecase のテストは **極力 real 実装** を使う
  (`broker.PaperBroker` / `backtest.InMemoryPositionRepo` / `InMemoryTradeRepo` /
  `InMemoryStrategyConfigRepo` / `InMemoryValidationEventRepo` /
  `InMemoryPositionCloser` / `safety.Trip` 等)
- [ ] mock/fake はシステム境界 (DB/HTTP/CLI) + 失敗注入 + 非決定性除去 の 3 用途のみ。
      各 mock の **ファイル / 型 / 関数の先頭コメント** に「3 用途のどれに該当するか」を
      明記した (例: `// MOCK rationale (TESTING.md §2 古典派 3 用途): §1 system boundary`)
- [ ] Adapter (DB / Broker) は integration / 録画再生でテスト
- [ ] Handler は httptest で書いた
- [ ] 同じ動作軸の複数ケースは **table-driven** にした (`t.Run(tc.name, ...)`)
- [ ] 新機能 / bug fix は **strict t_wada 流 TDD** で実装した:
  Red (失敗を目視確認) → Green (最小実装) → **Refactor** (省略禁止)。
- [ ] **Refactor 段で何をしたか** を PR description に 1 項目以上書いた
      (命名変更 / 関数抽出 / 重複削除 / mock 必要性再評価 / SOLID 違反解消 のいずれか)
- [ ] Refactor 段で Green 時に追加した fake / mock を「3 用途のどれか?」で再評価し、
      該当しないものは real collaborator に置換した
- [ ] `go test -race ./...` が通る

## observability

- [ ] Live で観測したい新しい失敗パターンは `app.Counters` に追加した
- [ ] 重要な遷移に `Logger.Info / Error` を入れた (key=value 形式)
- [ ] emergency_stop trip の理由文字列が新規パターンも grep しやすい命名

## コード品質

- [ ] magic number を constants ファイルに集約した
- [ ] `var _ = SomeType` のような死に guard を残していない
- [ ] 1 ファイル 1 type / 1 関数 100 行以内 (やむを得ない場合のみ例外)
- [ ] スペルチェック warning 以外の `go vet` クリーン

---

## Schema / Migration 変更時の追加チェック

- [ ] migration ファイル名は `NNNN_topic.(up|down).sql` 形式で、version は `0001` から gap / duplicate なし
- [ ] migration `*.up.sql` と `*.down.sql` の **ペア** が揃っている
- [ ] [DATA_MODEL.md](../runtime/DATA_MODEL.md) のテーブル / 制約 / mermaid ER を更新した
- [ ] [DATA_MODEL.md](../runtime/DATA_MODEL.md) の「マイグレーション履歴」表に行を追加した
- [ ] [MIGRATIONS.md](../workflows/MIGRATIONS.md) の履歴 / 手順が必要に応じて同期されている
- [ ] 既存重複データがある場合の cleanup を up.sql に入れた (例: partial unique index)
- [ ] port record (`backend/internal/port/repository.go`) と domain entity の対応フィールドを更新した
- [ ] 該当 adapter (`backend/internal/adapter/repository/*_repo.go`) の SQL を更新した
- [ ] `INTEGRATION_TEST_DB_URL` を専用の test DB (名前が `_test` で終わる DB) に向けて `make test-integration` が pass
      (integration test はテーブルを空にするので、取引履歴のある DB には絶対に向けない)

---

## API 変更時の追加チェック

- [ ] backend DTO ([backend/internal/app/handler/types.go](../../backend/internal/app/handler/types.go) or `usecase/query/*.go`) を更新
- [ ] frontend type ([frontend/app/page.tsx](../../frontend/app/page.tsx) の inline `type Foo = { ... }` / [frontend/app/lib/types.ts](../../frontend/app/lib/types.ts) の共有型) を同期
- [ ] [API_CONTRACT.md](../integrations/API_CONTRACT.md) のエンドポイント一覧を更新
- [ ] `cd frontend && npm run build` が pass

---

## Docs / Plan 変更時の追加チェック

- [ ] ARCHITECTURE.md の依存方向 / 禁止事項を変更したなら、[layers/](layers/) の対応 md も更新
- [ ] [PR_CHECKLIST.md](PR_CHECKLIST.md) (このファイル) を変更したなら ARCHITECTURE.md の参照も同期

---

## Live 発注経路を変更したときの追加チェック

- [ ] Live close / reconcile / entry admission の失敗経路が fail-closed (emergency_stop trip または起動中断) のまま
- [ ] TP/SL は broker 側 OCO に置かれ、bot の OnTick 監視だけに依存していない
- [ ] risk gate の集計は closed_at 基準、active config の SoT は DB (`strategy_configs`)
- [ ] integration test (`make test-integration`) を専用 test DB で実行した
- [ ] paper mode で同じ config を動かし、発注 → OCO → 決済 → reconcile の一巡を確認した

詳細は [FAILURE_MODES.md](FAILURE_MODES.md) を参照。

---

## 関連 docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — 設計契約の入口
- [layers/](layers/) — レイヤー別契約
- [FAILURE_MODES.md](FAILURE_MODES.md) — Rollback > Fallback
- [TESTING.md](../workflows/TESTING.md) — テスト戦略
- [OBSERVABILITY.md](../runtime/OBSERVABILITY.md) — Counters / logs / status
- [MIGRATIONS.md](../workflows/MIGRATIONS.md) — schema 変更の手順
- [API_CONTRACT.md](../integrations/API_CONTRACT.md) — backend ↔ frontend 同期
