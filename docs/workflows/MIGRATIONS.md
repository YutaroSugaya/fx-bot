# Migrations — Schema 変更の手順 / 命名 / cleanup / DATA_MODEL 更新

PostgreSQL のスキーマ変更を **必ず追跡可能** に管理する仕組み。
実体は [backend/migrations/](../../backend/migrations/),
[backend/cmd/migrate/main.go](../../backend/cmd/migrate/main.go),
[backend/sqlc.yaml](../../backend/sqlc.yaml),
[backend/internal/adapter/repository/queries/](../../backend/internal/adapter/repository/queries/)。

---

## 0. 現状

- 初期スキーマは `0001_init.up.sql` + `0001_init.down.sql` の **1 ペア** (paper 期間中に squash 済み)。
  以降の変更は `0002+` の forward ALTER (一覧は §10)。
- schema 詳細は [DATA_MODEL.md](../runtime/DATA_MODEL.md) (18 tables, 7 main + 11 junction)。
- 全 query は **sqlc** で生成 (`backend/internal/adapter/repository/queries/*.sql` →
  `backend/internal/adapter/repository/dbgen/`)。
- カラム追加時は **4 点セット** を必ず更新する:
  1. `migrations/NNNN_<topic>.up.sql` / `.down.sql` (`ALTER TABLE ... ADD COLUMN`)
  2. `queries/*.sql` の `INSERT` / `SELECT` 列追加 → `make sqlc-generate`
  3. 該当 repo wrapper (例: `position_repo.go`) で port struct ↔ dbgen struct のマップ拡張
  4. [DATA_MODEL.md](../runtime/DATA_MODEL.md) の該当テーブル列表
- **migration なしの query 意味変更** (列追加なし・`queries/*.sql` だけを変える) も同じく
  `make sqlc-generate` + DATA_MODEL.md の該当箇所を更新する。例: `queries/trades.sql` の loss/PnL 集計
  (`SumOpenedLossJPYSince` / `SumClosedLossJPYSince` / `SumClosedLossJPYBySymbolSince` /
  `SumPnLJPYClosedSinceBySymbol`) は NET (`profit_loss_jpy − fee_jpy + swap_jpy`) で集計する
  (GMO 手数料込みの損失額で日次 cap を判定 = 安全側)。スキーマ無変更なので DB マイグレは不要、bot 再起動で反映。

> ⚠️ **Live 投入後は squash 禁止** (= paper 期間限定の one-shot 操作)。
> 詳細は §4 「Live 投入後の禁止事項」を参照。

> 📌 **down.sql は必ず実コードを書く** (空コメントだけは禁止)。
> `cmd/migrate/migration_pair_test.go::TestAllUpHaveNonEmptyDown` が CI で
> 各 up.sql に対応する down.sql が非空かつ DROP/ALTER を含むことを検査する。
> 0001 は CASCADE で全テーブル drop、0002+ は逆 ALTER (ADD ↔ DROP) が定石。

> 📌 **新しい `trades.close_reason` を返すコードは、CHECK 制約を拡張する migration と同じ変更に入れる**。
> CHECK に無い値を返すと `CloseAndRecord` が制約違反で失敗し、reconcile は synthetic 0-PnL
> (`reconcile_cold_close`) にフォールバックして実際の損益が帳簿に乗らない。0004 / 0005 / 0006 はこの漏れの修正、
> 0010 / 0011 は evaluator 追加と同時に拡張した例。

---

## 1. 命名規約

```
backend/migrations/
├── 0001_init.up.sql      ← squash 済み (18 tables / 7 main + 11 junction)
├── 0001_init.down.sql
├── 0002_<topic>.up.sql   ← 以降は forward ALTER のみ
└── 0002_<topic>.down.sql
```

- 4 桁連番 (`0001`, `0002`, ...) — gaps を作らない
- `<seq>_<topic>.up.sql` と `<seq>_<topic>.down.sql` の **ペア必須**
- topic は snake_case で 3 単語以内が目安
- 1 migration = **1 概念変更** (例: 新カラム追加と新テーブル作成は別 migration)

`pre-stop-checks.sh` が naming / pair / 連番を自動検証する (§9)。

