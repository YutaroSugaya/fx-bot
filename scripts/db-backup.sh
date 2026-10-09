#!/usr/bin/env bash
# fxbot postgres backup.
#
# docker exec で postgres コンテナの pg_dump を呼び、gzip して $FXBOT_BACKUP_DIR(既定 ~/.fxbot/backups)に保存する。
# ログは $FXBOT_LOG_DIR(既定 ~/.fxbot/logs)。
# KEEP 世代を超えた古い dump は件数ベースで削除する。コンテナが動いていなければ黙って skip。
#
# Makefile の backup-now / backup-list / restore-drill / backup-restore は ~/.fxbot/ を見る。
# launchd から定期実行するなら、このファイルを ~/.fxbot/db-backup.sh にコピーして使う
# (macOS の TCC は launchd から ~/Desktop 配下のスクリプトを実行させない):
#   mkdir -p ~/.fxbot && cp scripts/db-backup.sh ~/.fxbot/db-backup.sh && chmod +x ~/.fxbot/db-backup.sh
#
# 単独実行: scripts/db-backup.sh

set -euo pipefail

BACKUP_DIR="${FXBOT_BACKUP_DIR:-$HOME/.fxbot/backups}"
LOG_DIR="${FXBOT_LOG_DIR:-$HOME/.fxbot/logs}"
LOG_FILE="$LOG_DIR/db-backup.log"
CONTAINER="${FXBOT_PG_CONTAINER:-fxbot-postgres}"
DB_NAME="${FXBOT_DB_NAME:-fxbot}"
DB_USER="${FXBOT_DB_USER:-fxbot}"
KEEP_GENERATIONS="${FXBOT_BACKUP_KEEP:-30}"
if ! [[ "$KEEP_GENERATIONS" =~ ^[0-9]+$ ]] || [ "$KEEP_GENERATIONS" -lt 1 ]; then
  echo "FXBOT_BACKUP_KEEP must be an integer >= 1 (got '$KEEP_GENERATIONS')" >&2
  exit 2
fi

mkdir -p "$BACKUP_DIR" "$LOG_DIR"

log() { printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" | tee -a "$LOG_FILE" >&2; }

# Skip silently if container not running (bot 停止中は通常運用).
if ! docker ps --format '{{.Names}}' 2>/dev/null | grep -qx "$CONTAINER"; then
  log "skip: container $CONTAINER not running"
  exit 0
fi

# Skip if postgres not actually accepting queries.
if ! docker exec "$CONTAINER" pg_isready -U "$DB_USER" -d "$DB_NAME" -q 2>/dev/null; then
  log "skip: $CONTAINER pg_isready failed"
  exit 0
fi

ts="$(date -u +%Y%m%dT%H%M%SZ)"
out="$BACKUP_DIR/$DB_NAME-$ts.sql.gz"
tmp="$out.tmp"

# pg_dump plain SQL → gzip。--no-owner/--no-privileges/--clean/--if-exists で
# restore 先の role 差異と既存 object 両方を許容。
if ! docker exec "$CONTAINER" \
       pg_dump --no-owner --no-privileges --clean --if-exists \
               -U "$DB_USER" "$DB_NAME" 2>>"$LOG_FILE" \
     | gzip -c > "$tmp"; then
  log "FAIL: pg_dump errored (see above); removing partial $tmp"
  rm -f "$tmp"
  exit 1
fi

mv "$tmp" "$out"
bytes=$(stat -f%z "$out" 2>/dev/null || wc -c < "$out")
log "ok: wrote $out ($bytes bytes)"

# Rotation: 件数ベース (mtime ではなく)。KEEP 世代を超えた古い dump を消す。
ls -1t "$BACKUP_DIR"/"$DB_NAME"-*.sql.gz 2>/dev/null \
  | tail -n +$((KEEP_GENERATIONS + 1)) \
  | while read -r old; do
      log "rotate: removing $old"
      rm -f "$old"
    done

exit 0
