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

if [ ! -f "${PROMPT_FILE}" ]; then
  echo "Prompt file not found: ${PROMPT_FILE}" >&2
  exit 1
fi
if [ ! -f "${INPUT_FILE}" ]; then
  echo "Input summary not found: ${INPUT_FILE}" >&2
  exit 1
fi

"${CLAUDE_BIN}" -p "$(cat "${PROMPT_FILE}")

Input JSON:
$(cat "${INPUT_FILE}")
" > "${OUTPUT_FILE}"

echo "Wrote ${OUTPUT_FILE}"
