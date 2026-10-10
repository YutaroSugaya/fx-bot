# Config — Bot / HardLimits / StrategyConfig / Validator / DB SoT

設定ファイルと DB の真の関係をまとめる。
**strategy config の SoT (Source of Truth) は DB**。YAML はその artifact (read-only on boot)。

---

## 1. ファイル一覧

| ファイル | 役割 | SoT 関係 |
|---|---|---|
| `configs/bot_config.yaml` | bot mode / symbols / scheduler / advisor・LLM ループ設定 / risk caps。git tracked の安全側既定 (paper・LLM 呼び出し全 off) | このファイル自身が SoT |
| `configs/bot_config.live.yaml` | live 用の bot_config (gitignore。雛形は [bot_config.live.example.yaml](../../configs/bot_config.live.example.yaml))。`BOT_CONFIG_PATH` で指定 | このファイル自身が SoT |
| `configs/hard_limits.yaml` | 戦略 config が出せる値の絶対上下限 (quantity / TP / SL / max_hold / per-trade 損失 etc.) | このファイル自身が SoT |
| `configs/event_calendar.yaml` | 経済指標の発表前後の entry freeze 窓 (手書き。同梱分は記入例)。既定は bot_config と同じディレクトリの `event_calendar.yaml`(`EVENT_CALENDAR_PATH` で変更可) | このファイル自身が SoT (起動時に読む) |
| `configs/<strategy>_<SYMBOL>.yaml` | 固定戦略の strategy config。`scripts/seed_active_config.sh` で DB の active に据える (§4.1) | **DB に入れたものが SoT** |
| `configs/strategy_config.active.yaml` | DB と同期する artifact (symbols が 2 つ以上なら `strategy_config.active_<SYMBOL>.yaml`) | **DB が SoT。bot 起動時は DB から読む** |
| `configs/strategy_config.next.yaml` | advisor が直近書き出した提案 (validator 待ち。symbols が 2 つ以上なら `strategy_config.next_<SYMBOL>.yaml`) | 議論用 artifact のみ |
| `runtime/emergency_stop.flag` | 存在する間は新規エントリ全停止 | このファイル自身が SoT |
| `runtime/ai_input/latest_summary.json` | 1 分ごと更新の市場サマリ (Claude 入力。symbols が 2 つ以上なら `latest_summary_<SYMBOL>.json`) | minuteLoop が随時更新 |
| `.env` | DATABASE_URL / GMO_API_KEY / LIVE_* / DASHBOARD_USER/PASS など | このファイル自身が SoT |

---

## 2. bot_config.yaml の構造

[backend/internal/config/bot_config.go](../../backend/internal/config/bot_config.go) で parse。
起動時に読む bot_config は `BOT_CONFIG_PATH` (既定 `configs/bot_config.yaml`)、hard_limits は
`HARD_LIMITS_PATH` (既定 `configs/hard_limits.yaml`)。

[configs/bot_config.yaml](../../configs/bot_config.yaml) の構造 (コメント省略。実値はファイルが正):

```yaml
bot:
  mode: paper_config        # disabled | paper_config | live_config
  timezone: Asia/Tokyo

# multi-symbol: 配列形式が正。legacy `symbol: USD_JPY` も Normalize() で
# 互換解釈されるが、新規記述では symbols[] を使う。
# 全 symbol が hard_limits.allowed_symbols に含まれていないと起動しない。
symbols:
  - USD_JPY
  - EUR_JPY
  - GBP_JPY

ai_advisor:                 # claude CLI が戦略 config を定期生成する経路 (opt-in)
  enabled: false
  provider: claude_cli
  interval_minutes: 30
  weekdays_only: true
  claude_cli_timeout_seconds: 300
  config_ttl_minutes: 60
  prompt_path: prompts/generate_strategy_config.md
  input_path: runtime/ai_input/latest_summary.json
  output_path: configs/strategy_config.next.yaml

advisor_v2:                 # 日足チャートブレイク検出 + 判定 subagent (opt-in)
  enabled: false
  # exclusive / deterministic / quantity / max_spread_pips / interval_minutes / symbols / max_concurrent

risk:
  # per-symbol cap (1 symbol あたりの上限)
  max_daily_loss_jpy: 2000
  max_consecutive_losses: 4
  max_open_positions: 1
  # account-wide cap。0 = 無効。symbols が 2 個以上なら設定する。
  account_max_open_positions: 3
  account_max_daily_loss_jpy: 18000

gmo:
  public_base_url: https://forex-api.coin.z.com/public
  private_base_url: https://forex-api.coin.z.com/private
  private_get_limit_per_sec: 6
  private_post_limit_per_sec: 1

orders:
  prefer_order_type: MARKET_THEN_OCO   # コードからは読まれない (parse のみ)。live の新規は常に成行 + 約定後の broker 側 OCO
  fallback_order_type: ""              # reserved

scheduler:
  weekdays_only: true
  start_day: MONDAY
  start_time: "07:00"
  end_day: SATURDAY
  end_time: "06:00"
```