---

## 2. up / down の責務

| ファイル | 役割 |
|---|---|
| `*.up.sql` | 新しい状態を作る (CREATE / ALTER / INSERT) |
| `*.down.sql` | up を完全に巻き戻す (DROP / ALTER REVERT / DELETE) |

### 実装ルール

- **up は idempotent 推奨** (`ADD COLUMN IF NOT EXISTS ...`)。
  ただし squash 済 `0001_init.up.sql` は **plain CREATE** (`IF NOT EXISTS` を付けない) —
  既存 schema 上で再実行すれば落ちる、それが意図 (= 既存 DB に対する誤再生を防ぐ)。
- **down は data loss を許容**: 巻き戻し時に新カラムのデータは失われる前提
- **down が失敗するパターン** (= 例: 外部参照テーブルが既にある) を up コメントで明記
- **partial unique index** などは `WHERE` 条件を明示
- 列追加は `NOT NULL DEFAULT <OFF 相当>` か nullable にし、既存行・稼働中の bot が無変更で動くようにする
  (例: 0003 / 0009 の ratchet 列は DEFAULT 0/false = 機能 OFF)

---

## 3. 実行コマンド

```bash
# 現在の状態確認
make migrate-status

# 全 up を実行 (boot 時に自動: make start)
make migrate-up

# 直前の migration を 1 つ戻す
make migrate-down
```

[backend/cmd/migrate/main.go](../../backend/cmd/migrate/main.go) が:
- `schema_migrations` テーブルから既適用バージョンを読む
- 未適用 `*.up.sql` を昇順で実行
- 各実行を `schema_migrations` テーブルに記録

---

## 4. Live 投入後の禁止事項

squash (= 過去 migration の `0001` 1 本化) は **paper 期間のみ** 許される操作。
Live で実取引データが入った後は **禁止**:

| 禁止操作 | 理由 |
|---|---|
| ❌ `0001_init.up.sql` を編集 | 既存 Live DB と新規 paper DB の schema 履歴がズレる。recovery / forensics 不能 |
| ❌ 既存 `0002+` を squash して `0001` に統合 | 同上 |
| ❌ 既存 migration 番号の rename / 削除 | `schema_migrations` table の version 記録と齟齬 |
| ❌ 既存 migration の up.sql 内容を後から書き換え | 既適用 DB と冪等性が崩れる |

**Live 投入後の schema 変更は必ず新規 `NNNN_<topic>.up.sql` を追加する forward ALTER**。
データ移行が必要なら `BEGIN; ... COMMIT;` で原子化し、cleanup-aware (§5) で書く。
SQL コメントだけの修正 (schema 変更なし) は既存ファイルを直してよい。

migration squash を再度行いたい場合は:
1. 該当 RDB を別環境 (paper) でゼロから drop / recreate できる状態にする
2. データ移植が必要なら export → import を別途実施
3. squash PR とは独立に運用判断を docs (本ファイル) に明記してから実施

---

## 5. Cleanup-aware migration

既存データを壊さずに新スキーマへ移行する場合、up で **cleanup を入れる**:

```sql
BEGIN;

-- 既存重複行を expire してから unique index を作る
WITH ranked AS (
    SELECT id, ROW_NUMBER() OVER (
        PARTITION BY symbol, mode ORDER BY activated_at DESC NULLS LAST, id DESC
    ) AS rn
    FROM strategy_configs WHERE status = 'active'
)
UPDATE strategy_configs SET status = 'expired'
WHERE id IN (SELECT id FROM ranked WHERE rn > 1);

CREATE UNIQUE INDEX IF NOT EXISTS strategy_configs_active_uniq
ON strategy_configs (symbol, mode)
WHERE status = 'active';

COMMIT;
```

cleanup なしで partial unique index を作ると、既存重複 active があると migration が落ちる。

> 注: `0001_init.up.sql` には partial unique index
> `uniq_strategy_configs_active_per_symbol_mode` を直接含めている。
> Live 投入後に類似 index を追加する場合は上の cleanup pattern を使う。

---

## 6. sqlc を使った新規 query 追加フロー

