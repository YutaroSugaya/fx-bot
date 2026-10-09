#!/usr/bin/env bash
# pretooluse-deny.sh — PreToolUse deny hook (Bash).
#
# 公式仕様上 PreToolUse の exit 2 は defaultMode=bypassPermissions でも貫通して
# 効く唯一の機械的防壁。Bash 系の破壊的コマンドを exit 2 で deny する。
# stdin に Claude Code が tool 入力 JSON を渡す。tool_name != Bash は素通り (exit 0)。
#
# 配線 (.claude/settings.json。AI 編集不可なので人間が追記):
#   "hooks": { "PreToolUse": [ { "matcher": "Bash",
#     "hooks": [ { "type": "command",
#       "command": "${CLAUDE_PROJECT_DIR}/.claude/hooks/pretooluse-deny.sh" } ] } ] }
#
# deny 対象 (絶対ルール / CLAUDE.md と一致):
#   - go test ... -tags ... integration         (live trades を truncate)
#   - docker compose down|stop|rm                (postgres を止める / DB wipe)
#   - rm -rf .*docker-data                       (postgres data 消去)
#   - DROP SCHEMA / DROP DATABASE / TRUNCATE     (DB 破壊)
#   - pkill/kill ... postgres                    (postgres を止める)
#   - pkill/killall ... bot プロセス             (再起動は人間に依頼する)
#   - psql 書込系 INSERT/UPDATE/DELETE/ALTER     (live DB は read-only。
#     例外 = 人間が個別指定した時のみ FXBOT_HUMAN_APPROVED_DB_WRITE=1 を
#     コマンドに前置して実行する。fxbot_test 宛ては常に許可)
#
# テスト: bash .claude/hooks/pretooluse-deny_test.sh
# pipefail は付けない: `printf | grep -q && deny` で grep が早く抜けると printf が SIGPIPE で
# 落ち、pipefail 下ではパイプ全体が失敗扱いになって長いコマンドの deny を素通りさせるため。
set -eu

input="$(cat 2>/dev/null || true)"

# tool 名を抽出 (jq が無くても動くよう grep フォールバック)。
tool=""
if command -v jq >/dev/null 2>&1; then
  tool="$(printf '%s' "$input" | jq -r '.tool_name // empty' 2>/dev/null || true)"
fi
[ -z "$tool" ] && tool="$(printf '%s' "$input" | grep -oE '"tool_name"[[:space:]]*:[[:space:]]*"[^"]+"' | head -1 | sed -E 's/.*"([^"]+)"$/\1/')"

# Bash 以外は対象外。
[ "$tool" != "Bash" ] && exit 0

# command 文字列を抽出。
cmd=""
if command -v jq >/dev/null 2>&1; then
  cmd="$(printf '%s' "$input" | jq -r '.tool_input.command // empty' 2>/dev/null || true)"
fi
[ -z "$cmd" ] && cmd="$input"

deny() {
  echo "PreToolUse DENY: $1" >&2
  echo "絶対ルール違反です。CLAUDE.md 参照。意図的に必要なら人間が手動で実行してください。" >&2
  exit 2
}

# DB 宛先/承認の判定ヘルパ (psql / dropdb / pg_restore / migrate down で共用)。
#   _test を含むコマンドは fxbot_test 宛てとみなし常に許可。
#   FXBOT_HUMAN_APPROVED_DB_WRITE=1 前置は人間が個別承認した書込の脱出口。
is_test_db() {
  printf '%s' "$cmd" | grep -qE '(postgres(ql)?://[^[:space:]"'"'"']*/|-d[[:space:]]*|--dbname[=[:space:]]|dbname=|(dropdb|createdb)[[:space:]]+(-[^[:space:]]+[[:space:]]+)*)[A-Za-z0-9_]+_test([?"'"'"'[:space:];&|)]|$)'
}
is_human_approved_write() { printf '%s' "$cmd" | grep -qE 'FXBOT_HUMAN_APPROVED_DB_WRITE=1'; }

# go test -tags integration (順序不問: -tags=integration / -tags integration / 別位置)
if printf '%s' "$cmd" | grep -qiE 'go[[:space:]]+test'; then
  if printf '%s' "$cmd" | grep -qiE '(-tags[=[:space:]]+[^ ]*integration|integration[^ ]*[[:space:]]+.*go[[:space:]]+test)'; then
    deny "go test -tags integration は live DB の trades を truncate する"
  fi
fi

