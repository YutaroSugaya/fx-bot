# Backtest — データ準備 / Engine 前提 / コストモデル / 判定

backtest の手順と前提条件をまとめた。
実装は [backend/internal/backtest/](../../backend/internal/backtest/)、CLI は
[cmd/backtest](../../backend/cmd/backtest/main.go) / [cmd/sweep](../../backend/cmd/sweep/main.go) /
[cmd/edge-judge](../../backend/cmd/edge-judge/main.go) / [cmd/histdata-ingest](../../backend/cmd/histdata-ingest/main.go)。
Go module は `backend/` にあるので、以下の `go run` はすべて `backend/` で実行する。

---

## 1. 何を backtest するか

| Mode | 入力 | 検証対象 |
|---|---|---|
| Mode A (実装済み) | 1m candles + 固定 strategy_config 1 本 | strategy 単体の PnL / 期待値 |
| Mode B (未実装) | candles + `market_summaries` + advisor outputs | advisor を含む bot 全体の挙動 |

Mode A の engine は live と同じ `domain/strategy` の戦略コードを 1m bar ごとに評価する。シミュレートしないもの:

- **risk Gate** (日次損失 cap / 連敗 / cooldown / max_open_positions) — engine は 1 symbol につき同時 1 ポジションだけ持つ
- **LLM** (advisor / LLM 決定ループ)。LLM 判断に依存する経路は backtest できない
- DB への書込 (CLI は candles を読むだけ)

---

## 2. データ準備 — 隔離 backtest DB

長期履歴の backtest は live の `fxbot` DB ではなく、同じ postgres インスタンス内の **隔離 DB `fxbot_backtest`** に対して回す。
live DB を backtest データの書込先にしない。

```bash
# 1) HistData.com の無料 M1 CSV (USDJPY/EURUSD/EURJPY/GBPJPY/GBPUSD × 2015-2023) を data/histdata/ へ取得 (repo root で)
#    スクリプトは HistData の無料ダウンロードフォームを自動で叩く。先に HistData の利用規約を確認し、
#    自動取得が許されないなら同サイトから手で落として data/histdata/ に置く。データは再配布しない (data/ は gitignore)
bash scripts/fetch_histdata.sh

# 2) fxbot_backtest DB を作って schema を migrate (fxbot-postgres コンテナ内。live の fxbot DB には触れない)
bash scripts/provision_backtest_db.sh
export BACKTEST_DATABASE_URL='postgres://fxbot:<password>@localhost:5432/fxbot_backtest?sslmode=disable'   # スクリプトが表示する値。.env に書いても go run には渡らないのでシェルで export する

# 3) 取り込み (backend/ で)。既定は dry-run = DB に接続せず parse と範囲チェックだけ
cd backend
go run ./cmd/histdata-ingest -dir ../data/histdata
go run ./cmd/histdata-ingest -dir ../data/histdata -dsn "$BACKTEST_DATABASE_URL" -apply
#   空の candles への初回ロードは -copy (COPY 一括 INSERT・重複処理なし) が速い。追記は -copy 無しの upsert
```

- `-apply` は `repository.SafeBacktestDSN` を通らないと拒否する: DSN の DB 名が `_backtest` で終わり、かつ `DATABASE_URL` と異なること。
- ファイル名 `DAT_ASCII_<PAIR>_M1_<period>.csv` から symbol を決める (`-symbol` で上書き可)。HistData の時刻 (EST・DST なし) は UTC に正規化される。
- 直近の期間は GMO public klines から `go run ./cmd/fetch-candles -symbol USD_JPY -from YYYY-MM-DD -to YYYY-MM-DD` でも取れる
  (`DATABASE_URL` の DB の `candles` に upsert する。GMO 外為 API の履歴は HistData より短い)。

---

## 3. 実行コマンド (`cmd/backtest`)

`cmd/backtest` は `DATABASE_URL` の DB から 1m candles を読む。backtest DB を読ませるには `DATABASE_URL` をその DSN にする。

