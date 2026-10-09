#!/usr/bin/env bash
# night_review.sh — 定点観測 (read-only)。
#
# scripts/night_review.sql を稼働中の fxbot-postgres に対して実行する。
# SELECT のみ・DB を一切変更しない (postgres も止めない。live bot 稼働中でも安全)。
#
# 使い方:
#   scripts/night_review.sh                       # 直近 30 時間
#   scripts/night_review.sh '2026-06-02 09:00:00+00'   # 指定時刻 (UTC) 以降
#   scripts/night_review.sh '7 days'              # 相対指定 (now() - interval)

set -euo pipefail

CONTAINER="${FXBOT_PG_CONTAINER:-fxbot-postgres}"
DB_NAME="${FXBOT_DB_NAME:-fxbot}"
DB_USER="${FXBOT_DB_USER:-fxbot}"
SQL_FILE="$(cd "$(dirname "$0")" && pwd)/night_review.sql"

if ! docker ps --format '{{.Names}}' | grep -q "^${CONTAINER}$"; then
  echo "ERROR: ${CONTAINER} が起動していません。make db-up で起動してください。" 1>&2
  exit 2
fi

# 引数の解釈: interval 風 ("7 days") なら now()-interval、それ以外は絶対時刻、
# 省略時は直近 30 時間。
ARG="${1:-}"
if [ -z "$ARG" ]; then
  SINCE_EXPR="now() - interval '30 hours'"
elif [[ "$ARG" =~ ^[0-9]+\ (hour|hours|day|days|week|weeks|month|months)$ ]]; then
  SINCE_EXPR="now() - interval '${ARG}'"
else
  SINCE_EXPR="'${ARG}'::timestamptz"
fi

# :since を解決した timestamptz として渡す (SQL 側は :'since' で参照)。
SINCE="$(docker exec "$CONTAINER" psql -U "$DB_USER" -d "$DB_NAME" -tAc "SELECT (${SINCE_EXPR})::timestamptz;")"

echo ">>> night_review since ${SINCE} (UTC) / $(date '+%Y-%m-%d %H:%M %Z')"
docker exec -i "$CONTAINER" psql -U "$DB_USER" -d "$DB_NAME" -v "since=${SINCE}" < "$SQL_FILE"
