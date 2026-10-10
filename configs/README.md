# configs/

bot の実行設定と、戦略 config(YAML)の例を置く。戦略 config はどれも**エッジが確認されていない例**で、
live でそのまま使うためのものではない。

## 実行設定

| ファイル | 内容 |
|---|---|
| `bot_config.yaml` | 実行設定(tracked)。mode は `paper_config`、LLM を定期的に呼ぶ経路は全部 off(live_config にならないこと・LLM 経路が off であることを `backend/internal/config/tracked_bot_config_test.go` が固定) |
| `bot_config.live.example.yaml` | live 用の雛形。`bot_config.live.yaml`(gitignore)にコピーし、`.env` の `BOT_CONFIG_PATH` をそのパスにする |
| `hard_limits.yaml` | 戦略 config と発注が越えられない上下限。起動時にだけ読む |
| `event_calendar.yaml` | 経済指標の前後の新規停止窓。同梱分は記入例で、日程は自分で保守する |

## 戦略 config の使い方

bot は起動時にだけ、DB の `strategy_configs` から (symbol, mode) ごとに active を 1 本読む。
YAML を active にするには `scripts/seed_active_config.sh` を使う。先に起動時と同じ検証
(`backend/cmd/config-check`)を掛け、1 本でも落ちたら何も入れない。

```bash
bash scripts/seed_active_config.sh configs/<file>.yaml                           # dry-run
bash scripts/seed_active_config.sh --apply configs/<file>.yaml                   # bot を止めてから。後で make start
bash scripts/seed_active_config.sh --apply --mode live_config configs/<file>.yaml
```

seed できたかは、起動ログの `active_config_loaded_from_db`(シンボルごとに 1 行)で確かめる。
active が無いシンボルは何もログを出さずに建てない。

backtest では seed せずに YAML を直接渡す(コスト系フラグは既定 0 なので必ず指定する。
[docs/workflows/BACKTEST.md](../docs/workflows/BACKTEST.md))。

```bash
cd backend && go run ./cmd/backtest -config ../configs/<file>.yaml -symbol <SYMBOL> -from <YYYY-MM-DD> -to <YYYY-MM-DD> -fee-rate 0.002 -spread-median 0.5 -pnl-out pnl.json
```

ファイル名の `v4` / `v7` / `v83` などは作った順の識別子で、中身の優劣は表さない。
`<SYMBOL>` は USD_JPY / EUR_JPY / GBP_JPY / EUR_USD / GBP_USD。

## ファミリー

ratchet は「含み益が arm pips に届いたら追跡を始め、ピークから giveback pips 戻ったら成行で利確する
トレーリング出口」(含み損の側にも同じ幅の鏡像がある)。

| ファイル | 戦略 | 中身 | config-check | 用途 |
|---|---|---|---|---|
| `trend_v4_<SYMBOL>` | trend_follow | 1H 200SMA の傾きの方向へ、Donchian-48 の継続ブレイクで入る。SL(2×ATR)と ratchet(ATR 基準)は戦略が算出。config 単位の損失 kill-switch(`max_loss_in_this_window_jpy`)付き | 通る | seed の例(ルート README の手順) |
| `frozen_ma_pullback_<SYMBOL>` | ma_pullback | 1H 200SMA の傾きの方向へ、5m 200MA への戻りと反発で入る。TP/SL・ratchet(arm 16 / giveback 8)・保有 480 分は戦略が算出 | 通る | seed / backtest |
| `frozen_mtf_pullback_<SYMBOL>` | mtf_pullback | 1h の方向 × 5m の押し目 × 1m の壁ブレイクで入る。TP/SL は建値時点で構造的に算出し、ratchet は使わない | 通る | seed / backtest |
| `paper_v83_USD_JPY` | exhaustion_fade | LLM 判断ループ用。新規は LLM ループが決め、この行は建玉の参照先と、admission が読む `entry.max_spread_pips`(3.0)/ `risk.*` を与える(値は `TestPaperV83Config` が固定)。config 単位の損失 kill-switch は 0(無効) | 通る | `llm_decision` を有効にした bot の active |
| `probe_trend_follow_<SYMBOL>`、`probe_daily_trend_{EUR_USD,GBP_USD}` | trend_follow / daily_trend | 多年データで回す検証用。スプレッドは backtest のフラグで与えるので、config のスプレッド上限は 10.0 にしてある | 落ちる(`max_spread_pips` が hard_limits の上限 3.0 を超える) | backtest のみ(daily_trend は `-replay-tf 1d`) |
| `probe_london_breakout_{EUR_JPY,GBP_JPY,EUR_USD,GBP_USD}`、`probe_gotobi_USD_JPY` | london_breakout / gotobi_fix | 時間帯アノマリーの検証用。出口は戦略が算出 | 通る | backtest 用(seed は想定しない) |
| `strategy_config.active.example.yaml` | momentum_pullback | advisor(`ai_advisor`)が出力する形式の見本(TTL 60 分) | 通るが `valid_until` が過去 | 形式の参照用(seed は失効で拒否される) |

## 注意

- `paper_v83_*` を `llm_decision` が無効の bot に seed すると、strategy ブロック
  (exhaustion_fade)がそのまま決定論エンジンで建てる。`llm_decision.enabled: true` なら、
  エンジンの新規は全シンボルで止まり(`llm_decision.symbols` に無いシンボルも)、建てるのは
  `llm_decision.exclude_hours_jst` でそのシンボルに渡した時間帯だけになる。
- LLM 判断ループにも、そのシンボルの active config が要る(無ければ stage `no_active_config` で発注しない)。
- advisor 用の breakout_follow / range_breakout_probe と、momentum_pullback の追いかけ防止フィルタ
  (`max_chase_pips`)は pip を 0.01 で固定している(`backend/internal/domain/strategy/`)。USD 建てペア
  (EUR_USD / GBP_USD)ではこれらを使わない。他の戦略は `market.PipSize` でペアごとに解決する。
- ファイルごとの検査は `backend/internal/config/*_config_test.go` と `backend/cmd/config-check/main_test.go`。