# docker compose down|stop|rm|kill|restart|pause
printf '%s' "$cmd" | grep -qiE 'docker(-| +)compose[[:space:]]+(.*[[:space:]])?(down|stop|rm|kill|restart|pause)\b' \
  && deny "docker compose down/stop/rm/kill/restart/pause は postgres/DB を止める・消す"

# plain docker でコンテナを停止/削除/再起動 (compose 非経由)。
# 破壊動詞 + postgres コンテナ名にスコープ (docker exec ... SELECT の read-only 調査は素通し)。
printf '%s' "$cmd" | grep -qiE '\bdocker[[:space:]]+(stop|kill|rm|restart|pause)\b[^|;&]*\b(fxbot-postgres|postgres)\b' \
  && deny "docker stop/kill/rm/restart/pause で postgres コンテナを止めない (CLAUDE.md)"

# rm -rf ... docker-data
printf '%s' "$cmd" | grep -qiE 'rm[[:space:]]+-[a-z]*r[a-z]*f?[a-z]*[[:space:]].*docker-data' \
  && deny "rm -rf docker-data は postgres のデータを消す"

# DROP SCHEMA / DROP DATABASE / TRUNCATE (psql 経由含む)。
# _test 宛て・human-approved は脱出口: read-only SELECT が文字列に
# DROP/TRUNCATE を含むだけの誤検知や、fxbot_test の正当な truncate/drop を許可する。
if ! is_test_db && ! is_human_approved_write; then
  printf '%s' "$cmd" | grep -qiE 'DROP[[:space:]]+(SCHEMA|DATABASE)\b' \
    && deny "DROP SCHEMA/DATABASE は DB を破壊する。_test or FXBOT_HUMAN_APPROVED_DB_WRITE=1 以外は人間が手動"
  printf '%s' "$cmd" | grep -qiE '\bTRUNCATE[[:space:]]+(TABLE[[:space:]]+)?[a-z]' \
    && deny "TRUNCATE は取引履歴を消す。_test or FXBOT_HUMAN_APPROVED_DB_WRITE=1 以外は人間が手動"
fi

# pkill / kill postgres
printf '%s' "$cmd" | grep -qiE '(pkill|killall)[[:space:]].*postgres' \
  && deny "postgres プロセスを止めると live bot の DB 接続が切れる"

# pkill / killall で bot プロセスを止める (再起動は人間に依頼。make kill-stale は
# Makefile 側の FORCE ガードに委譲するためここでは対象外)
printf '%s' "$cmd" | grep -qiE '(pkill|killall)[[:space:]].*(exe/bot|cmd/bot|fxbot)' \
  && deny "live bot プロセスを止めない。再起動が必要なら人間に依頼して待つ (CLAUDE.md)"

# psql 書込系 (live DB は read-only。SELECT のみ許可)
#   - fxbot_test 宛て (DSN に _test) は常に許可
#   - 人間が個別指定した UPDATE 等は FXBOT_HUMAN_APPROVED_DB_WRITE=1 前置で許可
if printf '%s' "$cmd" | grep -qE 'psql'; then
  if ! is_test_db && ! is_human_approved_write; then
    printf '%s' "$cmd" | grep -qiE '\b(INSERT|UPDATE|DELETE|ALTER)\b' \
      && deny "live DB は read-only (SELECT のみ)。人間が個別指定した書込なら FXBOT_HUMAN_APPROVED_DB_WRITE=1 を前置して再実行"
    # -f/--file はファイル内 SQL を hook が検査できない=書込が素通りする穴。
    # 連結形 -f/path・-f=path も捕捉 (-f 直後の空白を必須にしない)。read-only に psql -f は通常不要。
    printf '%s' "$cmd" | grep -qE '[[:space:]]-f([[:space:]/=]|$)|[[:space:]]--file([[:space:]]|=)' \
      && deny "psql -f/--file はファイル内 SQL を検査できない。read-only 調査に -f は不要。書込なら FXBOT_HUMAN_APPROVED_DB_WRITE=1 を前置して再実行"
    # パイプ/stdin 流し込み (cat x.sql | psql / psql < file) も SQL 本文が grep 不可視。
    # psql の出力を | grep する read-only 慣用は発火しない (パイプ先が psql の時だけ)。
    printf '%s' "$cmd" | grep -qE '\|[[:space:]]*psql\b' \
      && deny "'... | psql' はファイル/stdin の SQL を検査できない。read-only は -c 'SELECT'。書込なら FXBOT_HUMAN_APPROVED_DB_WRITE=1 を前置"
    printf '%s' "$cmd" | grep -qE '\bpsql\b[^|]*[[:space:]]<[[:space:]]*([~/.]|[^[:space:]]*\.(sql|txt|dump))' \
      && deny "'psql < file' は stdin の SQL を検査できない。read-only は -c 'SELECT'。書込なら FXBOT_HUMAN_APPROVED_DB_WRITE=1 を前置"
  fi
