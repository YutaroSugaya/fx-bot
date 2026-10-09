# fx-bot — FX 自動売買 bot(GMO コイン外国為替FX API)

> **English.** A Go trading bot for spot FX on the GMO Coin FX API, with a Next.js dashboard. It is built around
> a few engineering decisions: an LLM (Claude Code CLI) may only *propose* — a strategy config, or a trade / no-trade
> call with TP/SL — while deterministic code decides (schema and hard-limit validation, a risk gate, vetoes, loss caps);
> every live TP/SL sits on the broker as an OCO order; anything unverified fails closed; the broker is the source of
> truth and the ledger is reconciled against it. Strategies were screened offline on multi-year 1-minute data with
> costs, then run forward with frozen parameters at the minimum lot, and finally an LLM decision loop was run forward
> under pre-registered pass/fail rules. None of them showed an edge net of the ≈1.1 pip round-trip cost, so the system
> was never scaled. Docs and comments are in Japanese. MIT licensed. Not financial advice.

## これは何か(30 秒)

- GMO コイン外国為替FX の API で売買する bot。paper(実ティッカー × bot 内の約定シミュレーション・口座不要)と live(実弾)の 2 モード、ローカルのダッシュボード、オフラインの backtest / エッジ判定ハーネスを持つ。
- 売買判断の一部を LLM(Claude Code CLI の `claude -p`)に任せられるが、LLM ができるのは**提案まで**。発注してよいかはコード(スキーマ検証・hard limits・risk Gate・veto)が決め、守り(TP/SL)は broker 側に置く。
- 作ったものと同じくらい、**どう検証したか**に重きを置いた。backtest のスクリーン → パラメータを凍結した forward → LLM 判断ループの forward(判定基準を事前登録)の順で進め、判定はコード(`cmd/edge-judge`)が出す。
- 結論は「どの段でも、往復コスト(約 1.1 pips)込みのエッジを示せなかった」。だからロットは最小のまま拡大していない。

技術: Go(`pgx` / `yaml.v3` / `x/sync` のみ)、PostgreSQL 16(Docker)、Next.js 16 + React 18 + lightweight-charts、Claude Code CLI(任意)、Claude Code の hooks。テストは `go test -race`・vitest・integration(実 Postgres)・hook の自己テストまで。

## 設計のポイント(なぜそうしたか)

### 1. LLM は提案、決めるのはコード
- LLM の経路は 3 つあり、どれも既定 off(tracked の `configs/bot_config.yaml` で無効。`tracked_bot_config_test.go` が固定)。LLM が決められる範囲は経路ごとに狭く切ってある:
  - **advisor**(`ai_advisor`): 戦略 config の YAML を生成するだけ。スキーマ・hard limits・リスク状態・戦略 whitelist の検証を通ったものだけが active になり、売買そのものは決定論のエンジンが tick ごとに判定する(`promote_config_test.go`)。
  - **LLM 判断ループ**(`llm_decision`): 決められるのは go / side / TP・SL の pips だけ。数量・保有上限・ratchet は config、建値は実際の bid / ask。返答が読めなければ no_trade(`TestParseLLMDecision_FailSafe`)。
  - **advisor v2**(`advisor_v2`): 決定論の検出器が出した候補に go / no-go を返すだけ。SL / TP は検出器の幾何で、LLM は触れない。
- 全経路が同じ `ExecuteOrder.OnSignal` を通る: admission(mutex の下で DB を読み直し、緊急停止・日次損失・ナンピン禁止(外部建玉も数える)・連敗・クールダウン・スプレッドを検査)→ `ValidateSignalBoundaries`(実際の Signal 値で数量・SL/TP の上限・1 トレードの最悪損失を検査)→ 発注。LLM 経路は override できない。手動発注の override でも、緊急停止・日次損失・ナンピン禁止・再エントリーの冷却は越えられない(`entry_admission_pinning_test.go`、`hard_safety_never_overridable_test.go`、`entry_admission_no_nanpin_override_test.go`、`signal_boundary_test.go`、`gate_pyramiding_test.go`)。
- ダッシュボードの「Claude に聞く」は tool を全部外して呼ぶ(`TestNewClaudeRunner_DisablesAllTools`)。発注には届かない。
- bot が起動する claude には、broker の API キー・DB の DSN・ダッシュボードの認証を環境変数で渡さず、リポジトリの Claude Code hooks も無効にして呼ぶ(`claude_isolation_test.go`、`TestNewClaudeRunner_IsolatesEnvAndHooks`)。
- 理由: 再現性(同じ入力なら同じ発注)、監査(なぜ建てたかを config と判断ジャーナルで説明できる)、コスト(毎 tick で LLM を呼ばない)。

