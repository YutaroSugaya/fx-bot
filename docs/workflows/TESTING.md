# Testing — strict t_wada 流 TDD (Red-Green-Refactor) + 古典派 + table-driven + integration tag

fx-bot のテスト戦略。strict R-G-R cycle (Refactor 段の省略禁止) と古典派 mock 方針を
両輪とする。既存の usecase test は real collaborator で書かれており、新規 test にも同じ規範を適用する。

---

## 1. 層ごとのテスト種別

| 層 | テスト種別 | mock / fake の使い方 |
|---|---|---|
| Domain | 純粋単体 | 不要。値の入出力だけ |
| Usecase (Command/Query) | 単体 | port 配下の interface を fake で差し替え |
| Adapter (Repository) | 統合 | 実 DB (Docker / Postgres コンテナ) |
| Adapter (Broker GMO) | 録画再生 | httptest server で response を返す |
| Handler/Controller | 単体 | usecase を fake で差し替え |
| Server (e2e) | 統合 | httptest + 実 DB (任意。クリティカルパスのみ) |

---

## 2. 古典派 (Classical / Detroit / Chicago) TDD

**極力 mock や stub は使わず、実装をそのまま動かして検証する。**

### fake/mock の使用は以下 3 用途に限定

1. **システム境界** (= プロセス外部) を切り出すとき
   - 実 DB に繋げないユニットテストでは Postgres を [InMemory*Repo](../../backend/internal/backtest/inmem_repos.go) で代用
   - GMO API は `httptest.NewServer` で fixture response を返す
   - Claude CLI は `PromptRunner func(ctx, prompt) (yaml, err)` を関数注入
2. **特定の失敗シナリオを注入したいとき**
   - ResolveExecution timeout、DB エラー、cancel 失敗、race condition 等
   - その目的に特化した最小限の fake を使う
3. **時刻・乱数など非決定性を排除したいとき**
   - `time.Now` を `Clock func() time.Time` で注入する
   - SignalID 生成は `strategy.Engine.SignalIDFn` を caller (wiring 層) から注入する

### 禁止

動作する `usecase.command.X` がある実装を test 用 `mockX` で置き換えること。

Command/Query は **PaperBroker や real Closer (with InMemoryRepo) を組み合わせて
end-to-end に近い形でテスト** する。これにより、リファクタで内部実装が変わっても
テストは挙動を継続的に保証する (= 仕様ベース)。

### 理由

- mock 過多のテストは **テスト自身が二度書き** となり、リファクタ抵抗が高まる
- 実装と乖離した mock は **嘘の安全感** を生む (mock は通るが実装は壊れている)
- usecase の **本物の協調** を検証できないと、結合不具合 (usecase 間の受け渡しのズレ) を見逃す

---

## 3. テーブル駆動テスト (Table-Driven)

**同じ動作軸で複数ケースを検証するときは必ず table-driven にする。**

```go
func TestComputeTPSLPrices(t *testing.T) {
    cases := []struct {
        name              string
        side              order.Side
        entry, tp, sl, pip float64
        wantTP, wantSL    float64
    }{
        {"BUY",  order.SideBuy,  100.00, 20, 15, 0.01, 100.20, 99.85},
        {"SELL", order.SideSell, 100.00, 20, 15, 0.01,  99.80, 100.15},
        {"zero pips returns entry", order.SideBuy, 100.00, 0, 0, 0.01, 100.00, 100.00},
    }
    for _, tc := range cases {
        t.Run(tc.name, func(t *testing.T) {
            tp, sl := ComputeTPSLPrices(tc.side, tc.entry, tc.tp, tc.sl, tc.pip)
            // ...
        })
    }
}
```

### ルール

- ケース名は短く目的を示す (e.g. `"buy"`, `"sl_negative_pips_returns_error"`)
- `t.Run(tc.name, ...)` で必ずサブテスト化 (-run でケース単独実行可能に)
- 失敗時のメッセージに `tc.name` か入力値を含める (どのケースが落ちたか即わかる)
- ケース数が増えると table 列も増える → **列が 8 を超えるならテストを分割**

### 例外

- 完全に異なるセットアップが必要なケース (e.g. `mode=live` vs `paper`) は別の `Test...` 関数
- 1 ケースしかない検証は table にしない

---

## 4. テスト命名

```
TestClosePositionCommand_HappyPath
TestClosePositionCommand_RaceCondition_TripsEmergency
TestClosePositionCommand_BrokerError_DoesNotInsertTrade
```

`<Type>_<Scenario>_<ExpectedBehavior>` の 3 要素。
table-driven のサブテストは ケース名を `_` 区切り (`/` は go test runner が階層として扱う)。