**全 query を sqlc で生成** している。手書き SQL は repository 層に置かない。

### 追加手順

```text
1. 必要なら新規 migration: backend/migrations/NNNN_<topic>.up.sql / .down.sql
   (= 既存 column / table で足りるなら skip)

2. クエリ追加: backend/internal/adapter/repository/queries/<table>.sql
   例) -- name: ListOpenOrClosing :many
       SELECT * FROM positions WHERE symbol = $1 AND status IN ('OPEN','CLOSING');

3. 再生成: make sqlc-generate
   → backend/internal/adapter/repository/dbgen/ に Go コードが生成される

4. repository wrapper を追加 / 更新:
   backend/internal/adapter/repository/<repo>_repo.go
   port interface 経由で usecase に公開

5. integration test: *_integration_test.go で実 Postgres (_test DB) に対して検証
   ($ make test-integration — 手順と DB ガードは TESTING.md §6)
```

### 設定ファイル

- [backend/sqlc.yaml](../../backend/sqlc.yaml) — sqlc config (schema path / queries path / 出力先)
- 再生成コマンド: `make sqlc-generate` (= 内部で `cd backend && sqlc generate`)

### 注意

- DDL (`CREATE TABLE` / `ALTER TABLE` / `ADD COLUMN`) は **絶対に query ファイルに書かない**。migrations にだけ書く (`pre-stop-checks.sh` の static check で検出)。
- sqlc 生成コードは手で編集しない (= 次回 `make sqlc-generate` で消える)。
- **JOIN 経由で junction の列を main 行に乗せて返すケース**: `ListOpenOrClosingPositions` は `recovered_positions` を LEFT JOIN し `COALESCE(r.recovery_reason, '')::text AS recovery_reason` で空文字列を保証する (sqlc は LEFT JOIN の結果列を nullable 扱いするので `::text` cast + COALESCE で `string` ベタ列にする)。repository wrapper が reason 文字列を `port.PositionSource` に map する。同じパターンを他 junction に使う際の参考。
- **status ガード付き `:one ... RETURNING` で「対象なし」を表現するパターン**: `ExtendPositionMaxHold` は `UPDATE positions SET max_hold_minutes = max_hold_minutes + sqlc.arg(add_minutes) WHERE id=$id AND status='OPEN' RETURNING max_hold_minutes, opened_at` を `:one` で生成する。`WHERE status='OPEN'` にマッチしない (CLOSING/CLOSED/未知 id) と RETURNING が 0 行 → `:one` は `pgx.ErrNoRows` を返す。repository wrapper はこれを `errors.Is(err, pgx.ErrNoRows)` で捕まえて `(nil, nil)` (= 対象なし) に翻訳し、usecase 側で `ErrPositionNotFound` に map する。`:execrows` (UpdatePositionRatchetState のように affected 行数だけ欲しい場合) と使い分ける: 更新後の値を呼び元に返したいなら `:one + RETURNING`、no-op を黙って許すだけなら `:execrows`。
- **per-symbol vs account-wide query の命名規約**: 多 symbol 化 (`bot_config.symbols: [...]`) 以降、同じ集計を per-symbol と account-wide の 2 経路で持つことがある。命名は揃える:
  - account-wide: `<Action>ClosedSince` / `CountOpenPositionsAllSymbols` (filter 引数なし)
  - per-symbol  : `<Action>ClosedBySymbolSince` / `*BySymbol` (filter 引数 `symbol`)
  - 例: `SumClosedLossJPYSince` (account-wide) と `SumClosedLossJPYBySymbolSince` (per-symbol)
  - risk gate の per-symbol cap には必ず `*BySymbol` を使う (account-wide query を流用すると sibling symbol の trade で誤発火する)。詳細は [layers/port.md `多 symbol risk gate 用の per-symbol / account-wide 集計分離`](../architecture/layers/port.md) 参照。

---

## 7. DATA_MODEL.md / MIGRATIONS.md の更新義務

schema を変更したら **必ず** [DATA_MODEL.md](../runtime/DATA_MODEL.md) を更新する:

- 該当テーブルのカラム一覧表に追加 / 削除
- 「Why important」「制約」セクションを更新
- §マイグレーション履歴 表に行を追加
- mermaid ER 図を更新

加えて、このファイルの §10 一覧 / 注意点も同期する。

Stop hook (`.claude/hooks/pre-stop-checks.sh`) は `backend/migrations/` 変更時に
DATA_MODEL.md とこのファイルの **同時更新を強制** する (= 更新が無いと merge blocker)。

加えて **code → docs sync hook** (`.claude/hooks/docs-sync-check.sh`) が
`port/repository.go` / `domain/position/state.go` / `safety/*.go` 等の変更に対しても
対応する docs (`layers/*.md` / `STATE_MACHINE.md`) の同時更新を強制する。
allowlist は `.claude/hooks/spec-sync-allowlist.txt` で管理。

PR レビューでも「migration あったけど docs 更新ない」は merge ブロッカー
([PR_CHECKLIST.md](../architecture/PR_CHECKLIST.md))。

---

## 8. port record / Domain entity の更新

schema が変わると以下も影響する:

| 影響範囲 | 確認すべきファイル |
|---|---|
| port record | `backend/internal/port/repository.go` |
| Domain entity | `backend/internal/domain/<aggregate>/*.go` |
| Repository adapter | `backend/internal/adapter/repository/*_repo.go` (sqlc 入力 query 含む) |
| Tests | `*_test.go` で新カラムを assert |

---

## 9. Stop hook / 自動チェック

Claude Code Stop hook (`.claude/hooks/pre-stop-checks.sh`) は migration / DB 契約変更時に以下をブロック条件として検査する:

- `backend/migrations/` 配下のファイル名が `NNNN_topic.(up|down).sql` 形式である
- up/down のペアが揃っている
- migration version が `0001` から gap / duplicate なしで連続している
- migration 変更時に `docs/runtime/DATA_MODEL.md` と `docs/workflows/MIGRATIONS.md` も変更されている
- repository / port に schema DDL (`CREATE TABLE`, `ALTER TABLE`, `ADD COLUMN` など) を直書きしていない
- DB 関連変更時は `INTEGRATION_TEST_DB_URL` が設定され、`make test-integration` が pass する

`docs-sync-check.sh` は逆方向 (code → docs) のドリフトを検出する:

- `backend/internal/port/repository.go` 変更時に `docs/architecture/layers/usecase.md` (または `layers/port.md`) の更新を要求
- `backend/internal/domain/position/state.go` 変更時に `docs/runtime/STATE_MACHINE.md` の更新を要求
- `backend/internal/safety/*.go` 変更時に `docs/architecture/layers/safety.md` の更新を要求
- `backend/internal/adapter/repository/queries/*.sql` 変更時に `DATA_MODEL.md` + `MIGRATIONS.md` の更新を要求
- 仕様非変更の修正は `.claude/hooks/spec-sync-allowlist.txt` で除外可能

`INTEGRATION_TEST_DB_URL` が無い環境では、DB 関連変更の Stop hook は Stop をブロックする
(同じセッションで 3 回続けてブロックしたあとは警告を残して通す)。
先に `make db-up` で Postgres を起動し、`_test` で終わるテスト用 DB を作って migration を当て、その DSN を export してから再実行する
([TESTING.md §6](TESTING.md))。integration suite はその DB の全テーブルを空にする。

---

## 10. migration 一覧