> ⚠️ **コスト系フラグの既定値はすべて 0 = 摩擦ゼロの backtest**。`-slippage` / `-fee-rate` / `-spread-*` を指定しないと
> スプレッドも手数料も引かれず、実運用では負ける戦略が勝って見える。結果を判断に使う run では必ずコストを入れる。

```bash
cd backend
DATABASE_URL="$BACKTEST_DATABASE_URL" go run ./cmd/backtest \
    -config ../configs/probe_trend_follow_USD_JPY.yaml \
    -symbol USD_JPY -from 2015-01-01 -to 2024-01-01 \
    -fee-rate 0.002 \
    -spread-median 0.5 -spread-tokyo-spike 9.5 \
    -slice both \
    -pnl-out /tmp/bt_pnl.json
```

| フラグ | 意味 | 既定 |
|---|---|---|
| `-config` | strategy config YAML (必須。`-batch` で指定も可) | — |
| `-symbol` / `-symbols` | 単一 symbol / CSV で複数 symbol | `USD_JPY` |
| `-from` / `-to` | 期間 `YYYY-MM-DD` (UTC、`-to` は exclusive) | 必須 |
| `-slippage` | 片道 (leg) ごとの不利方向 slippage pips | 0 |
| `-fee` | 1 trade あたり固定手数料 (円) | 0 |
| `-fee-rate` | 約定金額 × rate% の往復手数料 (GMO 公表値は `0.002` = 0.002%) | 0 |
| `-spread-median` / `-spread-tokyo-spike` | 時間帯別スプレッドモデル (平常の中央値 + 05-08 JST の上乗せ)。半スプレッドを leg ごとに加算 | 0 (off) |
| `-spread-file` | 較正済み時間帯別スプレッド YAML (`cmd/spread-calibrate` が `market_summaries` から生成。`market_summaries` は advisor が動いた分しか溜まらないので、advisor を使っていなければ作れない)。ファイルにある symbol はフラグのモデルを上書き | — |
| `-swap-table` | スワップ表 YAML (`swap_table: {USD_JPY: {BUY: …, SELL: …}}` = 1,000 通貨あたり円/晩、21:00 UTC 跨ぎで計上、水曜 3 倍) | off |
| `-replay-tf` | `1m` / `1d` (日足戦略は 1m を日足に resample して評価) | `1m` |
| `-slice` | `hour` / `weekday` / `both` — JST の時間帯別・曜日別の内訳を追加表示 (pretty のみ) | — |
| `-format` | `pretty` / `json` | `pretty` |
| `-pnl-out` | 取引ごとの PnL (円・コスト控除後) を JSON 配列で書き出す (`cmd/edge-judge -json` の入力)。単一 symbol run のみ | — |
| `-batch` | symbols / period / config / slippage / fee をまとめた YAML。明示した CLI フラグが優先 | — |

出力は標準出力。pretty は SampleSize / ProfitFactor / Expectancy / MaxDrawdown / WinRate / AmbiguousBars と先頭 10 trade、
最後にコスト設定を表示する。複数 symbol では symbol 別ブロック + 口座合算 (時系列 merge の DD・equity) を出す。

**コストの目安**: USD/JPY 1,000 通貨・150 円付近なら、手数料 0.002% × 往復 ≈ 0.6 pips + スプレッド ≈ 0.5 pips で、
往復 ≈ 1.1 pips が最低限のコスト床。東京早朝 (05-08 JST) はスプレッドが大きく開くので `-spread-tokyo-spike` か `-spread-file` で再現する。

---

## 4. Engine の前提

[backend/internal/backtest/engine.go](../../backend/internal/backtest/engine.go):

### Bar の扱い

- シグナル評価の「今」= bar close (`OpenTime + 1m`)。戦略に渡す history は現在 bar を含まない
- entry は signal bar の close で約定 (+ 不利方向の slip)
- exit の判定は **次の bar から** (entry bar の中で TP に触れていても無視 = look-ahead 防止)
- TP / SL の到達判定は bar の High / Low

### Same-bar policy

同一 bar 内で TP / SL の両方に触れた場合は `ConflictPolicy` で決める。既定は **PessimisticSLFirst (SL 優先 = 最悪ケース)**。
`OptimisticTPFirst` は感度分析用、`SkipAmbiguous` は次 bar へ持ち越す。該当 bar 数は `AmbiguousBars` に出る。

