#!/usr/bin/env bash
# pretooluse-deny-edit_test.sh — pretooluse-deny-edit.sh の自動テスト。
#
# 使い方: bash .claude/hooks/pretooluse-deny-edit_test.sh
#   deny  = exit 2 を期待 (enforcement ファイルへの Write/Edit)
#   allow = exit 0 を期待 (それ以外 / sentinel あり / 非編集 tool)
set -u

hook="$(cd "$(dirname "$0")" && pwd)/pretooluse-deny-edit.sh"
pass=0
fail=0

run_case() {
  local expect="$1" name="$2" tool="$3" fp="$4" sentinel_dir="${5:-}"
  local json rc
  json=$(printf '{"tool_name":"%s","tool_input":{"file_path":"%s"}}' "$tool" "$fp")
  if [ -n "$sentinel_dir" ]; then
    printf '%s' "$json" | CLAUDE_PROJECT_DIR="$sentinel_dir" bash "$hook" >/dev/null 2>&1
  else
    printf '%s' "$json" | CLAUDE_PROJECT_DIR=/nonexistent-proj-xyz bash "$hook" >/dev/null 2>&1
  fi
  rc=$?
  case "$expect" in
    deny)  [ "$rc" -eq 2 ] ;;
    allow) [ "$rc" -eq 0 ] ;;
  esac
  if [ $? -eq 0 ]; then
    pass=$((pass + 1)); echo "  ok   [$expect] $name"
  else
    fail=$((fail + 1)); echo "  FAIL [$expect, got rc=$rc] $name"
  fi
}

echo "=== deny: enforcement ファイルへの Write/Edit ==="
run_case deny  "Write .claude/settings.json"          Write "/Users/x/fx-bot/.claude/settings.json"
run_case deny  "Write .claude/settings.local.json"    Write ".claude/settings.local.json"
run_case deny  "Edit pretooluse-deny.sh"              Edit  "/Users/x/fx-bot/.claude/hooks/pretooluse-deny.sh"
run_case deny  "Edit hook test"                       Edit  ".claude/hooks/pretooluse-deny_test.sh"
run_case deny  "Write .githooks/pre-push"             Write ".githooks/pre-push"

echo "=== allow: enforcement 以外への編集 ==="
run_case allow "Write backend go file"                Write "backend/internal/app/worker.go"
run_case allow "Edit docs"                            Edit  "docs/README.md"
run_case allow "Write .claude/agents (非enforcement)" Write ".claude/agents/trade-decider.md"
run_case allow "Write .claude/rules (非enforcement)"  Write ".claude/rules/layers.md"
run_case allow "Write runtime sentinel itself"        Write "runtime/ALLOW_ENFORCEMENT_EDIT"

echo "=== allow: 非編集 tool は対象外 ==="
run_case allow "Bash tool"                            Bash  "/Users/x/fx-bot/.claude/settings.json"
run_case allow "Read tool"                            Read  ".claude/hooks/pretooluse-deny.sh"

echo "=== allow: sentinel があれば enforcement 編集も許可 ==="
tmp="$(mktemp -d)"
mkdir -p "$tmp/runtime"
: > "$tmp/runtime/ALLOW_ENFORCEMENT_EDIT"
run_case allow "settings edit with sentinel"          Write ".claude/settings.json" "$tmp"
rm -rf "$tmp"

echo
echo "pass=$pass fail=$fail"
[ "$fail" -eq 0 ] || exit 1
exit 0