tracked ファイルの安全側既定 (mode が live_config でない / 損失 cap が小さい / LLM を定期的に呼ぶ経路が
全部 off) は [tracked_bot_config_test.go](../../backend/internal/config/tracked_bot_config_test.go) が固定している。

旧 `position_guard` セクションは廃止済み (読む production code が無い)。残っていても非 strict
unmarshal で無視される。naked broker position の扱いは reconcile が決める ([DATA_MODEL.md](DATA_MODEL.md) `recovered_positions`)。

`bot.mode` は `config.Mode` 型。production code では `m.IsLive()` / `m.IsPaper()` で判定する
(`"live_config"` 文字列直接比較は禁止)。

### 2.1 `llm_decision` セクション (自律 LLM トレードループ・opt-in)

LLM が一定間隔で trade / no_trade を判断し、既存の発注経路 (risk Gate + broker OCO) で建てるループの設定。
**tracked の `configs/bot_config.yaml` にはこのセクションが無い = 無効**。使う場合は自分の bot_config
(`BOT_CONFIG_PATH`) に `llm_decision:` を書き `enabled: true` にする。主要キー:

- `enabled` / `symbols` / `quantity` / `interval_minutes` / `max_spread_pips` / `max_hold_minutes` / `max_concurrent`
- `ratchet_arm_pips` / `ratchet_giveback_pips` (トレーリング出口)
- エントリー規律 (コード veto。LLM の判断後に決定論で拒否する): `night_buy_veto_hours_jst` /
  `max_range_position_24h_buy` / `min_range_position_24h_sell` / `htf_trend_veto_pips`
  (summary_24h.change_pips 基準) / `exhaustion_veto_pips` / `spike_veto_pips_15m` / `daily_loss_stop_count`
- セッションガード: `no_entry_hours_jst` (この JST 時間帯は LLM を呼ばずに新規を止める) /
  `session_flatten_jst` (毎朝この JST 時刻に全 OPEN 玉を `session_flatten` で手仕舞う)
- 条件付きエントリー: `arm_enabled` / `arm_max_distance_pips`
- イベント再判断: `event_retrigger` (決済・急変動で判断サイクルを前倒し)
- Reflexion 反省ループ: `reflection_enabled` (省略時 true。false で playbook の自動書換を止める) /
  `reflection_interval_minutes` / `reflection_min_trades` / `reflection_start_at`
- シンボル別の上書き: `quantity_by_symbol` (シンボルごとの発注数量) /
  `htf_trend_veto_exempt_{sell,buy}_rpos` (24h レンジ内の位置で `htf_trend_veto` を免除する境界。シンボル別 map)
- 決定論戦略との時間帯の住み分け: `exclude_hours_jst` (シンボル → JST の時間。その時間は LLM ループが判断せず、
  active config の戦略を決定論エンジンが回す)
- `decision_single_agent` (省略時 true = 1 つの claude で判断。false で subagent パネル)
- 不使用: `htf_trend_veto_lookback` (旧 yaml の parse 互換のみ)

`runtime/emergency_stop.flag` がある間、判断サイクルは LLM を呼ばずに stage `emergency_stop` で終わる。

**値はここに二重管理しない**: キーの意味・既定値は
[backend/internal/config/bot_config.go](../../backend/internal/config/bot_config.go) の
`LLMDecisionSection` 定義 + コメントが正。フローは [SYSTEM_DESIGN.md §6.5](SYSTEM_DESIGN.md)。

---

## 3. hard_limits.yaml — 戦略 config の出力上下限

[backend/internal/config/hard_limits.go](../../backend/internal/config/hard_limits.go) で parse。

strategy config (advisor 生成 / 手動 seed とも) の各値が hard_limits の範囲内かを validator が検証する。
範囲外は **必ず reject** (= 緩めの fallback はしない)。