| # | 内容 |
|---|---|
| 0001 | **初期スキーマ** (squash 済み) — 18 tables (7 main + 11 junction)、nullable / sentinel 撤廃、State パターン + append-only ledger。partial unique index `uniq_strategy_configs_active_per_symbol_mode` を含む |
| 0002 | `positions.early_exit_window_minutes` / `early_exit_target_pips` 追加 (MaxHold 手前の早期 exit。DEFAULT 0 = 無効) |
| 0003 | `positions` に ratchet TP (trailing take-profit) 4 列を追加: `ratchet_arm_pips` / `ratchet_giveback_pips` (config snapshot)、`peak_unrealized_pips` / `ratchet_armed` (runtime state、OnTick で更新・armed は monotonic)。armed 後に peak から giveback だけ戻ったら MARKET close (Live でも close saga の cancel→MARKET を再利用)。新 query `UpdatePositionRatchetState`。DEFAULT 0/false = OFF |
| 0004 | `trades.close_reason` CHECK に `'ratchet_takeprofit'` を追加 (0003 の reason。CHECK 拡張のみ) |
| 0005 | `trades.close_reason` CHECK に `'early_exit'` を追加 (early-exit window 発火を `max_hold` と分離。`CountEarlyExitTradesSinceBySymbol` は `close_reason='early_exit'` を直接カウント) |
| 0006 | `trades.close_reason` CHECK に `'broker_close'` を追加 (TP/SL 理論価格に一致しない broker-side close を reconcile が実 fill から復元して記録する reason) |
| 0007 | `trades` に `fee_jpy` / `swap_jpy` / `fee_estimated` を追加 (GMO 手数料 0.002%×往復 + 跨ぎスワップの per-trade 計上。`profit_loss_jpy` は GROSS 維持。close saga / reconcile の `composeLiveCloseCosts` が書く) |
| 0008 | `positions` に `entry_fee_jpy` / `entry_spread_pips` / `entry_slippage_pips` を追加 (entry 時点の実コスト。全 nullable = NULL 未捕捉と 0 実報告を区別。execute_order / manual_trade の entry 経路が書き、close saga が `trades.fee_jpy` へ合成) |
| 0009 | `positions` に trailing STOP (損切り側 ratchet) の runtime 2 列 `trough_unrealized_pips` / `loss_ratchet_armed` を追加。arm/giveback は利確側 `ratchet_arm_pips` / `ratchet_giveback_pips` を共用する mirror なので snapshot 列は増やさない。`UpdatePositionRatchetState` が 4 値を 1 write で更新。DEFAULT 0/false = OFF |
| 0010 | `trades.close_reason` CHECK に `'ratchet_stoploss'` を追加 (0009 の trailing STOP が返す reason。0009 と同じ変更で拡張) |
| 0011 | `trades.close_reason` CHECK に `'session_flatten'` を追加 (`llm_decision.session_flatten_jst` による毎朝の全玉手仕舞いが返す reason。`evaluateSessionFlattenExit` の追加と同じ変更で拡張) |

0002 以降はすべて列追加 (nullable / DEFAULT 付き) か CHECK 拡張 (既存値は新集合の部分集合) で、既存行への影響はない。

---

## 11. アンチパターン

- ❌ up.sql だけ書いて down.sql を「あとで」放置する → ペア義務
- ❌ `0002_new_columns.up.sql` の中で 3 テーブルにカラム追加 + 既存テーブル名 rename → 1 migration 1 概念に分割
- ❌ partial unique index を既存重複あるテーブルに `CREATE` だけして migration を落とす → cleanup を入れる
- ❌ migration 番号を飛ばす (`0003` の次が `0006`) → gap なし
- ❌ 新しい close_reason / enum 値をコードだけに足して CHECK 制約を拡張しない → 同じ変更で migration を足す
- ❌ schema 変更後 DATA_MODEL.md / port record を更新しない → PR_CHECKLIST.md でブロック
- ❌ **Live 投入後の squash / 既存 migration 書換え** (§4) → recovery 不能、絶対禁止
- ❌ repository 層に手書き SQL を書く → sqlc 経由を強制 (§6)
- ❌ query ファイル (`adapter/repository/queries/*.sql`) に DDL を書く → migrations にだけ

---

## 12. 関連 docs

- [ARCHITECTURE.md](../ARCHITECTURE.md)
- [DATA_MODEL.md](../runtime/DATA_MODEL.md) — 各テーブル詳細
- [STATE_MACHINE.md](../runtime/STATE_MACHINE.md) — position state ledger
- [layers/adapter.md](../architecture/layers/adapter.md) — repository adapter の更新
- [layers/port.md](../architecture/layers/port.md) — 境界 record の更新
- [layers/usecase.md](../architecture/layers/usecase.md) — interface 仕様
- [PR_CHECKLIST.md](../architecture/PR_CHECKLIST.md) — schema 変更時のチェック
