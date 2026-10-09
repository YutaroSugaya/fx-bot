#!/usr/bin/env bash
# pretooluse-deny-edit.sh — PreToolUse deny hook for Write/Edit/MultiEdit/NotebookEdit.
#
# pretooluse-deny.sh は matcher:Bash 限定なので、Write/Edit による enforcement ファイル
# (.claude/settings*.json / .claude/hooks/ / .githooks/) の書換えはこちらでその場で exit 2 する
# (pre-stop-checks.sh の自己防衛は turn 終了時の warn-once なので二段構え)。
#
# 配線 (.claude/settings.json。AI 編集不可なので人間が追記):
#   "hooks": { "PreToolUse": [
#     { "matcher": "Bash",        "hooks": [ { "type": "command",
#         "command": "${CLAUDE_PROJECT_DIR}/.claude/hooks/pretooluse-deny.sh" } ] },
#     { "matcher": "Write|Edit|MultiEdit|NotebookEdit",  "hooks": [ { "type": "command",
#         "command": "${CLAUDE_PROJECT_DIR}/.claude/hooks/pretooluse-deny-edit.sh" } ] }
#   ] }
#
# 脱出口 (人間の意図的なメンテ用):
#   touch runtime/ALLOW_ENFORCEMENT_EDIT  # 存在する間は enforcement 編集を許可。
#   作業後に rm すること。
#
# テスト: bash .claude/hooks/pretooluse-deny-edit_test.sh
set -uo pipefail

input="$(cat 2>/dev/null || true)"

# tool 名を抽出 (jq が無くても動くよう grep フォールバック)。
tool=""
if command -v jq >/dev/null 2>&1; then
  tool="$(printf '%s' "$input" | jq -r '.tool_name // empty' 2>/dev/null || true)"
fi
[ -z "$tool" ] && tool="$(printf '%s' "$input" | grep -oE '"tool_name"[[:space:]]*:[[:space:]]*"[^"]+"' 2>/dev/null | head -1 | sed -E 's/.*"([^"]+)"$/\1/' || true)"

# 編集系 tool 以外は対象外。
case "$tool" in
  Write|Edit|MultiEdit|NotebookEdit) ;;
  *) exit 0 ;;
esac

# file_path を抽出。
fp=""
if command -v jq >/dev/null 2>&1; then
  fp="$(printf '%s' "$input" | jq -r '.tool_input.file_path // .tool_input.notebook_path // empty' 2>/dev/null || true)"
fi
[ -z "$fp" ] && fp="$(printf '%s' "$input" | grep -oE '"(file_path|notebook_path)"[[:space:]]*:[[:space:]]*"[^"]+"' 2>/dev/null | head -1 | sed -E 's/.*"([^"]+)"$/\1/' || true)"
[ -z "$fp" ] && exit 0

# 人間のメンテ脱出口: sentinel があれば許可。
proj="${CLAUDE_PROJECT_DIR:-.}"
[ -f "${proj}/runtime/ALLOW_ENFORCEMENT_EDIT" ] && exit 0
[ -f "runtime/ALLOW_ENFORCEMENT_EDIT" ] && exit 0

# enforcement ファイル (settings*.json / hooks/ / githooks/) への編集を deny。
if printf '%s' "$fp" | grep -qE '(^|/)\.claude/settings[^/]*\.json$|(^|/)\.claude/hooks/|(^|/)\.githooks/'; then
  echo "PreToolUse DENY: enforcement ファイルの AI 直接編集は禁止: $fp" >&2
  echo "settings/hooks/githooks は防壁本体です。意図的な変更は人間が直接編集するか、" >&2
  echo "メンテなら 'touch runtime/ALLOW_ENFORCEMENT_EDIT' 後に再実行 (作業後に rm)。" >&2
  exit 2
fi

exit 0
