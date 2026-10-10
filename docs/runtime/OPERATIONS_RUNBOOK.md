# Operations Runbook — fx-bot 運用引き継ぎ

本ドキュメントは **fx-bot を壊さず・安全に運用するための不変条件・トラブルシュート・ロールバック手順** を集約する。

## 1. 不変条件 (リファクタで壊さないこと)

- **positions に建てた時点の `config_id / TP / SL / MaxHold / extension_* / early_exit_*` を凍結保存する**。新規エントリーだけが最新 active config を使う。トレーリング等で動的 TP を入れる場合は active config を直接参照せず、`positions` に新カラムで管理する
- [strategy/engine.go](../../backend/internal/domain/strategy/engine.go) の未知 strategy 名 → no_trade フォールバックは保持
- **Live モードで TP/SL は必ず GMO 側に置く** (Bot 内 OnTick 監視のみで損切りする設計は禁止)。新規は成行 (MARKET) で建て、約定後に OCO を付ける (broker adapter は IFDOCO 一括発注にも対応しているが、発注経路では使っていない)
- 外部ポジション (`source=external_broker`) は `max_open_positions` カウント対象外、TP/SL/MaxHold 管理対象外、強制決済ボタン非表示。bot が触らない不可侵 inventory として扱う
- `runtime/emergency_stop.flag` がある間は新規エントリーしない (新規は risk Gate が拒否する。サイクル冒頭で確認して LLM も呼ばずに止まるのは LLM 判断ループだけで、advisor / advisor v2 は claude を呼んだうえで新規が拒否される)

## 2. 撤退ライン (例)

数値は資金規模とロットに合わせて運用者が事前に決める。決めたら期間中は動かさない。

| 状況 | アクション |
|---|---|
| 月次 PnL が 2 ヶ月連続マイナス | ロットを 1 段下げる |
| 単月 MDD が事前に決めた上限を超える | 翌月は新規エントリー停止、原因解析のみ |
| 累積 PnL が事前に決めた損失上限に達する | 完全停止。Backtest から戦略やり直す |
| Reconciler 不整合 (`naked_broker_position` 以外) が 1 件でも出る | Live 新規エントリー停止 |
| 重要指標帯で想定外約定が出る | 経済指標 no_trade ルール修正まで停止 |
| 結果を見て手動で介入したくなる | ロットを 1 段下げる |

## 3. 既知のハマりポイント

| 症状 | 原因 | 対策 |
|---|---|---|
| `read bot_config: no such file or directory` | `cd backend && go run ./cmd/bot` を素で打った (既定パス `configs/…` は起動ディレクトリ相対で、`backend/` には無い) | `make start` / `make backend` を使う (Makefile が `.env` と `../configs/…` のパスを渡す)。直接起動するなら `BOT_CONFIG_PATH` / `HARD_LIMITS_PATH` 等を `backend/` からの相対で渡す |
| 起動はするが何も建てない (起動ログに `active_config_loaded_from_db` が出ない。LLM 判断ループなら判断が `no_active_config` で終わる) | 起動した mode (`bot.mode`) の active strategy config が DB に無い (このとき warn は出ない) | `bash scripts/seed_active_config.sh --apply [--mode …] configs/<file>.yaml` → bot 再起動 ([CONFIG.md §4.1](CONFIG.md)) |
| active config を差し替えたのに挙動が変わらない | active config は起動時にしか読まれない | bot を再起動する |
| `claude not found` | claude CLI 未インストール | `npm install -g @anthropic-ai/claude-code` |
| `advisor_run status=cli_error` (auth 系) | claude のセッションが切れた | `claude` 単体で起動して再ログイン |
| `config_rejected reason=[hard_limit] ttl ...` | advisor が valid_from と valid_until を異常に短く/長くした | プロンプトの "valid_until は valid_from から 60分" を強調 |
| dashboard が `loading…` のまま | bot が止まっている / API_ADDR が違う | `runtime/logs/bot_stdout.log` を見て、止まっていれば `make start` (`make stop` は Docker Desktop も終了させる) |
| `make start` 後に残るプロセス | trap で kill されなかった子プロセス | `lsof -nP -iTCP:8080 -iTCP:3000 -sTCP:LISTEN` で LISTEN しているプロセスを確認し、bot / next のものだけ `kill <pid>` する (`lsof -ti:8080` は接続中のブラウザ等も拾うので、そのまま `xargs kill` しない) |
| 平日でも config が生成されない | `ai_advisor.enabled: false` (既定) か、scheduler の `start_time`/`end_time` 帯から外れている | `bot_config` の `ai_advisor` / `scheduler` セクション確認 |

## 4. テスト / 品質維持

```bash
make test          # 全 race-detector 付き
make test-cover    # カバレッジ
make vet           # go vet
make fmt           # go fmt
make check-backend # PR 前の一括チェック
```

## 5. ロールバック計画

active config が暴走したら:

```bash
# 1) 新規エントリーを止める
curl -X POST -u "$DASHBOARD_USER:$DASHBOARD_PASS" http://127.0.0.1:8080/api/emergency-stop
# または touch runtime/emergency_stop.flag

# 2) 開いているポジションを確認し、必要なら GMO Web UI / アプリで手動クローズ

# 3) 直近の config 履歴を確認 (SELECT のみ)
docker compose exec postgres psql -U fxbot -d fxbot \
  -c "SELECT id, config_id, mode, symbol, status, strategy_name, created_at FROM strategy_configs ORDER BY id DESC LIMIT 5;"

# 4) 既知の良い config を active に戻す。bot だけ止める (make start の terminal で Ctrl+C。postgres は動いたまま)
bash scripts/seed_active_config.sh configs/<known-good>.yaml          # dry-run で確認
bash scripts/seed_active_config.sh --apply configs/<known-good>.yaml
make start      # OPEN / CLOSING の建玉があると kill-stale が止める。建玉を確認したうえで続行するなら make start FORCE=1

# 5) 状態を確認してから再開
curl -X POST -u "$DASHBOARD_USER:$DASHBOARD_PASS" http://127.0.0.1:8080/api/emergency-resume
# または rm runtime/emergency_stop.flag
```

## 関連ドキュメント

- [ARCHITECTURE.md](../ARCHITECTURE.md) — 層の契約 + PR チェックリスト
- [CONFIG.md](CONFIG.md) — bot_config / hard_limits / active config の seed
- [DATA_MODEL.md](DATA_MODEL.md) — テーブルスキーマ
- [SYSTEM_DESIGN.md](SYSTEM_DESIGN.md) — 既存システムの全体像
- [CLAUDE.md](../../CLAUDE.md) — 不変条件(ポジに config を凍結保存 / Live で TP/SL は必ず broker 側 / DB wipe 禁止)
