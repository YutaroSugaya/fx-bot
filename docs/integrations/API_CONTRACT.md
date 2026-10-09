# API Contract — Backend Response DTO ↔ Frontend Type の同期

backend HTTP API (`/api/*`) と frontend dashboard (Next.js) の型同期契約。
両者は **手書きの型定義** で繋がっているため、片方を変更したらもう片方も同期する。

---

## 1. backend 側 DTO

[backend/internal/app/handler/types.go](../../backend/internal/app/handler/types.go) に集約。

主要レスポンス DTO:

- `ManualTradeRequest` / `ManualTradeResponse` (`POST /api/trade/manual`)
- `ClosePositionRequest` / `ClosePositionResponse` (`POST /api/positions/close`)
- `ExtendPositionRequest` / `ExtendPositionResponse` (`POST /api/positions/extend`)
- `TriggerResult` (`POST /api/advisor/trigger`)
- `AskClaudeRequest` / `AskClaudeResponse` (`POST /api/ask-claude`)

Status / open positions / trades などの **GET 系 DTO** は usecase/query/ 内に定義 (例: `OpenPositionView`, `AdvisorDecisionView`)。

---

## 2. frontend 側型

[frontend/app/page.tsx](../../frontend/app/page.tsx) の冒頭の **inline 型定義** と、複数画面で共有する型 (`Status` / `Trade` 等) を置く [frontend/app/lib/types.ts](../../frontend/app/lib/types.ts) で持つ:

```typescript
type Status = {
  mode: string                          // "paper_config" | "live_config" | "disabled"
  symbol: string
  uptime: string                        // "3h27m12s"
  open_positions: number
  emergency_stop: boolean
  active_config_id?: string
  valid_until?: string                  // RFC3339
  strategy?: string                     // active config の strategy.name
  enabled?: boolean
  ticker_errors?: number                // legacy top-level (counters.ticker_errors と二重)
  counters?: {
    ticker_errors: number
    emergency_trips: number
    resolve_timeouts: number
    close_races: number
    naked_positions: number
    trading_cycle_missing: number
  }
}
type AskClaudeResult = {
  answer: string                        // Claude CLI の応答
  duration_ms: number
  error?: string                        // omitempty
}
type Trade = { ... }
type TriggerResult = { ... }
type ManualTradeResult = { ... }
type OpenPosition = { ... }
type TimeframeView = { ... }
type MarketState = { ... }
type AdvisorDecision = { ... }
```

これらは backend DTO と **1:1** で対応する。フィールド名 (snake_case JSON tag) を一致させる。`Status` の SoT は [BotStatusView](../../backend/internal/usecase/query/get_bot_status.go)、`AskClaudeResult` の SoT は [AskClaudeResponse](../../backend/internal/app/handler/types.go)。

---

## 3. /api/* エンドポイント一覧

### GET (Query 系)

| Path | レスポンス型 (frontend) | backend usecase/handler |
|---|---|---|
| `/api/status` | `Status` (= `BotStatusView`) | `query.GetBotStatus` → `status_handler.go` |
| `/api/active-config` | `StrategyConfig` (parsed YAML as JSON) | `status_handler.go:ActiveConfig` (active config holder から取得) |
| `/api/positions` | `OpenPosition[]` | `query.ListOpenPositions` → `positions_handler.go` |
| `/api/trades?limit=N` | `Trade[]` | `query.ListTrades` → `trades_handler.go` |
| `/api/advisor/recent?limit=N` | `AdvisorDecision[]` | `query.ListRecentDecisions` → `advisor_handler.go` |
| `/api/market/state?symbol=USD_JPY` | `MarketState` | `query.GetMarketState` → `market_handler.go` |
| `/api/strategy/signal?symbol=USD_JPY` | `livesignal.Snapshot` (bot の実 LIVE 判定: strategy 判定 + risk gate 判定。`symbol` 必須=400、snapshot 未生成=404) | `StrategySignalHandler.Signal` → `strategy_signal_handler.go` |
| `/api/llm-decision` | LLM 決定ループの最新 per-symbol スナップショット (`runtime/llm_decision_status.json` をそのまま返す。未生成時は `{"by_symbol":{}}`) | `LLMDecisionHandler.Status` → `llm_decision_handler.go` |
| `/api/advisor-v2` | advisor v2 の最新 per-symbol スナップショット (`runtime/advisor_v2_status.json` をそのまま返す。未生成時は `{"by_symbol":{}}`) | `AdvisorV2Handler.Status` → `advisor_v2_handler.go` |
| `/healthz` | `{"status":"ok"}` (200 OK。liveness 用) | `status_handler.go:Healthz` (Basic Auth 不要) |

