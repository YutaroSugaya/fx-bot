#!/usr/bin/env bash
# bot-healthcheck.sh — bot の死活を外部から監視する。
#
# 主出口 (ratchet / MaxHold) は bot プロセス前提。bot が死ぬと armed 後の利確 floor が
# 消え、遠い SL (broker OCO) まで往復し得る。このスクリプトを launchd で定期実行し、
# bot が応答しなければ通知する。read-only (GET のみ)・DB には触れない。
#
# probe は GET /healthz (BasicAuth の外にある liveness endpoint。200 + {"status":"ok"})。
# /api/status は BasicAuth 必須なので、認証無しで叩くと 401 になり DOWN と誤判定する。
#
# 使い方:
#   scripts/bot-healthcheck.sh                 # 1 回チェックして exit code で結果を返す
#   API_URL=http://localhost:8080 scripts/bot-healthcheck.sh
# exit 0 = healthy, 1 = unreachable/unhealthy。launchd 経由なら deploy/launchd 参照。
# 結果は $HEALTHCHECK_LOG(既定 ~/.fxbot/healthcheck.log・repo の外)に追記する。
# DOWN 時の通知は macOS なら osascript のローカル通知、HEALTHCHECK_WEBHOOK があればそこへ POST。
set -euo pipefail

API_URL="${API_URL:-http://localhost:8080}"
TIMEOUT="${HEALTHCHECK_TIMEOUT:-5}"
LOG_FILE="${HEALTHCHECK_LOG:-$HOME/.fxbot/healthcheck.log}"
mkdir -p "$(dirname "$LOG_FILE")"

ts() { date -u +"%Y-%m-%dT%H:%M:%SZ"; }
log() { echo "$(ts) $*" | tee -a "$LOG_FILE" >&2; }

notify() {
  local msg="$1"
  # macOS のローカル通知 (best-effort)。Slack 等を使う場合は HEALTHCHECK_WEBHOOK を設定。
  if command -v osascript >/dev/null 2>&1; then
    osascript -e "display notification \"${msg}\" with title \"fxbot bot DOWN\"" 2>/dev/null || true
  fi
  if [ -n "${HEALTHCHECK_WEBHOOK:-}" ]; then
    curl -s -m "$TIMEOUT" -X POST -H 'Content-Type: application/json' \
      -d "{\"text\":\"fxbot DOWN: ${msg}\"}" "$HEALTHCHECK_WEBHOOK" >/dev/null 2>&1 || true
  fi
}

body_file="$(mktemp "${TMPDIR:-/tmp}/fxbot_health.XXXXXX")"
trap 'rm -f "$body_file"' EXIT

code=$(curl -s -m "$TIMEOUT" -o "$body_file" -w "%{http_code}" "${API_URL}/healthz" 2>/dev/null) || code="000"
if [ "$code" != "200" ]; then
  log "UNHEALTHY: GET ${API_URL}/healthz -> HTTP ${code}"
  notify "GET /healthz -> HTTP ${code}"
  exit 1
fi
# 同じポートで別プロセスが 200 を返すケースを弾く (bot の /healthz は "status":"ok" か "ok":true)。
if ! grep -Eq '"status": ?"ok"|"ok": ?true' "$body_file"; then
  log "UNHEALTHY: GET ${API_URL}/healthz -> HTTP 200 but unexpected body: $(head -c 200 "$body_file")"
  notify "GET /healthz -> unexpected body"
  exit 1
fi

log "healthy: HTTP 200"
exit 0