### コストの適用

- leg ごとの slip = `-slippage` + (スプレッドモデルがあれば) その時刻のスプレッド ÷ 2
- entry: BUY は `+slip`、SELL は `-slip`。exit も同じく不利方向へずらす
- 例: BUY entry 100.000、slip 0.5 pips (pip=0.01) → 100.005 で約定とみなす
- 円 PnL から `-fee` と `-fee-rate` の往復手数料を引き、スワップを足す。`ProfitLossJPY` はコスト控除後 (net)

### Ratchet 出口

Signal の `ratchet_arm_pips` / `ratchet_giveback_pips` を読み、peak (前 bar までの favorable excursion) から giveback だけ戻ると
`ratchet_takeprofit` で close する。**bar 解像度 + 悲観モデル** (この bar の高値で peak を更新する前に戻り判定し、trailing stop の価格で約定)。
GMO OCO は trailing 非対応なので、bot プロセスが生きている前提の出口を再現している (下方バイアスあり)。arm/give 0 = OFF。

### History の上限

各 bar で戦略に渡す 1m history は直近 `maxHistoryBars=20000` (~14 日) に cap し、replay を O(n) にしている
(1h 200SMA の傾き ≈ 13,200 本の 1m を内包)。`-replay-tf 1d` では日足 300 本。

### その他の落とし穴

- ⚠️ **config の `valid_from` / `valid_until` の外は 0 trade** (`IsActive` 判定で `config_not_active`)。過去期間を回すときは
  validity を backtest 期間に掛かるようにした config を使う (例: `configs/probe_trend_follow_*.yaml` / `probe_daily_trend_*.yaml` は
  `valid_from: 2014-01-01`。`probe_gotobi_*` / `probe_london_breakout_*` は 2023 始まりなので、それより前を回すなら validity を広げた複製を作る)。
  `cmd/sweep` は自動で validity を広げた複製を使う (`CloneForOffline`)。
- 数量は Signal の quantity → config の `quantity` → 1,000 の順で決まる。
- USD-quote ペア (EUR_USD 等) の円 PnL は `cmd/backtest` では quote 通貨 (USD) のまま。pips で見るか、`cmd/sweep` の
  `costs.assumed_usdjpy_rate` を使う。
- candles の取得上限は 5,000,000 行/symbol。超える期間はエラーで止まる (古い bar が黙って落ちるのを防ぐ)。

---

## 5. Walk-forward スイープ (`cmd/sweep`)

パラメータをグリッドで振り、train / OOS / holdout の 3 分割で検定する。現状のグリッドは `ma_pullback` のパラメータ
(`strategy.MAPullbackParams`、例: `ratchet_arm_pips` / `sl_min_pips` / `tp_cap_pips`) が対象。

```bash
cd backend
DATABASE_URL_RO="$BACKTEST_DATABASE_URL" go run ./cmd/sweep -config ../path/to/sweep.yaml
```

sweep YAML: `strategy_config` (または symbol 別 `strategy_configs`)・`symbols`・`grid` (キー → 値リスト)・`splits`
(`train_from` … `holdout_to`。省略時は `DefaultSplits`)・`costs` (`slippage_pips` / `fee_rate_pct` / `spread_median` /
`spread_tokyo_spike` / `spread_file` / `swap_table` / `assumed_usdjpy_rate`)・`top_oos`。

- 探索は train のみ。train でコスト後 PF < 1 の combo は即棄却し、上位 `-top` 件だけが OOS を 1 回見る
- holdout は `-unlock-holdout` を付けたときだけ開封する (1 回だけの最終試験。見てから再調整したら実験は無効)
- 試行総数を出力する。最良 combo の隣接値 (各次元 ±1 step) も勝っているか (プラトー) を報告する
- sweep / edge-judge は `DATABASE_URL_RO` を `DATABASE_URL` より優先する。backtest DB を読ませるときは `DATABASE_URL_RO` に backtest DSN を渡す
- config も DB も書かない。勝った combo を使うなら新しい `config_id` の config を作り、[CONFIG.md §4.1](../runtime/CONFIG.md) の手順で seed する