### POST (Command 系)

| Path | Request / Response | backend usecase/handler |
|---|---|---|
| `/api/trade/manual` | `ManualTradeRequest` / `ManualTradeResult` | `command.ManualTrade.Execute` → `trades_handler.go:Manual` |
| `/api/positions/close` | `ClosePositionRequest` / `ClosePositionResponse` | `command.ClosePosition.Execute` → `positions_handler.go` |
| `/api/positions/extend` | `ExtendPositionRequest` (`{id, symbol, add_minutes}`) / `ExtendPositionResponse` (`{position_id, max_hold_minutes, added_minutes, deadline_at, remaining_minutes, error?}`。範囲外 add_minutes=400、非存在/OPEN でない=404) | `command.ExtendMaxHoldCommand.Execute` → `positions_handler.go:Extend` |
| `/api/llm-decision/trigger` | (body 不要) / `{"started": true}` (202。実行中なら 409 `{"started": false, "error": ...}`、未配線なら 503) — 全ペアの LLM 判断サイクルを即時再実行 | `LLMDecisionHandler.TriggerNow` → `llm_decision_handler.go` |
| `/api/advisor/trigger` | (body 不要) / `TriggerResult` | `runAdvisorOnce(manual)` → `advisor_handler.go` |
| `/api/ask-claude` | `AskClaudeRequest` / `AskClaudeResult` | `query.AskClaude.Execute` → `ask_claude_handler.go` (Q&A は読み取り扱い) |
| `/api/emergency-stop` | (body 不要) / `{}` | `safety.Trip(flag, "manual_via_api")` → `emergency_handler.go:Stop` |
| `/api/emergency-resume` | (body 不要) / `{"emergency_stop":"cleared"}` | `os.Remove(flag)` → `emergency_handler.go:Resume` (運用責任で flag file を削除) |

### `/api/trade/manual` — `allow_override` フィールド

`ManualTradeRequest.allow_override` (bool, default false) は **operator-explicit な
soft gate bypass フラグ**。フィールド省略 / `false` の場合、manual entry は auto
entry と同じ admission gate を全て通る。

- `true` のとき bypass 可能 (allowlist・`isOverridableReason`): `open_positions`, `cooldown`,
  `consecutive_losses`, `post_loss_freeze`, `trades_in_window`, `loss_in_window`,
  `direction_*`(同方向 2 SL の当日ブロックを含む), `spread`
- **常に enforce される hard safety**(`risk.EvaluateHardSafety` が第 2 パスで再検査): `emergency_stop`,
  `no_active_config`, `event_freeze`, `daily_loss_*` / `account_daily_loss_*`, `reentry_cooldown`,
  `pyramiding_blocked_same_side_*`(ナンピン禁止・外部建玉を含む), `account_open_positions`
  (`hard_safety_never_overridable_test.go`, `entry_admission_no_nanpin_override_test.go`)

`direction_*` と `spread` が override 可なのは、手動発注は operator が現在のスプレッドと方針を見たうえでの
明示的な opt-in とみなすため。自動経路(advisor / LLM 判断ループ / advisor v2)は常に `allow_override: false`。
現在のダッシュボードには手動発注のボタンが無い(`page.tsx` の `executeTrade()` は未使用)。API を直接叩く場合だけ使う。

bypass 成功時は backend が `admission_manual_override` warn ログを
`source` / `reason` / `side` / `config_id` 付きで出力する (audit trail)。

---

## 4. 認証

すべてのエンドポイントは **HTTP Basic Auth** で保護される (`/healthz` のみ例外)。
`DASHBOARD_USER` / `DASHBOARD_PASS` 環境変数で credential を設定。

**fail-closed**:
- `bot.mode` が `paper_config` / `live_config` (= production) では両 env 必須。
  欠落で `APIServer.Run()` が `ErrAPIAuthMisconfigured` を返し bot 全体を停止する。
  mode の SSOT は bot config の `bot.mode` (`BOT_MODE` env は参照しない)。
- auth bypass は次の 2 つだけ (どちらも起動時に WARN ログ):
  - `bot.mode=disabled` で credential 未設定 (`api_auth_bypass_disabled_mode`)
  - `DASHBOARD_AUTH_DISABLE=true` (`api_auth_bypass_via_env`)
- bypass は loopback bind (`API_ADDR=127.0.0.1:…`) のときだけ有効。それ以外の bind では
  `APIServer.Run()` が `ErrAPIAuthUnsafeBind` で起動を拒否する。
- `DASHBOARD_PASS` が例の値 (`change_me_locally` / `change_me` / `password` / `admin` 等) のままなら
  `ErrAPIAuthPlaceholderPassword` で起動を拒否する。