[configs/hard_limits.yaml](../../configs/hard_limits.yaml) の構造 (抜粋。
**実値は configs/hard_limits.yaml が正。本サンプルは構造の説明用**):

```yaml
hard_limits:
  allowed_symbols: [USD_JPY, EUR_JPY, GBP_JPY, EUR_USD, GBP_USD]

  quantity:           # min/max。ExecuteOrder の defaultQty も hard cap 経由
    min: 1000         # 最小発注 (= 0.1 lot)。手動売買 UI default と同じ
    max: 100000       # = 10 lot

  take_profit_pips: { min: 16.0, max: 100.0 }   # global fallback
  stop_loss_pips:   { min: 10.0, max: 45.0 }
  max_hold_minutes: { min: 30,   max: 43200 }
  max_trades_in_this_window:   { min: 0, max: 5 }
  max_loss_in_this_window_jpy: { min: 0, max: 5000 }
  max_spread_pips:    { min: 0.3, max: 3.0 }
  config_ttl_minutes: { min: 60, max: 120 }

  order_boundary:     # 発注直前の per-trade 境界 (全戦略共通)
    max_loss_per_trade_jpy: 8000
    max_stop_loss_pips: 50
    max_take_profit_pips: 120
  strategy_order_boundary:   # 戦略別に order_boundary を上書き
    signature_breakout: { max_loss_per_trade_jpy: 20000, max_stop_loss_pips: 200, max_take_profit_pips: 1500 }

  strategy_limits:    # 戦略別に TP/SL レンジを override
    momentum_pullback:
      take_profit_pips: { min: 8.0, max: 80.0 }
      stop_loss_pips:   { min: 6.0, max: 40.0 }
    # breakout_follow / range_breakout_probe / ma_pullback ...

  paper:              # Paper モードのシミュレーション cost (Live は無視)
    simulated_slippage_pips: 0.0
    api_fee_jpy_per_trade: 0.0

  cooldown:           # 決済直後の連打防止 (0 = 無効)
    after_entry_seconds: 0
    after_loss_seconds: 0
    after_take_profit_seconds: 0
```

**未充足のレンジ (validator ギャップ)**: `extension_max_minutes` / `extension_unrealized_pips_threshold` (StrategyConfig.Exit に存在) は hard_limits 側にレンジ定義がなく、config が任意値を出せる。advisor の config を live で使うなら、先に hard_limits へレンジを足すこと。

(SoT は [hard_limits.go](../../backend/internal/config/hard_limits.go) の struct 定義)

---

## 4. strategy_config.yaml の構造 — DB SoT

[backend/internal/config/strategy_config.go](../../backend/internal/config/strategy_config.go) で parse。

YAML は nested。mode は YAML に持たず、seed / promote するときに決まる (`seed_active_config.sh --mode`)。
全キーと advisor 出力の契約は [PROMPTS.md §3](../integrations/PROMPTS.md)、記入例は
[configs/strategy_config.active.example.yaml](../../configs/strategy_config.active.example.yaml)
(advisor 形式) と [configs/trend_v4_USD_JPY.yaml](../../configs/trend_v4_USD_JPY.yaml) (固定戦略)。抜粋:

```yaml
config_id: "<一意な ID>"
generated_at: "<RFC3339>"
valid_from: "<RFC3339>"
valid_until: "<RFC3339>"        # 固定 TP/SL の config は valid_from から 60〜120 分 (config_ttl_minutes)
symbol: USD_JPY
enabled: true
market_regime: { type: unclear, confidence: 0.70, reason: "<自由記述>" }
strategy:
  name: trend_follow            # 登録済み戦略。一覧は backend/internal/domain/strategy/engine.go の Register
entry:
  max_spread_pips: 1.5
  require_breakout: false
  direction: both               # buy_only | sell_only | both | none
  # allowed_hours_jst: [4, 10, 11]   # 任意。この JST 時間だけ新規を許す
exit:
  exit_policy: strategy_computed  # 出口を戦略が算出する (下の TP/SL は validator 用の placeholder。TTL 検査も免除)
  take_profit_pips: 100.0
  stop_loss_pips: 30.0
  max_hold_minutes: 43200
  ratchet_arm_pips: 0.0
  ratchet_giveback_pips: 0.0
risk:
  quantity: 1000
  max_open_positions: 1
  max_trades_in_this_window: 0
  max_loss_in_this_window_jpy: 2000   # config 単位の損失 kill-switch (0 = 無効)
no_trade: { enabled: false, reason: "" }
# next_advisor_run_in_minutes: 30   # advisor 形式のみ
```