### 2. 守りは broker 側に置く
- live は成行で建て、約定照会で実際の positionId と建値を取り、その建値から計算した TP / SL を **GMO 側の OCO** として置く。bot のプロセスが死んでも TP / SL は効く。
- OCO が置けなければ成行で建玉を畳む(補償)。それも失敗したら、あるいは約定照会自体が失敗したら緊急停止(`live_exit_protector_test.go`)。
- live では bot は TP / SL を自分で監視しない(`TestEvaluateExit_LiveMode_TPHit_SkippedReturnsEmpty`)。bot 側の出口(保有上限・ratchet・朝の一斉手仕舞い)は、決済前に OCO の脚を取り消し、broker に決済を拒否されたら OCO を置き直す(`close_saga_rearm_test.go`)。

### 3. fail-close
- **live は三重ロック**: `bot.mode: live_config` + `LIVE_TRADING_ENABLED=true` + `LIVE_CONFIRM_SYMBOLS` が bot_config の symbols と集合として一致。どれかが欠けると paper に降格する。API キーが無ければ起動しない(`TestCheckLiveModeFlags*`)。tracked の bot_config は常に paper(`TestTrackedBotConfigIsNotLiveMode`)。
- 起動時: symbols は `hard_limits.allowed_symbols` の部分集合でなければ起動しない。live では active config が検証に落ちる、または起動時の reconcile が失敗すると起動しない。
- API: `/healthz` 以外の全 `/api`(参照の GET も)に BasicAuth。パスワードが `.env.example` の例の値のままなら起動しない。認証を外せるのは loopback に bind したときだけで、それ以外は起動を拒否する(`TestAPIServer_Run_RefusesUnauthenticatedNonLoopbackBind`)。cross-site な POST は拒否(CSRF)、CORS は allowlist。loopback bind では Host ヘッダが localhost / 127.0.0.1 / [::1](と `DASHBOARD_ALLOWED_HOSTS`)以外のリクエストを API とダッシュボードの両方で拒否する(DNS rebinding 対策・`api_server_host_test.go`、`frontend/app/lib/host_guard.test.ts`)。
- 緊急停止は `runtime/emergency_stop.flag` というファイル 1 つ。新規エントリーだけを止め、既存建玉の出口と broker の OCO は動き続ける(自動では畳まない。畳む判断は人間)。LLM 判断ループはサイクルの冒頭でこれを見て、LLM を呼ばずに止まる。

### 4. broker が正(reconcile)
- 定期的に broker の建玉と DB を突き合わせる。DB に無い建玉は「外部建玉」として表示だけ(bot は管理しないが、同方向の新規は止める)。broker に無い DB 建玉は約定照会で実際の決済を探し、分からなければ猶予の後に緊急停止。live では架空の 0 円決済を記帳しない(`reconcile_no_synthetic_zero_pnl_test.go`)。
- 決済は DB で OPEN → CLOSING を CAS で取ってから broker を叩く(二重決済の防止)。
- 建玉時に config_id / TP / SL / 保有上限 / ratchet を positions 行に凍結する。出口の判定はその行だけを読むので、後から config を切り替えても既存の建玉には効かない。

### 5. 不変条件をテストで固定する
- 破滅を防ぐ上限(1 トレードの損失上限 ¥8,000 以下・SL の上限 50 pips 以下・数量 10 万通貨以下)を `configs/hard_limits.yaml` で緩めるとテストが落ちる(`catastrophe_guards_test.go`)。tracked の bot_config が paper で、LLM の定期呼び出しが off で、損失 cap が厳しいことも同様(`tracked_bot_config_test.go`)。
- ヘキサゴナル 4 層 + CQRS。`domain` は純粋(`pgx` / `net/http` / `slog` を import しない)。層規約は [docs/architecture/layers/](docs/architecture/layers/)。
- strict TDD(Red → Green → Refactor)。マージゲートは `go test -race` + `vet` + `build` と frontend の test / build。

