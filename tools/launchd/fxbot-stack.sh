#!/bin/bash
# fx-bot スタック自動復帰スクリプト (launchd: com.fxbot.stack)
#
# 目的: Mac 再起動・プロセス死の後に postgres コンテナ + bot + frontend を
# 自動復帰させる (スタックが止まったまま気付かないと、その間の取引と計測が丸ごと抜ける)。
#
# macOS (launchd + Docker Desktop) 専用。
#
# 導入 (1回だけ。plist 内の /Users/USERNAME は自分の $HOME に置き換える。
# repo が ~/Desktop/fx-bot 以外なら plist のスクリプトのパスも直し、FXBOT_REPO を設定する):
#   手順は deploy/launchd/README.md(repo の場所と $HOME の 2 段で置換してから load する)。
#   launchctl load ~/Library/LaunchAgents/com.fxbot.stack.plist
#   ※ load した瞬間に RunAtLoad で即起動する (手動 make start は不要になる)。
#   ※ ログイン・再起動のたびに、その時の bot_config / .env のまま起動する (live 設定なら live で動く)。
#   ※ deploy/launchd の com.fxbot.bot (bot-supervisor.sh) とは併用しない (どちらか一方)。
#   ※ Desktop 配下のスクリプトは macOS の TCC で launchd から実行できないことがある
#     (deploy/launchd/README.md)。
#   ※ OPEN/CLOSING ポジションが DB にあると make start は kill-stale で止まる (FORCE=1 が要る)。
#     その間はこのスクリプトも起動に失敗し、KeepAlive が再試行を続ける (broker OCO は残る)。
# 停止したい時:
#   launchctl unload ~/Library/LaunchAgents/com.fxbot.stack.plist
#   ※ unload せずに kill しても KeepAlive が再起動する (それが仕事)。
# ログ: ~/.fxbot/logs/stack-launchd.log (FXBOT_LOG_DIR で変更)
#
# PATH はリテラル絶対パスで列挙する (launchd から起動されたプロセスの PATH は最小限なので、
# 既存 PATH への追記に頼らず docker / make の場所を明示する)。
set -u
REPO="${FXBOT_REPO:-$HOME/Desktop/fx-bot}"
LOG_DIR="${FXBOT_LOG_DIR:-$HOME/.fxbot/logs}"
mkdir -p "$LOG_DIR"
exec >>"$LOG_DIR/stack-launchd.log" 2>&1
echo "=== $(date '+%Y-%m-%d %H:%M:%S') fxbot-stack boot ==="
export PATH="/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

# Docker daemon 待ち (Docker Desktop の起動が遅れるケース。最大 5 分)
for _ in $(seq 1 60); do
  if docker info >/dev/null 2>&1; then
    break
  fi
  sleep 5
done
if ! docker info >/dev/null 2>&1; then
  echo "docker daemon not up after 5min; giving up this attempt (KeepAlive will retry)"
  exit 1
fi

cd "$REPO" || exit 1
docker compose up -d postgres

# make start = kill-stale + migrate-up + bot + frontend (foreground)。
# プロセスが死ねば launchd の KeepAlive + ThrottleInterval が再起動する。
exec make start