### DB SoT のルール

- bot は **起動時にだけ** (symbol, mode) ごとに DB の `status='active'` 行を 1 本読む
  ([backend/cmd/bot/symbol_bundle_wiring.go](../../backend/cmd/bot/symbol_bundle_wiring.go) `loadActiveConfigsForBundles` → `Promoter.LoadActiveFromDB`)。
  読込時に起動時検証 (parse + `ValidateStatic`: schema / hard_limits / 戦略 whitelist) を掛ける。
- DB 読込・検証の失敗時に YAML へ fallback はしない。Live は起動を中断 (fail-closed)、paper は WARN
  (`active_config_db_load_failed_starting_without_active`) を出して active 無しで起動する。
- active 行が無い symbol は何も建てない。GetActive / LoadActiveFromDB は (nil, nil) を返し、何もログを出さない
  (決定論エンジンの価格ループは黙って見送り、LLM 判断ループは stage `no_active_config` で終わる)。
  seed できたかは起動ログの `active_config_loaded_from_db` で確かめる。
- advisor の promote 成功後に `configs/strategy_config.active.yaml` を best-effort で同期 (artifact)
- 既存ポジションは **建てた時点の config を凍結保存**

### 4.1 active config を DB に入れる

固定戦略 (advisor を使わない運用) では、`configs/` の strategy config YAML を手で DB の active に据える:

```bash
# repo root から。既定は dry-run (BEGIN … ROLLBACK で DB を変えない)
bash scripts/seed_active_config.sh configs/<file>.yaml
# 本適用 (bot を止めてから)
bash scripts/seed_active_config.sh --apply configs/<file>.yaml
# live 側に入れる / SQL を表示するだけ
bash scripts/seed_active_config.sh --apply --mode live_config configs/<file>.yaml
bash scripts/seed_active_config.sh --print-sql configs/<file>.yaml
```

- 検証は [backend/cmd/config-check](../../backend/cmd/config-check/main.go) が行う。起動時と同じ検証
  (parse + schema / hard_limits / 戦略 whitelist) で、1 本でも落ちたら何も seed しない。
  失効済み (`valid_until` が過去) の config や同一 symbol の 2 本指定も中止する。
- 同じ (mode, symbol) の既存 active を `expired` にしてから upsert する (行は消さない。過去の trade の FK を保つ)。
- `--mode` は `paper_config` (既定) か `live_config`。bot が読むのは `bot.mode` と同じ mode の行だけ。
- **active config は起動時にしか読まれない**。seed したら bot を再起動する (`make start`)。
  active 行が無い symbol は、paper / live とも起動は続くが何も建てない (ログは出ない。確認は起動ログの
  `active_config_loaded_from_db`)。active 行の読込・検証に落ちると、paper は WARN を出して続行し、live は起動しない。

### partial unique index (0001_init)

```sql
CREATE UNIQUE INDEX uniq_strategy_configs_active_per_symbol_mode
ON strategy_configs (symbol, mode)
WHERE status = 'active';
```

= 同じ (symbol, mode) で active は同時に 1 つだけ。partial Tx 失敗時の安全網。

---

## 5. Validator — strategy_config の段階検証

[backend/internal/config/validator.go](../../backend/internal/config/validator.go) で実装。

| 段階 | 検証対象 | 失敗時の挙動 |
|---|---|---|
| `schema` | YAML パース可能か / 必須フィールド揃ってるか | reject |
| `hard_limit` | 各値が hard_limits.yaml の範囲内か | reject |
| `semantic` | trend_up + sell_only のような矛盾 | reject |
| `risk` | 現在 account state (daily_loss / consec_losses / cooldown) と合うか | reject |

advisor 経路では各段階の pass/fail が `config_validation_events` テーブルに保存される ([DATA_MODEL.md](DATA_MODEL.md) 参照)。
起動時と `cmd/config-check` は runtime 状態に依存しない `ValidateStatic` (risk 段を除く) を使う。

---

## 6. ActiveConfigHolder — メモリ内 active config

[backend/internal/app/active_config.go](../../backend/internal/app/active_config.go):