---

## 6. 判定 (`cmd/edge-judge`)

コスト控除後の取引ごと PnL 系列に、事前に固定した 3-way 判定 ([internal/analysis/judge.go](../../backend/internal/analysis/judge.go)) を掛ける。

```bash
cd backend
go run ./cmd/edge-judge -json /tmp/bt_pnl.json                  # backtest の -pnl-out
go run ./cmd/edge-judge -config-id <config_id>                 # DB の trades (SELECT のみ。DATABASE_URL_RO 優先)
go run ./cmd/edge-judge -json /tmp/bt_pnl.json -dsr-trials 40 -dsr-sr-variance 0.01   # 多重検定の割引も表示
```

| verdict | 条件 |
|---|---|
| `reject` | 平均 PnL の bootstrap 95% CI 上限 < 0 (エッジ無しを高信頼で棄却) |
| `promote_candidate` | CI 下限 > 0 かつ PF ≥ 1.1 (合格の目安。プラトー確認は sweep 側) |
| `continue` | それ以外 (判定不能 = 合格ではない。計測を続ける) |

信頼水準 0.95 と PF 下限 1.1 は定数でフラグ化しない (後から動かせると事前登録の意味がない)。`-resamples` (既定 10000) と
`-seed` (既定 42) で再現できる。`-dsr-trials` を渡すと Sharpe / PSR / Deflated Sharpe も出す。最後の 1 行は JSON。

---

## 7. Metrics

[backend/internal/backtest/metrics.go](../../backend/internal/backtest/metrics.go) の `Metrics` struct (すべて net 円 PnL ベース):

- **SampleSize** / **WinRate** (PnL = 0 は負け扱い) / **ProfitFactor** (負けが無く勝ちがあれば +Inf)
- **Expectancy** (1 trade あたり平均)
- **MaxDrawdown** (累積 PnL の peak からの最大下げ幅)

PF の +Inf は JSON 出力で扱いにくいので、判定には `-pnl-out` → `edge-judge` を使う。
`-slice` の内訳で n < 5 のバケットには `(n<5)` が付く (少数サンプルの PF を信用しない)。

---

## 8. InMemoryRepo (テスト用 fake)

[backend/internal/backtest/inmem_repos.go](../../backend/internal/backtest/inmem_repos.go):

`port.PositionRepository` / `port.TradeRepository` / `port.SignalRejectionRepository` の **in-memory 実装**。
Postgres を起動せずに usecase をテストするための fake として使う ([TESTING.md](TESTING.md) §2)。backtest engine 自体は使わない。

---

## 9. アンチパターン

- ❌ コストを入れずに (= 既定の 0 のまま) 回して「PF が高い」と判断する
- ❌ candle データが不足したまま backtest を回す (= 期間の一部が無トレードになる。`valid_from` 外の 0 trade も同様)
- ❌ same-bar policy を TP 優先にして結果を採用する (= 実 broker では SL が先に発火する可能性が高い)
- ❌ OOS / holdout を見てからパラメータを直し、同じ区間で再評価する
- ❌ 試行回数を数えずに最良の 1 combo だけを報告する
- ❌ 古いバージョンの strategy_config で backtest した結果を新バージョン strategy で運用する根拠にする
- ❌ backtest の書込先 (`histdata-ingest -apply`) に live DB を指定する (`SafeBacktestDSN` が拒否する)

---

## 10. 関連 docs

- [ARCHITECTURE.md](../ARCHITECTURE.md)
- [DATA_MODEL.md](../runtime/DATA_MODEL.md) — candles / market_summaries テーブル
- [CONFIG.md](../runtime/CONFIG.md) — strategy config の構造と seed
- [SYSTEM_DESIGN.md](../runtime/SYSTEM_DESIGN.md) §11 — どこに何があるか
- [layers/domain.md](../architecture/layers/domain.md) — strategy / risk gate (backtest で検証する対象)
- [TESTING.md](TESTING.md) — InMemoryRepo を fake として使うパターン
