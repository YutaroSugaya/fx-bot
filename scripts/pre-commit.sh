#!/usr/bin/env bash
# Simple secret-leak pre-commit hook.
# Enabled by `git config core.hooksPath .githooks` (.githooks/pre-commit calls this script).
# 自己テスト: bash scripts/secret-scan_test.sh
set -euo pipefail

# Patterns that should NEVER appear in a commit.
PATTERNS=(
  # GMO creds with a value (=, : and quoted forms)
  '(GMO_API_KEY|GMO_API_SECRET)[[:space:]]*[:=][[:space:]]*["'"'"']?[A-Za-z0-9+/=_-]{8,}'
  'API_SECRET=[A-Za-z0-9]'
  'AKIA[0-9A-Z]{16}'                 # AWS access key
  '-----BEGIN [A-Z ]*PRIVATE KEY-----'
  'xox[baprs]-[A-Za-z0-9-]{10,}'    # Slack token
  'gh[pousr]_[A-Za-z0-9]{30,}'       # GitHub token
  'sk-ant-[A-Za-z0-9_-]{20,}'        # Anthropic key
  'sk-[A-Za-z0-9]{20,}'              # OpenAI-style key
)

# Look only at ADDED lines in staged content. Removed lines (lines starting
# with `-`) and hunk headers (`@@ ... @@ context`) must be excluded — otherwise
# removing a secret or editing near a placeholder triggers a false positive.
staged_added=$(git diff --cached --no-color | grep -E '^\+' | grep -Ev '^\+\+\+ ' || true)

if [ -z "${staged_added}" ]; then
  exit 0
fi

violations=0
for pat in "${PATTERNS[@]}"; do
  # Use `-e` so patterns that start with `-` (e.g. "-----BEGIN ...") aren't
  # interpreted as grep flags. Here-string, not `echo | grep -q`: with pipefail a large
  # diff makes echo die of SIGPIPE when grep -q exits early, which would hide the match.
  if grep -E -q -e "${pat}" <<< "${staged_added}"; then
    echo "✗ pre-commit: potential secret pattern detected: ${pat}"
    violations=$((violations + 1))
  fi
done

# Also reject any staged file literally named .env / *.pem / *.key
staged_files=$(git diff --cached --name-only --diff-filter=AM || true)
while IFS= read -r f; do
  case "${f}" in
    .env|*.pem|*.key|*.p12)
      echo "✗ pre-commit: refusing to commit secret-like file: ${f}"
      violations=$((violations + 1))
      ;;
  esac
done <<< "${staged_files}"

if [ "${violations}" -gt 0 ]; then
  echo ""
  echo "Commit aborted. Remove the secret(s) above, or unstage the file."
  exit 1
fi

exit 0
