#!/usr/bin/env bash
# Manual invocation of Claude CLI to generate next strategy config.
# Bot normally calls claude -p directly via internal/advisor — this script is
# for debugging / smoke testing. Run it from the repo root (paths are relative).
# Each run calls Claude once (uses your Claude plan / API usage) and overwrites
# OUTPUT_FILE (default configs/strategy_config.next.yaml, gitignored).
set -euo pipefail

PROMPT_FILE="${PROMPT_FILE:-prompts/generate_strategy_config.md}"
INPUT_FILE="${INPUT_FILE:-runtime/ai_input/latest_summary.json}"
OUTPUT_FILE="${OUTPUT_FILE:-configs/strategy_config.next.yaml}"
CLAUDE_BIN="${CLAUDE_CLI_PATH:-claude}"

# bot 本体と同じく、claude にはツールも repo の hooks も使わせず、bot の秘密を環境変数で渡さない。
SCRUB=(env -u GMO_API_KEY -u GMO_API_SECRET -u DATABASE_URL -u DATABASE_URL_RO -u BACKTEST_DATABASE_URL
       -u INTEGRATION_TEST_DB_URL -u DASHBOARD_USER -u DASHBOARD_PASS)

if [ ! -f "${PROMPT_FILE}" ]; then
  echo "Prompt file not found: ${PROMPT_FILE}" >&2
  exit 1
fi
if [ ! -f "${INPUT_FILE}" ]; then
  echo "Input summary not found: ${INPUT_FILE}" >&2
  exit 1
fi

"${SCRUB[@]}" "${CLAUDE_BIN}" -p --no-session-persistence --tools "" --settings '{"disableAllHooks":true}' "$(cat "${PROMPT_FILE}")

Input JSON:
$(cat "${INPUT_FILE}")
" > "${OUTPUT_FILE}"

echo "Wrote ${OUTPUT_FILE}"
