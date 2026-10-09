#!/usr/bin/env bash
# bot-supervisor.sh — launchd 常駐用の bot 起動ラッパ。
#
# launchd の KeepAlive と組み合わせて bot を常駐させる。terminal `go run` 手動起動・
# supervisor 無しだと bot 死で主出口 (ratchet/MaxHold) が消える問題への対策。
# 注: macOS TCC のため Desktop 配下のスクリプトを launchd から実行すると失敗しうる (db-backup.sh と同じ)。
#    deploy/launchd/README.md の手順でこのスクリプトを ~/.fxbot/ にコピーして使う。
# macOS 前提(launchd / lsof)。ログは $FXBOT_LOG_DIR(既定 ~/.fxbot/logs・repo の外)に書く。
set -euo pipefail

# 既定は clone 先が ~/Desktop/fx-bot の場合。別の場所なら FXBOT_PROJECT_DIR を設定する
# (deploy/launchd/README.md)。
PROJECT_DIR="${FXBOT_PROJECT_DIR:-$HOME/Desktop/fx-bot}"
LOG_DIR="${FXBOT_LOG_DIR:-$HOME/.fxbot/logs}"
mkdir -p "$LOG_DIR"

cd "$PROJECT_DIR/backend"

# .env をロード (DATABASE_URL / LIVE_TRADING_ENABLED 等)。
if [ -f "$PROJECT_DIR/.env" ]; then
  set -a; . "$PROJECT_DIR/.env"; set +a
fi

# 二重起動ガード: bot API ポートが既に LISTEN なら別 bot (例: terminal の make start)
# が稼働中。二重稼働 = 二重発注になるため、空くまで待つ (旧 bot を Ctrl+C すれば
# 自動で handover される。KeepAlive のリスポーン暴走もこの待機で防ぐ)。
BOT_PORT="${FXBOT_BOT_PORT:-8080}"
while lsof -iTCP:"$BOT_PORT" -sTCP:LISTEN >/dev/null 2>&1; do
  echo "$(date -u +%FT%TZ) supervisor: port $BOT_PORT busy (another bot instance?) — waiting 30s" >> "$LOG_DIR/bot.log"
  sleep 30
done

ts=$(date -u +"%Y%m%dT%H%M%SZ")
echo "$(date -u +%FT%TZ) supervisor: starting bot (pid wrapper)" >> "$LOG_DIR/bot.log"
exec go run ./cmd/bot >> "$LOG_DIR/bot-$ts.log" 2>&1