### 6. AI エージェントで書く前提のガードレール
- コードの大半は Claude Code で書いた。だから「AI に壊させない」仕組みを同梱している: 危険な操作(integration テストの実行・DB の破壊・postgres / bot の停止・ハーネス自身の書換え)を PreToolUse hook で拒否し、Stop 時に secret scan・層規約・migration 規約・`make check-backend` を強制する。commit と push でも秘密情報を検査する。
- hook とスキャナには自己テストがある(`bash .claude/hooks/pretooluse-deny_test.sh`、`bash scripts/secret-scan_test.sh`)。

## 検証の進め方と結論

| 段階 | 何をするか | 落とす基準 |
|---|---|---|
| backtest のスクリーン | HistData の 1 分足(複数年・複数レジーム)を別 DB に入れ、`cmd/backtest` で決定論リプレイ。スプレッド・手数料・スリッページ・スワップを計上する。`cmd/sweep` は walk-forward(train → OOS は 1 回だけ → holdout は鍵付き) | コスト込みで負け / OOS で符号が変わる / パラメータを隣に動かすと崩れる |
| 凍結した機械戦略の forward | config を DB に入れたら触らずに、最小ロット(1,000 通貨)で回す | 事前に決めた件数で `cmd/edge-judge` が reject / continue を出す |
| LLM 判断ループの forward | LLM がチェックリスト(playbook)を数値で判定し、veto と OCO はコード。条件を凍結し、判定基準(件数または期限・1 件あたり net > 0)を事前登録する | 基準に届かなければ拡大しない。結果を見てから条件を動かさない |
| 判定 | `cmd/edge-judge`: コスト控除後の 1 件あたり損益の bootstrap 95% 信頼区間と PF | 上限 < 0 なら reject、下限 > 0 かつ PF ≥ 1.1 なら昇格候補、それ以外は continue(「少しプラス」は合格にしない) |

結論:
- intraday の候補(押し目・ブレイク・レンジ・時間帯アノマリー・急変の逆張り)の大半は、backtest の段でコスト床を越えられなかった。残ったものも件数が少ないか、OOS やパラメータの頑健性で崩れた(日足のトレンドフォローは 1 つのパラメータでは大きくプラスでも、隣の値で崩れたので運として捨てた)。
- パラメータを凍結した機械戦略は、forward で損益がほぼゼロに収束した。
- LLM 判断ループも、forward の件数を貯めた版でコスト込みのプラスを示せなかった。
- だからロットは上げていない。当たりが出るまで候補を増やして探す(多重比較で偶然の勝ちを拾う)こともしていない。

学んだこと:
- **コスト床が先**。API 経由の往復コストは約 1.1 pips(手数料 + スプレッド)。intraday の値幅ではこれだけで期待値が負になる戦略が大半で、コストを最初から計上しない backtest は見る価値が無い。
- **LLM は判事より研究助手**。LLM 判断ループのログを機械的に分類すると、大半はコードの veto や数値のしきい値と同じ答えだった。LLM を判断ループに置くと backtest ができず、forward の少ない件数でしか測れない。ルールの候補出しと振り返りに使い、売買の可否は決定論のルールに置くほうが検証できる。
- **自分を騙さない規律はコードに書く**。事前登録・条件の凍結・パラメータの頑健性(1 つの値だけ良い戦略は運として捨てる)・判定の機械化。文書に書いた規律は守られない。

## できること

- **2 つの執行モード**: `paper_config`(GMO の公開 API の実ティッカー × bot 内の約定シミュレーション。口座不要)/ `live_config`(GMO の本番口座で実弾)。
- **戦略**(`backend/internal/domain/strategy/`): momentum_pullback / breakout_follow / range_breakout_probe / mtf_pullback / ma_pullback / ma_pullback_v2 / trend_follow / daily_trend / london_breakout / gotobi_fix / exhaustion_fade / no_trade。どれも config(YAML)で選ぶ。
- **守り**: broker 側 OCO・ナンピン禁止・日次損失 cap・1 トレード損失 cap・スプレッドガード・緊急停止・セッションガード(深夜の新規禁止・朝の一斉手仕舞い)・建玉時の config 凍結・reconcile。経済指標の前後の新規停止(`configs/event_calendar.yaml`。同梱分は記入例なので、使うなら日程を自分で保守する)。
- **LLM(任意)**: advisor / LLM 判断ループ / advisor v2 / ダッシュボードからの質問。全部 opt-in。
- **検証ハーネス**: `cmd/backtest` → `cmd/edge-judge`、`cmd/sweep`(walk-forward)、`cmd/histdata-ingest`(ヒストリカルの取込み)、`cmd/spread-calibrate`(時間帯別スプレッド)。
- **ダッシュボード**: `http://localhost:3000`(建玉・損益・判断ジャーナル・チャート・緊急停止 / 再開・手動決済・保有上限の延長・Claude に質問)。