fi

# make 経由の間接実行: make ラッパ経由だと上の文字列マッチを素通りするので target 名で deny。
#   db-down        = docker compose down (postgres 停止)
#   backup-restore = --clean 付き dump を psql で流し込み (live DB 上書き)
# どちらも live 稼働に直結するので無条件 deny (意図的な実行は人間が手動)。
printf '%s' "$cmd" | grep -qiE '\bmake\b[^|;&]*[[:space:]](db-down|backup-restore|stop)([[:space:];&|)]|$)' \
  && deny "make db-down/backup-restore/stop は postgres停止/live DB上書き。意図的なら人間が手動実行 (CLAUDE.md)"

# migrate down (make 経由 / 直接 go run) は schema を巻き戻す。_test 宛て・human-approved 以外は deny。
if printf '%s' "$cmd" | grep -qiE '\bmake\b.*\bmigrate-down\b|cmd/migrate[[:space:]]+down\b'; then
  if ! is_test_db && ! is_human_approved_write; then
    deny "migrate down は live スキーマを巻き戻す。_test 宛て or FXBOT_HUMAN_APPROVED_DB_WRITE=1 以外は人間が手動実行"
  fi
fi

# dropdb / pg_restore は psql/DROP/TRUNCATE のどのパターンにも当たらないが live DB を破壊/上書きする。
# (pg_dump は read-only=バックアップ用途なので対象外)
if printf '%s' "$cmd" | grep -qiE '\b(dropdb|pg_restore)\b'; then
  if ! is_test_db && ! is_human_approved_write; then
    deny "dropdb/pg_restore は live DB を破壊/上書きする。_test 宛て or FXBOT_HUMAN_APPROVED_DB_WRITE=1 以外は人間が手動実行"
  fi
fi

# enforcement ファイル(防壁本体)を Bash で書換え/削除/無力化する経路を deny。
# edit-guard(Write/Edit matcher)は Bash を見ないので Bash 側はここで塞ぐ。さもなくば
# `printf 'exit 0' > .claude/hooks/pretooluse-deny.sh` 一発で全 deny を無力化できてしまう。
# 対象: .claude/settings*.json / .claude/hooks/ / .githooks/ への リダイレクト(> >>)・
#       tee / cp / mv / rm / ln / install / dd / truncate・sed -i(書込動詞 + パス隣接の形のみ)。
# 読取(cat/grep/bash 実行/git add)・/tmp 等への書込は対象外。脱出口=runtime/ALLOW_ENFORCEMENT_EDIT。
if [ ! -f "${CLAUDE_PROJECT_DIR:-.}/runtime/ALLOW_ENFORCEMENT_EDIT" ] && [ ! -f "runtime/ALLOW_ENFORCEMENT_EDIT" ]; then
  enf='(\.claude/settings[^/]*\.json|\.claude/hooks/|\.githooks/)'
  printf '%s' "$cmd" | grep -qE ">>?[[:space:]]*[\"']?[^[:space:]|;&<>\"']*${enf}" \
    && deny "enforcement ファイル(.claude/settings*/hooks/, .githooks/)への Bash リダイレクト書込は禁止。防壁本体です。メンテは 'touch runtime/ALLOW_ENFORCEMENT_EDIT' 後に再実行(後で rm)"
  printf '%s' "$cmd" | grep -qE "\b(tee|cp|mv|rm|ln|install|dd|truncate)\b[^|;&]*${enf}" \
    && deny "enforcement ファイルの Bash 書換え/削除/移動は禁止。防壁本体です。メンテは 'touch runtime/ALLOW_ENFORCEMENT_EDIT' 後に再実行(後で rm)"
  printf '%s' "$cmd" | grep -qE "\bsed\b[^|;&]*-i[^|;&]*${enf}" \
    && deny "sed -i で enforcement ファイルを書換えない。メンテは 'touch runtime/ALLOW_ENFORCEMENT_EDIT' 後に再実行(後で rm)"
fi

exit 0
