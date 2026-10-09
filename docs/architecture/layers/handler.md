# Layer: Handler

## 役割 (1 行)

HTTP / scheduler / signal の入口。リクエストを受けて usecase を呼び、レスポンスを返す薄い層。

---

## やること (do)

- HTTP リクエストの JSON parse、レスポンスの JSON encode
- 簡単な validation (必須項目、enum、範囲)
- usecase の直接呼び出し (`closeCmd.Execute(ctx, input)`)
- 認証 / Authz の薄い適用 (`api_server.go` の BasicAuth ラッパー経由)
- usecase エラー → HTTP status code (`400` / `409` / `503` / `500`) へのマッピング

---

## やらないこと (don't)

- ❌ Repository / Broker を **直接** 呼ぶ (必ず usecase 経由)
- ❌ ビジネスロジックを書く (TP/SL 計算、risk gate、Tx 内 update など)
- ❌ DB 接続 / GMO API 呼び出しを自分で持つ
- ❌ Domain entity をそのまま JSON 出力する (View DTO を作る — `types.go` 参照)

---

## 命名 / 配置

| 種別 | 場所 | ファイル名 | 例 |
|---|---|---|---|
| Handler | `backend/internal/app/handler/` | `<resource>_handler.go` | `position_handler.go` |
| 共通 DTO | `backend/internal/app/handler/types.go` | 1 ファイル集約 | `PositionsResponse`, `TradeRow` |
| Helper | `backend/internal/app/handler/json.go` | 共通 JSON util | `writeJSON`, `readJSON` |

### Controller 抽出条件

`backend/internal/app/controller/` は**現状存在しない**。
以下のいずれかになったら抽出する:

- 単一 handler の入力検証 + HTTP マッピングが **50 行を超えた**
- 同じ DTO 変換ロジックが 2 handler 以上に**重複**してきた
- 認証 / Authz が usecase 呼び出しごとに分岐するようになってきた

Controller は **request DTO → usecase input への変換 / validation / 認証 / エラーマッピング**
の薄い層。business logic は持たない (= usecase との混同を避ける)。

---

## テスト方法 (この層特有)

- **httptest** で書く (`net/http/httptest.NewRecorder`)
- usecase は **fake** に差し替える (`usecase.Closer = &fakeCloser{}`)
- 検証対象は: HTTP status code、レスポンス JSON、Logger に出た重要 key

```go
func TestPositionHandler_ClosePosition_RaceCondition_Returns409(t *testing.T) {
    closer := &fakeCloser{err: command.ErrCloseRaceLost}
    h := handler.NewPositionHandler(closer, nil, ...)
    req := httptest.NewRequest("POST", "/api/positions/123/close", nil)
    rec := httptest.NewRecorder()
    h.ServeHTTP(rec, req)
    if rec.Code != http.StatusConflict { t.Errorf(...) }
}
```

詳細は [TESTING.md](../../workflows/TESTING.md) を参照。

---

## 既存実装の代表例

- [backend/internal/app/handler/positions_handler.go](../../../backend/internal/app/handler/positions_handler.go) — open positions list + manual close. `GET /api/positions[?symbol=X]` で `?symbol=` 指定時はその symbol だけ / 空なら全 symbol。`POST /api/positions/close` は `CloseCommands map[symbol]*ClosePositionCommand` を持ち body の `symbol` (= 該当 row の OpenPositionView.Symbol) で dispatch。`symbol` 未指定 / 未知 → 400、map 自体が空 → 503。`ListOpenPositionsQuery.GetTicker(ctx, symbol)` は row の symbol を受け取り symbol ごとに 1 度だけ評価 (per-call cache、GMO rate limit 配慮)。
- [backend/internal/app/handler/advisor_handler.go](../../../backend/internal/app/handler/advisor_handler.go) — manual advisor trigger
- [backend/internal/app/handler/status_handler.go](../../../backend/internal/app/handler/status_handler.go) — `/api/status` (multi-symbol DTO: `symbols: [{symbol, open_positions, active_config_id, valid_until, strategy, enabled, pnl_24h_jpy, early_exit_count_24h}]` 配列 + `account_open_positions` top-level、加えて primary symbol 値を legacy top-level field に複製) + `/api/active-config` (no query で `{configs: {sym: cfg}}` の全 symbol map、`?symbol=X` で単一 config)。Holder は `app.ActiveConfigHolder.All` から map を取る (= multi-symbol bundle 全部 expose)。
- [backend/internal/app/handler/emergency_handler.go](../../../backend/internal/app/handler/emergency_handler.go) — emergency_stop trip / reset
- [backend/internal/app/handler/ask_claude_handler.go](../../../backend/internal/app/handler/ask_claude_handler.go) — manual Q&A toward Claude CLI
- [backend/internal/app/handler/market_handler.go](../../../backend/internal/app/handler/market_handler.go) — multi-timeframe market state
- [backend/internal/app/handler/trades_handler.go](../../../backend/internal/app/handler/trades_handler.go) — closed trades list (`GET /api/trades`) + manual entry (`POST /api/trade/manual`)。Manual は `TradesHandler.ManualCommands map[string]*ManualTradeCommand` を持ち、body の `symbol` で dispatch する。`symbol` 未指定 / map に無い → 400、map 自体が空 → 503。
- [backend/internal/app/handler/types.go](../../../backend/internal/app/handler/types.go) — レスポンス DTO 一覧 (frontend と同期する)。`ManualTradeRequest.Symbol` が multi-symbol で **必須** field。

API レスポンス DTO と frontend 型の同期契約は [API_CONTRACT.md](../../integrations/API_CONTRACT.md) 参照。

---

## アンチパターン (= 過去にやってしまった失敗)

- ❌ handler 内で `pgxpool.Pool` を直接持ち、SQL を発行した — usecase 経由に直した
- ❌ Live 専用の broker 呼び出しを handler に書いた — usecase / port に閉じ込めた
- ❌ Domain entity (`*domain.Position`) を `json.Marshal` してそのまま返した — `PositionRow` DTO を介す
- ❌ エラーメッセージにスタックトレースを乗せて返した — Logger 側に残し、レスポンスは `{ "error": "kind" }` 程度

---

## 関連 docs

- [usecase.md](usecase.md) — handler が呼ぶ層
- [API_CONTRACT.md](../../integrations/API_CONTRACT.md) — frontend 型と backend DTO の同期
- [OBSERVABILITY.md](../../runtime/OBSERVABILITY.md) — `/api/status` などの観測系
- [TESTING.md](../../workflows/TESTING.md) — httptest と handler テスト