## 前提

| 用途 | 必要なもの |
|---|---|
| ビルド・テスト・backtest | Go 1.21 以上。`GOTOOLCHAIN=auto`(既定)なら `backend/go.mod` の `toolchain` 行の版(現在は go1.26.9)を初回に自動取得する(要ネットワーク)。`GOTOOLCHAIN=local` の環境ではその版を直接入れる |
| `make test` / `make check-backend` | cgo が使える C コンパイラ(`go test -race` の要件。macOS は Xcode Command Line Tools) |
| ダッシュボード | Node.js 22 LTS 推奨(20.19 以上 / 22.12 以上。vitest 4 / vite の要件。CI は 22)。Next.js 16 は React 18 の非推奨警告を出すが、動作には影響しない |
| 実行(paper / live) | Docker(Postgres 16。`docker-compose.yml`。ホストの `127.0.0.1:5432` が空いていること) |
| live | GMO コイン外国為替FX の口座と API キー |
| LLM の経路(任意) | [Claude Code](https://docs.claude.com/en/docs/claude-code) CLI(ログイン済み。呼ぶたびに利用料・利用枠を消費する) |
| `make start` / `make stop` | macOS(Docker Desktop を `open` / `osascript` で操作する。Linux では Docker を手で起動すれば概ね動く) |

## セットアップ

```bash
git clone https://github.com/YutaroSugaya/fx-bot.git && cd fx-bot
cd backend && go build ./... && go vet ./... && go test ./... && cd ..
cd frontend && npm ci && npm test && cd ..
git config core.hooksPath .githooks
```

### .env
```bash
cp .env.example .env
```
`.env.example` のコメントに従って埋める。既定は paper 側で、live に必要な値は空にしてある。
`.env` は make が `-include` で読む(Makefile の変数になる)ので、値はクォートせず、`$` と `#` を含めないこと。

| キー | 内容 |
|---|---|
| `DATABASE_URL` | Postgres の DSN(既定は `docker-compose.yml` の開発用 DB) |
| `DASHBOARD_USER` / `DASHBOARD_PASS` | BasicAuth。paper / live では必須。例の値(`change_me_locally` 等)のままだと起動しない |
| `API_ADDR` | 制御 API の bind(既定 `127.0.0.1:8080`) |
| `CLAUDE_CLI_PATH` | claude CLI のパス。呼ばずに試すなら `<repo>/tools/fake_claude.sh` |
| `GMO_API_KEY` / `GMO_API_SECRET` | live のときだけ |
| `LIVE_TRADING_ENABLED` / `LIVE_CONFIRM_SYMBOLS` | live の確認 env(既定は false と空) |

制御 API とダッシュボードは既定で `127.0.0.1` だけに bind する。loopback の外に向けるなら BasicAuth を外さないこと(外したまま bind すると起動しない)。ダッシュボードの表示を絞る env は `frontend/.env.example` にある。

### Postgres と active config
```bash
make migrate-up
bash scripts/seed_active_config.sh configs/trend_v4_USD_JPY.yaml           # dry-run(検証して ROLLBACK)
bash scripts/seed_active_config.sh --apply configs/trend_v4_USD_JPY.yaml   # bot を止めた状態で
```
- `make migrate-up` は(Docker の daemon が落ちていれば Docker Desktop を起動し)`docker compose up -d postgres` してから migration を当てる。
- bot は**起動時にだけ** DB の `strategy_configs` から (symbol, mode) ごとの active config を 1 本読む。`--apply` は bot を止めた状態で行い、そのあと `make start` する。active が無いシンボルは何も建てない(paper は warn を出して続行する)。tracked の bot_config は USD_JPY / EUR_JPY / GBP_JPY なので、上の例だけ seed した場合、EUR_JPY / GBP_JPY の warn が起動時に出るのは正常。
- `seed_active_config.sh` は起動時と同じ検証(`backend/cmd/config-check`)を掛けてから、YAML を active として入れる。`--mode live_config` で live 側に入れる。書込みは `DATABASE_URL` ではなく `docker exec fxbot-postgres psql` で行う。別の Postgres なら `FXBOT_PG_CONTAINER` / `FXBOT_DB_USER` / `FXBOT_DB_NAME` を設定するか、`--print-sql` の出力を自分で流す。
- `configs/` の YAML はどれも戦略の例で、エッジは確認されていない。

### データ(backtest)
- ヒストリカルデータは同梱しない。`scripts/fetch_histdata.sh` は HistData の無料ダウンロードフォームを自動で叩いて 1 分足を取得する補助。先に HistData の利用規約を確認し、自動取得が許されないなら手で落とす。データの再配布はしないこと(`data/` は gitignore)。
- 取込みは別 DB(名前が `_backtest` で終わるもの)に: `scripts/provision_backtest_db.sh` → `cd backend && go run ./cmd/histdata-ingest -apply …`。手順は [docs/workflows/BACKTEST.md](docs/workflows/BACKTEST.md)。

## 動かす

### paper(口座不要・実際には発注しない)
```bash
make start
```
- bot(`go run ./cmd/bot`)と dashboard(`next dev`)を前景で起動する。Ctrl+C で両方止まる。
- dashboard は `http://localhost:3000`。開くとブラウザの BasicAuth ダイアログが出る(API の 401 と `WWW-Authenticate` が Next の proxy 越しに届く)ので、`.env` の `DASHBOARD_USER` / `DASHBOARD_PASS` を入れる。
- tracked の `configs/bot_config.yaml` は USD_JPY / EUR_JPY / GBP_JPY の paper で、LLM の経路は全部 off。上の seed をしたシンボルだけ、決定論のエンジンが config の戦略で建てる。
- LLM を使うなら、自分の bot_config(`BOT_CONFIG_PATH`)で `ai_advisor` / `advisor_v2` の `enabled: true` にする。LLM 判断ループは tracked の bot_config にセクションが無いので、`llm_decision:` を自分で書く(キーは [docs/runtime/CONFIG.md §2.1](docs/runtime/CONFIG.md))。判断に使うチェックリストは `runtime/playbook_<SYMBOL>.jsonl`(gitignore)の最終行で、1 行 1 版の JSON(`{"symbol":"USD_JPY","rules":"<チェックリスト本文>","note":"<任意>","created_at":"<RFC3339>"}`)。ファイルが無ければ空の playbook で判断する(コードの veto と risk Gate はそのまま効く)。

### live
エッジは確認されていない。動かすなら次の全部が要る:
1. `configs/bot_config.live.example.yaml` を `configs/bot_config.live.yaml`(gitignore)にコピーし、`.env` の `BOT_CONFIG_PATH=../configs/bot_config.live.yaml`
2. `.env` の `LIVE_TRADING_ENABLED=true` と `LIVE_CONFIRM_SYMBOLS`(bot_config の symbols と同じ集合)、`GMO_API_KEY` / `GMO_API_SECRET`
3. live 側の active config(`seed_active_config.sh --apply --mode live_config …`)
数量上限と損失上限は [configs/hard_limits.yaml](configs/hard_limits.yaml)。

### `make start` / `make stop` が他にやること(知らずに打たないこと)
- `make start` の前段の `kill-stale` は、OPEN / CLOSING の建玉があると止まる(`FORCE=1` で続行。postgres が落ちていると検査せずに通る)。そのうえで **API のポート(8080)と FE_PORT(3000)を LISTEN しているプロセスを、名前を見ずに SIGTERM → SIGKILL する**(別のアプリの dev server も落ちる。`make start FE_PORT=3001` で避けられる)。
- `make start` は Docker の daemon が落ちていれば Docker Desktop を起動し(`open -a Docker`)、postgres を `docker compose up -d` し、migration を当て、`node_modules` が無ければ `npm install`、`frontend/.next` を消し、`runtime/logs/bot_stdout.log` を空にする。
- `make stop` は建玉の有無を見ずに bot と dashboard を止め(ポート 8080 / 3000 を LISTEN しているプロセスも止める)、`docker compose down`(volume は残す)して、**Docker Desktop を終了する**(他のコンテナも止まる)。
- `make clean-runtime` は緊急停止フラグも消す。
- ダッシュボードの「advisor を今すぐ実行」「LLM 判断を今すぐ」「Claude に聞く」は押すと claude を呼ぶ(advisor のボタンは `ai_advisor.enabled` に関係なく呼ぶ)。

### 緊急停止
```bash
set -a; . ./.env; set +a     # DASHBOARD_USER / DASHBOARD_PASS をシェルに読む(repo root で)
curl -X POST -u "$DASHBOARD_USER:$DASHBOARD_PASS" http://127.0.0.1:8080/api/emergency-stop
curl -X POST -u "$DASHBOARD_USER:$DASHBOARD_PASS" http://127.0.0.1:8080/api/emergency-resume
```
新規エントリーだけを止める。既存の建玉と broker の OCO はそのまま。bot が応答しないときは `touch runtime/emergency_stop.flag` でも止まる(再開はファイルを消す)。

### バックテストとエッジ判定
```bash
export BACKTEST_DATABASE_URL='postgres://fxbot:fxbot@localhost:5432/fxbot_backtest?sslmode=disable'  # provision_backtest_db.sh が表示する値
cd backend
DATABASE_URL="$BACKTEST_DATABASE_URL" go run ./cmd/backtest \
    -config ../configs/probe_trend_follow_USD_JPY.yaml -symbol USD_JPY -from 2016-01-01 -to 2024-01-01 \
    -fee-rate 0.002 -spread-median 0.5 -spread-tokyo-spike 9.5 \
    -pnl-out pnl.json
go run ./cmd/edge-judge -json pnl.json
```
- `BACKTEST_DATABASE_URL` は `.env` に書いても `go run` には渡らない(make も export しない)。シェルで export する。
- `cmd/backtest` のコスト系フラグ(スプレッド・手数料・スリッページ・スワップ)は**既定 0(摩擦なし)**。必ず指定する。`-pnl-out` が取引ごとのコスト控除後損益を書き、`cmd/edge-judge` がそれを判定する。例と手順は [docs/workflows/BACKTEST.md](docs/workflows/BACKTEST.md)。

## 構成

```
cmd/bot ─▶ app(配線・ループ)─▶ handler(HTTP・薄い)─▶ usecase(command / query・CQRS)─▶ domain(純粋)
                                                                   │
                                                                   ▼ port(interface)◀── adapter(broker / repository / advisor …)
                                                                   safety(emergency・横断)
frontend(Next.js)── /api/* を 127.0.0.1:8080 へ proxy
```
層の契約は [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)、動きの全体図は [docs/runtime/SYSTEM_DESIGN.md](docs/runtime/SYSTEM_DESIGN.md)。

`backend/cmd/`: `bot`(本体)/ `migrate` / `config-check`(active config の検証)/ `backtest` `sweep` `edge-judge`(検証)/ `histdata-ingest` `fetch-candles` `spread-calibrate`(データ)/ `fee-backfill`(手数料の推定記帳)。

| ディレクトリ | 中身 |
|---|---|
| `configs/` | `bot_config*.yaml`(実行設定)・`hard_limits.yaml`(人間が commit する上限)・戦略 config の例・経済指標カレンダー |
| `prompts/` | advisor が使うプロンプトと skill |
| `.claude/agents/` | LLM の各経路が呼ぶ subagent の定義 |
| `scripts/` | seed・backup・データ取得・秘密情報スキャン・死活監視 |
| `tools/` | `fake_claude.sh`(Claude を呼ばずに試す)・`launchd/`(postgres + bot + dashboard の自動復帰) |
| `deploy/launchd/` | macOS の launchd テンプレート(bot の常駐・healthcheck・夜間レポート)。手順は [deploy/launchd/README.md](deploy/launchd/README.md) |

## 既知の制限

- live の bot 側の出口(保有上限・ratchet・朝の一斉手仕舞い)は bot が落ちると止まる。broker の OCO(TP / SL)だけが残る。
- live で発注の応答が通信エラーで失われ、実は約定していた場合、reconcile はその建玉を「外部建玉」として取り込むだけで OCO を付けない(画面に出るので、手で守りを置く)。
- 層規約は Stop hook の grep で検査していて、CI では検査しない。
- 経済指標カレンダーは同梱の `configs/event_calendar.yaml` が記入例(過去の日付)で、日程の自動取得はしない。使うなら自分で保守する(既定では bot_config と同じディレクトリから読む。`EVENT_CALENDAR_PATH` で変更可。起動ログの `event_calendar_loaded` で件数を確かめる)。
- `cmd/fetch-candles` は `DATABASE_URL` に書き込む(backtest 用 DB に入れるなら差し替える)。`cmd/backtest` のコストは既定 0。
- 時刻は JST(+9)固定、pip size はシンボルごとの定数。ダッシュボードのロット表示は全ペアで 1 lot = 10,000 通貨。
- LLM 判断ループの playbook とヒストリカルデータは同梱しない。
- macOS 前提の部分がある(Makefile の Docker Desktop 操作・`lsof`・launchd テンプレート)。

## 検証コマンド

```bash
make check-backend                 # go test -race + vet + build
cd frontend && npm test && npm run build
bash scripts/secret-scan_test.sh && bash scripts/secret-scan.sh
bash .claude/hooks/pretooluse-deny_test.sh
bash .claude/hooks/pretooluse-deny-edit_test.sh
```
integration テスト(`make test-integration`)は全テーブルを truncate する。`INTEGRATION_TEST_DB_URL` は名前が `_test` で終わる専用 DB を指すこと(違えば Makefile が拒否する)。作り方は [docs/workflows/TESTING.md §6](docs/workflows/TESTING.md)。

## Claude Code / Codex で開くとき

- `.claude/settings.json` と `.claude/hooks/` は、このリポジトリを Claude Code で開いた瞬間に効く。PreToolUse hook が integration テストの実行・DB の破壊・postgres / bot の停止・ハーネスの書換えを拒否する。
- Stop hook(`.claude/hooks/pre-stop-checks.sh`)は毎回 secret scan を回し、Go か migration の変更があると層規約・migration 規約・`make check-backend` も回す。`make check-backend` は `go test -race` を使うので cgo(C コンパイラ)が要る。
- migration や repository のコード(`backend/migrations/`・`backend/internal/adapter/repository/`・`backend/internal/port/repository.go`・`backend/cmd/migrate/`)が変わると、Stop hook は `make test-integration` を自動で実行する。これは `INTEGRATION_TEST_DB_URL` の DB の**全テーブルを空にする**ので、名前が `_test` で終わる専用 DB を指すこと(違えば Makefile が拒否する)。hook は `.env` ではなく自分の環境変数を見る(Claude Code を起動するシェルで export するか、`.claude/settings.local.json` の `env` に書く)。未設定だと Stop をブロックし、同じセッションで 3 回続けてブロックしたあとは警告を残して通す。
- `.claude/settings.json` は hooks と 2 つのフラグだけを持ち、permissions は持たない(許可ルールやモードは各自の `.claude/settings.local.json` に書く)。hook は Claude Code を起動したシェルの `PATH` で `go` / `node` / `make` を探す。GUI から起動して見つからないときは `.claude/settings.local.json` の `env` で `PATH` を足す。
- deny hook は「止める仕組み」で、サンドボックスではない(文字列の照合なので、別の言語のワンライナー等までは止めない)。
- Stop hook を外すには `FXBOT_PRESTOP_CHECKS=off` / `FXBOT_DOCS_SYNC_CHECKS=off`(`.claude/settings.local.json` の `env` などで)。deny hook を外すには `.claude/settings.json` の hooks から外す。
- AI 作業の絶対ルールは [CLAUDE.md](CLAUDE.md)(Codex 向けの入口は [AGENTS.md](AGENTS.md)。`.codex/` に Stop hook の雛形)。

## ドキュメント

[docs/README.md](docs/README.md) が目的別の索引。

## ライセンス・免責・第三者

- MIT([LICENSE](LICENSE))。投資助言ではなく、無保証(AS IS)。利用によって生じた損失について作者は責任を負わない。live で動かす前に「既知の制限」を読むこと。
- GMO コイン株式会社・Anthropic・TradingView とは無関係の個人プロジェクト。各社の名称・商標は説明のために使っているだけで、提携や推奨を意味しない。GMO コインの API 利用規約、Anthropic の利用規約・利用ポリシー、データ提供元(HistData など)の規約は各自で確認すること。
- ダッシュボードのチャートは [TradingView Lightweight Charts™](https://www.tradingview.com/lightweight-charts/)(Apache-2.0, © TradingView, Inc.)を使い、チャート上の TradingView の帰属表示を残している。