frontend は `fetch(path, { cache: 'no-store' })` を呼ぶ。Next.js のクライアントサイド fetch なので、初回ロード時に basic auth dialog が出る。

## 4.1. CORS

- `DASHBOARD_ALLOWED_ORIGINS` 環境変数 (comma-separated) で allowlist を設定。
- 未設定なら CORS ヘッダを一切出さない (= 同一 origin only)。
- リクエストの `Origin` ヘッダが allowlist に**完全一致**したときだけ
  `Access-Control-Allow-Origin` に echo する。`*` ワイルドカードは意図的に未サポート。

## 4.2. CSRF

- 全ルートを `net/http.CrossOriginProtection` で包む (`newCSRFMiddleware`)。
  ブラウザからの cross-origin な POST (`Sec-Fetch-Site: cross-site` / Origin と Host の不一致) は **403**。
- 同一 origin (dashboard の Next.js rewrite 経由) と非ブラウザ (curl / healthcheck) は通る。GET 等の safe method は常に通る。
- trusted origin = 既定 dashboard (`http://localhost:3000` / `http://127.0.0.1:3000`) + `DASHBOARD_ALLOWED_ORIGINS`。
- 待受は既定で `127.0.0.1:8080` (`API_ADDR`)。dashboard も `next dev -H 127.0.0.1`。

## 4.3. Host ヘッダ (DNS rebinding 対策)

- loopback bind のとき、Host ヘッダが `localhost` / `127.0.0.1` / `[::1]` / `DASHBOARD_ALLOWED_HOSTS`
  (カンマ区切り) のどれでもないリクエストは **421** (`newHostGuard`)。ダッシュボード側も同じ規則で
  ページと `/api` の proxy を **403** にする (`frontend/proxy.ts`)。
- 理由: 攻撃者のドメインを 127.0.0.1 に解決させたページは、ブラウザから見て同一 origin なので
  CSRF 検査を通るが、Host ヘッダは攻撃者のドメインのまま。
- loopback 以外の bind では Host で絞らない (BasicAuth が必須で、IP やホスト名で来るため)。

---

## 5. JSON フィールド命名規約

- **すべて snake_case** (Go の `json:"foo_bar"` タグ、frontend の `foo_bar: ...`)
- 数値は数値型のまま (string 化しない)
- bool は `boolean` (`enabled`, `emergency_stop`)
- 時刻は **RFC3339 UTC 文字列** (`2026-05-20T07:00:00Z`)
- ID は **int64**, **string** どちらも可 (frontend では `number`, `string` を使い分け)
- `error?` フィールドは optional (omitempty) — エラー時のみ含まれる

---

## 6. 変更フロー

backend API を変えたいとき:

1. `backend/internal/app/handler/types.go` または `usecase/query/*.go` の DTO 更新
2. `backend/internal/app/handler/<resource>_handler.go` のレスポンス組み立て更新
3. `frontend/app/page.tsx` (または `frontend/app/lib/types.ts`) の対応する `type Foo = { ... }` 更新
4. fetch 呼び出し箇所 (例: `fetchJSON<NewType>('/api/foo')`) で型を反映
5. UI コンポーネントの map / render を更新
6. `make check-backend` で go test pass
7. `cd frontend && npm run build` で TypeScript pass

---

## 7. 型の自動同期 (将来構想)

現状は **手書き** で同期している。将来:

- OpenAPI / JSON Schema を backend から生成し、frontend で `openapi-typescript` 等で型を import
- protobuf に統一して両者が同じスキーマから生成 (= overkill だが選択肢)

ただし、今の規模 (エンドポイント十数本) では手書きで足りている。

---

## 8. アンチパターン

- ❌ backend で `User` を `user` (lowercase) JSON tag 化 / frontend で `User` (PascalCase) を期待 → snake_case 統一
- ❌ backend が optional フィールドを `null` で返す / frontend が `?` 推論 → omitempty + frontend optional 統一
- ❌ frontend 側で「fetch して `as Status`」とキャストするだけで型不整合を見逃す → `fetchJSON<Status>` helper を使う
- ❌ 同じ概念の DTO を handler ごとに別定義 (= 重複型) → query/ または handler/types.go に集約
- ❌ JSON タグを backend で `omitempty` なのに frontend で必須にする (実行時 undefined)

---

## 9. 関連 docs

- [ARCHITECTURE.md](../ARCHITECTURE.md)
- [layers/handler.md](../architecture/layers/handler.md) — handler 実装
- [layers/usecase.md](../architecture/layers/usecase.md) — query usecase
- [OBSERVABILITY.md](../runtime/OBSERVABILITY.md) — `/api/status` レスポンス内容