---

## 5. strict t_wada 流 TDD: Red → Green → Refactor (3 段すべて必須)

> **Refactor 段の省略は禁止**。本リポジトリでは Refactor は cycle の 1/3 の比重。
> 「ついでに refactor もしておきました」ではなく **cycle の中で必ず通る関門**。

### 5.1 Red — 失敗するテストを先に書く

- 期待挙動を assertion で表現する。
- **compile error / assertion error で「実際に失敗していること」を目視確認** してから
  Green に進む。「書いた瞬間に通ってしまった」テストは Red になっていない (= 既存挙動を
  単に観測しているだけで仕様駆動になっていない) ので、テストを inversion させて
  本当に落ちることを確認する。

### 5.2 Green — テストを通すだけの最小コードを書く

- 設計や命名は二の次。**重複 OK、命名雑 OK、関数長い OK**。とにかく green を作る。
- 「ここで設計を整えたい」誘惑は Refactor に回す。Green は仮置きで良い。

### 5.3 Refactor — テストを green に保ったまま設計を整える (= **必須**)

Refactor 段では最低限以下を点検する:

- **命名見直し** (Green で雑に命名した変数 / 関数を意図が伝わる名前に)
- **重複削除** (Green で許した duplication を helper / 関数抽出で集約)
- **関数抽出 / インライン化** (1 関数 1 責務に近づける)
- **SOLID 違反の解消** (特に SRP / DIP のズレ)
- **mock の必要性再評価** — Green で fake を増やしたなら、それが
  [§2 古典派 3 用途](#2-古典派-classical--detroit--chicago-tdd) のどれに該当するかを
  自問する。該当しないなら **real な実装** (`backtest.InMemory*Repo` /
  `broker.PaperBroker` 等) に置換する。

**Refactor 完了の判定基準**: 「次に書きたいテスト / コードがすぐ書ける状態」になったら
完了。逆に「次に何書くか考えるのに脳内で構造を組み立て直さないといけない」なら
Refactor が足りていない。

### 5.4 PR / commit 単位の規律

- **Refactor を skip した PR は不可**。
- PR description に **Refactor 段階で何をしたか** を 1 項目以上書く
  (= 命名変更 / 関数抽出 / 重複削除 / mock 置換 のいずれか)。
- Refactor が「特に無し」になりそうなら、それは Green が雑でなかった or
  本当に小さい変更ということ。後者なら問題ないが、前者は Red を強化して再 cycle。

### 5.5 新規コードへの適用範囲

- 新規 usecase / domain service / port adapter 追加時は **必ず strict R-G-R**。
- 既存コードへの bug fix も同じ — まず regression test を Red で書き、Green で fix、
  Refactor で関連 callsite / helper の整理を行う。
- adapter (録画再生) は §2 の §1 用途 (システム境界) なので Refactor 段では
  「fixture が現行 API spec と乖離していないか」を確認する。

---

## 6. Integration test (build tag `integration`)

Postgres / GMO 等の外部依存を持つテストは build tag `integration` で分離する:

```go
//go:build integration

package repository

func TestPositionCloser_AtomicTxRollsBackOnInsertError(t *testing.T) {
    dsn := os.Getenv("INTEGRATION_TEST_DB_URL")
    if dsn == "" {
        t.Skip("INTEGRATION_TEST_DB_URL not set")
    }
    // ... open real pool, exec tests
}
```

### 実行コマンド

```bash
make db-up                                                       # postgres を起動
docker exec fxbot-postgres createdb -U fxbot fxbot_test          # 初回のみ: テスト専用 DB を作成
export INTEGRATION_TEST_DB_URL='postgres://fxbot:fxbot@localhost:5432/fxbot_test?sslmode=disable'
(cd backend && DATABASE_URL="$INTEGRATION_TEST_DB_URL" go run ./cmd/migrate up)   # schema を fxbot_test に当てる
make test-integration                                            # adapter/ 配下のみ実行
```

> `DATABASE_URL=… make migrate-up` のように env で上書きしても、make は `.env` の `DATABASE_URL` を優先する
> (`-include .env` で Makefile の変数になるため)。test DB に migration を当てるときは上のように `cmd/migrate` を直接呼ぶか、
> `make migrate-up DATABASE_URL="$INTEGRATION_TEST_DB_URL"` (コマンドライン引数) にする。

通常の `make test` は `integration` タグを付けないので **これらのテストはスキップされる** (CI で Docker が無くても安定して通る)。

### ⚠️ 本番 DB を使わない

`INTEGRATION_TEST_DB_URL` には **必ず本番 DB と別の DB** を指定する。Integration test
suite は `truncateAll` で `positions / trades / strategy_configs / candles` ほぼ全テーブルを
`TRUNCATE ... CASCADE` するため、間違って本番 DB (`DATABASE_URL`) に向けた状態で実行すると
**取引履歴 / 戦略設定 / ポジション台帳 が消える**。broker 側の建玉は残っても、bot DB は
active config の seed や外部建玉の再採用からやり直しになる。

ルール:
- **DB 名は接尾辞 `_test` を必ず付ける** (例: `fxbot_test`)。`_test` で終わらない DSN は **Makefile が実行を拒否**
- `INTEGRATION_TEST_DB_URL` と `DATABASE_URL` が **同じ値の場合も、Makefile は実行を拒否** ([test-integration target](../../Makefile) のガード)
- 未設定なら Makefile は WARN ログを出して **skip** (テストは個別に `t.Skip` するので失敗にはならない)
- integration タグ付きの `go test` を直接打たない (Makefile のガードを通らない)。型チェックだけなら `cd backend && go vet -tags integration ./...`

### Stop hook での扱い

`.claude/hooks/pre-stop-checks.sh` は DB 関連変更時 (`backend/migrations/`, `backend/internal/adapter/repository/`,
`backend/internal/port/repository.go`, `backend/cmd/migrate/`) に `make test-integration` を自動実行する。
hook は `.env` ではなく自分の環境変数を見るので、`INTEGRATION_TEST_DB_URL` は Claude Code を起動するシェルで export するか
`.claude/settings.local.json` の `env` に書く。未設定なら Stop hook はブロックする (同じセッションで 3 回続けてブロックしたあとは、
警告を残して通す。CI と pre-push は integration テストを回さない)。

DB 変更を含む作業では、実装完了前に上の「実行コマンド」(test DB の作成・migration・`make test-integration`) を済ませておく。

### 現在の integration ファイル

- `backend/internal/adapter/repository/*_integration_test.go` (共通 helper は `testdb_integration_test.go`。
  promoter / position closer / close_reason CHECK / position repo の各列など)
- `backend/internal/adapter/broker/gmo_fx_integration_test.go`

---

## 7. Race detector (`-race`)

`go test -race` を **必須** で通す。

- mutex を取らずに共有 state を書き換えていないか
- channel / goroutine の競合がないか
- atomic を使うべき counter を mutex 化していないか

`make test` は race を有効にする。CI でも `go test -race -count=1 ./...` を回す ([PR_CHECKLIST.md](../architecture/PR_CHECKLIST.md))。

---

## 8. Coverage

`make test-cover` で `coverage.out` 出力 + `go tool cover -html` で可視化。

特定の閾値 (例: 70%) は強制しない。代わりに **PR レビューでカバレッジが不足しているクリティカルパスを指摘** する。

---

## 9. 一時的に skip するテスト

`t.Skip(reason)` を使うとき:

- skip 理由を必ず文字列で書く (`t.Skip("INTEGRATION_TEST_DB_URL not set")`)
- 環境依存スキップ以外は `// TODO: ` コメントで再開条件を書く
- `t.SkipNow()` の単独使用は禁止 (理由不明)

---

## 10. アンチパターン

- ❌ `usecase` のテストで全 dependency を `mockBroker` / `mockRepo` / `mockNotifier` で書いた
- ❌ table case を 10 ケース以上 + 列を 12 列にして読めなくなった
- ❌ `t.Helper()` を使わずに helper 内で `t.Fatalf` した (失敗時のスタックトレースが helper 行になる)
- ❌ race 関連バグを「テスト不安定」として `t.Skip` した (= 根本原因を直す)
- ❌ **Red 段階を飛ばして Green から書き始めた** (= テストが既存挙動を観測しているだけで仕様駆動になっていない)
- ❌ **Refactor 段を skip した** ("Green で通ったから OK" は cycle 不完全。次の cycle で技術的負債を踏む)
- ❌ Green 段階で増やした fake を、Refactor 段階で「mock 必要性の再評価」に
  かけずにそのまま PR に出した (= §2 古典派 3 用途のどれにも当てはまらない fake が残る)

---

## 11. 関連 docs

- [ARCHITECTURE.md](../ARCHITECTURE.md)
- [layers/usecase.md](../architecture/layers/usecase.md) — usecase テストの fake 選択
- [layers/domain.md](../architecture/layers/domain.md) — 純粋単体テスト
- [layers/adapter.md](../architecture/layers/adapter.md) — integration + 録画再生
- [layers/handler.md](../architecture/layers/handler.md) — httptest
- [PR_CHECKLIST.md](../architecture/PR_CHECKLIST.md) — merge 前 race / coverage チェック