- `Get(symbol)` で symbol の現在の active config を返す (`sync.Map`。mutex は持たない)
- `Set(symbol, cfg)` で起動時の読込と promote 成功時に更新
- worker / handler / advisor すべてここ経由で active を参照

---

## 7. 環境変数 / Live mode の制御

[backend/internal/app/live_guard.go](../../backend/internal/app/live_guard.go) /
[backend/cmd/bot/exec_broker.go](../../backend/cmd/bot/exec_broker.go):

Live 起動には以下が **全て** 必要:

| 条件 | 役割 | 欠けた場合 |
|---|---|---|
| `bot_config.bot.mode = live_config` | YAML 側でも明示 | paper のまま |
| `LIVE_TRADING_ENABLED=true` | safety gate | paper に降格 (WARN) |
| `LIVE_CONFIRM_SYMBOLS=<csv>` | `bot_config.symbols` と集合一致 (単一 symbol なら legacy `LIVE_CONFIRM_SYMBOL` も可) | paper に降格 (WARN) |
| `GMO_API_KEY` / `GMO_API_SECRET` | GMO API auth | 起動エラー |

数量上限の SSOT は `configs/hard_limits.yaml` の `quantity.max`。数量上限用の環境変数は持たない。

---

## 8. アンチパターン

- ❌ `bot_config.yaml` を bot 動作中に変更する (= 再起動必要)
- ❌ DB の active 行を差し替えて再起動しない (= active config は起動時にしか読まれない)
- ❌ `strategy_config.active.yaml` を手で書き換えて bot を再起動する (= DB と乖離する。DB の active 行を変更してから artifact を書く)
- ❌ Live 起動時に `LIVE_*` env を 1 つだけ欠けて起動する (= paper に降格する WARN を見逃す)
- ❌ hard_limits.yaml の値を緩めて validator の reject を減らす (= 攻めた値を validator で抑え込む設計が崩れる)
- ❌ ローカル DB を wipe する (= §9 を参照)

---

## 9. ローカル DB データ保護ルール

ローカル開発でも postgres のデータは **bind mount** (`./.docker-data/postgres:/var/lib/postgresql/data`, [docker-compose.yml](../../docker-compose.yml)) で host 側に永続化される。`make stop` ⇄ `make start` を何度往復しても positions / trades / strategy_configs / advisor 履歴は失われない設計。

### 禁止操作

- ❌ `docker compose down -v` — named volume を破壊する。bind mount は無事だが将来 named volume に切り替えた場合に備えて打たない
- ❌ `rm -rf .docker-data/postgres` / `.docker-data/` — host 側データの直接削除
- ❌ trade / position 系テーブルに対する手動 `DROP TABLE` / `TRUNCATE`
- ❌ dev 環境でも migrations を down → up し直す操作

**Why:** 取引履歴を継続管理するため。dev DB でも live DB でも、過去の trade / position レコードを失うのは強い損失。

### postgres が壊れた時の復旧優先度

データ保護の度合いが高い順に試す:

1. **Docker Desktop の再起動** — VirtioFS の lock 状態 (`Resource deadlock would occur` 等) をクリア
2. **`pg_resetwal -f`** をコンテナ内で実行 — **実行前に `.docker-data/postgres` を丸ごと退避する**。WAL を捨てるので直近のコミットが
   失われ、データが不整合になることもある。起動できたらすぐ `pg_dump` を取り、新しい DB へ入れ直すのが安全
3. **data dir wipe + `migrate up`** ← **最終手段。事前に DB 所有者の明示承認を取る** (取引履歴は backup から戻す)

### 自動化されたガード

- [Makefile](../../Makefile) `test-integration`: `INTEGRATION_TEST_DB_URL` 未設定なら skip、`DATABASE_URL` と同じ値か DB 名が `_test` で終わらない場合は refuse して exit。詳細は [TESTING.md §6](../workflows/TESTING.md)
- [Makefile](../../Makefile) `make stop`: `docker compose down` のみ (`-v` 無し) — bind mount は触らない

---

## 10. 関連 docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — 設計契約
- [DATA_MODEL.md](DATA_MODEL.md) — strategy_configs / config_validation_events テーブル
- [RUNTIME.md](RUNTIME.md) — ActiveConfigHolder / runtime ファイル
- [PROMPTS.md](../integrations/PROMPTS.md) — advisor が hard_limits 内で出力するように仕向ける skill
