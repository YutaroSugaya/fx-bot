#!/usr/bin/env bash
# Resolve & exec the newest VSCode-bundled `claude` binary so the bot's
# advisor cycle always picks up the latest Claude Code release without
# manual `npm update -g`. Falls back to PATH `claude` if no bundled
# binary is found.
# Only looks at the macOS Apple Silicon extension layout (darwin-arm64); on any
# other platform it simply runs `claude` from PATH.
#
# Pointed at via .env: CLAUDE_CLI_PATH=<repo>/tools/claude_latest.sh
set -euo pipefail

EXT_GLOB="$HOME/.vscode/extensions/anthropic.claude-code-*-darwin-arm64/resources/native-binary/claude"

# `ls -t` sorts by mtime desc; newest extension dir wins. shellcheck disable=SC2012
latest="$(ls -t $EXT_GLOB 2>/dev/null | head -n 1 || true)"

if [ -n "${latest:-}" ] && [ -x "$latest" ]; then
  exec "$latest" "$@"
fi

# Fallback: whatever `claude` is on PATH (e.g. npm global install).
exec claude "$@"
