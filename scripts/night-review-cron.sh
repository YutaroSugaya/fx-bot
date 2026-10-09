#!/usr/bin/env bash
# night-review-cron.sh — 夜間の定点観測レポート (read-only)
#
# 毎晩 launchd から実行される read-only 定点観測。
#   - 入力: scripts/night_review.sql (同ディレクトリに cp して配置)
#   - 出力: ~/.fxbot/reports/night_review_YYYY-MM-DD.md (TCC 対策で Desktop 外)
#     ログ: ~/.fxbot/logs/night-review.log
#   - DB 書込・config 変更・bot 操作は一切しない (自動ループから live 系の操作はしない)
#   - 環境変数: FXBOT_REPO (repo の場所。既定 ~/Desktop/fx-bot) / FXBOT_API_URL (既定 http://localhost:8080)
#     / FXBOT_PG_CONTAINER / FXBOT_DB_NAME / FXBOT_DB_USER / FXBOT_REPORT_DIR / FXBOT_LOG_DIR
#
# install (人間が実施。deploy/launchd/README.md 参照):
#   cp scripts/night-review-cron.sh scripts/night_review.sql ~/.fxbot/
#   chmod +x ~/.fxbot/night-review-cron.sh
#   cp deploy/launchd/com.fxbot.night-review.plist ~/Library/LaunchAgents/
#   launchctl load ~/Library/LaunchAgents/com.fxbot.night-review.plist
set -euo pipefail

CONTAINER="${FXBOT_PG_CONTAINER:-fxbot-postgres}"
DB_NAME="${FXBOT_DB_NAME:-fxbot}"
DB_USER="${FXBOT_DB_USER:-fxbot}"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SQL_FILE="${SCRIPT_DIR}/night_review.sql"
REPORT_DIR="${FXBOT_REPORT_DIR:-${HOME}/.fxbot/reports}"
LOG="${FXBOT_LOG_DIR:-${HOME}/.fxbot/logs}/night-review.log"
EMERGENCY_FLAG="${FXBOT_REPO:-$HOME/Desktop/fx-bot}/runtime/emergency_stop.flag"
API_URL="${FXBOT_API_URL:-http://localhost:8080}"

mkdir -p "$REPORT_DIR" "$(dirname "$LOG")"
ts="$(date +%F)"
out="${REPORT_DIR}/night_review_${ts}.md"

note() { echo "$(date '+%F %T') $*" >> "$LOG"; }

if [ ! -f "$SQL_FILE" ]; then
  note "ERROR: $SQL_FILE not found (install 手順で cp する)"
  exit 2
fi
if ! docker ps --format '{{.Names}}' 2>/dev/null | grep -q "^${CONTAINER}$"; then
  note "ERROR: ${CONTAINER} not running — skip"
  exit 2
fi

{
  echo "# Night Review ${ts} (read-only)"
  echo
  echo "- generated: $(date '+%F %T %Z')(DB 時刻は UTC。JST は +9)"

  # emergency_stop の確認 (自動ループは各 iteration の冒頭で確認する)。
  # TCC で repo にアクセスできない環境では unknown として続行する。
  if [ -f "$EMERGENCY_FLAG" ] 2>/dev/null; then
    echo "- ⚠️ **emergency_stop: ACTIVE** — $(head -c 200 "$EMERGENCY_FLAG" 2>/dev/null || echo '(内容読取不可)')"
  elif [ -d "$(dirname "$EMERGENCY_FLAG")" ] 2>/dev/null; then
    echo "- emergency_stop: なし"
  else
    echo "- emergency_stop: unknown (repo にアクセス不可)"
  fi

  # bot 死活 (停止中は ratchet/MaxHold が効かない = 重要シグナル)。
  # /healthz は BasicAuth の外にある liveness endpoint (scripts/bot-healthcheck.sh と同じ)。
  if curl -s -f -m 3 -o /dev/null "${API_URL}/healthz" 2>/dev/null; then
    echo "- bot /healthz: **alive**"
  else
    echo "- ⚠️ **bot /healthz: 応答なし** — bot 停止中は ratchet/MaxHold が無効 (broker OCO のみ)"
  fi
  echo
  echo "## 直近 30 時間の定点観測 (night_review.sql)"
  echo
  echo '```'
  # night_review.sh と同じ呼び方 (read-only SELECT のみ)
  SINCE="$(docker exec "$CONTAINER" psql -U "$DB_USER" -d "$DB_NAME" -tAc "SELECT (now() - interval '30 hours')::timestamptz;")"
  echo ">>> since ${SINCE} (UTC)"
  docker exec -i "$CONTAINER" psql -U "$DB_USER" -d "$DB_NAME" -v "since=${SINCE}" < "$SQL_FILE"
  echo '```'
  echo
  echo "> 適用・判断は人間が行う (このループは config/戦略/DB に書き込まない)。"
} > "$out" 2>&1

note "wrote $out"
