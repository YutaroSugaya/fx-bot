#!/usr/bin/env bash
# secret-scan.sh — committed-secret スキャン。
# CI (.github/workflows/ci.yml) と pre-push hook (.githooks/pre-push) が共用。
# 自己テスト: bash scripts/secret-scan_test.sh
#
# **実際のキー形式**でマッチし、docs の解説中の文字列 (例: "sk-ant-api03-" への言及) で
# 誤検出しないよう、キー本体まで要求する。
#
# tracked files のみ走査。
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

# real key bodies を要求するので doc 中の言及には当たらない。
patterns=(
  'sk-ant-api[0-9]{2}-[A-Za-z0-9_-]{40,}'   # Anthropic API key (real body)
  'sk-[A-Za-z0-9]{32,}'                     # OpenAI-style key
  'AKIA[0-9A-Z]{16}'                        # AWS access key id
  '-----BEGIN[A-Z ]*PRIVATE KEY-----'       # private key block
  'xox[baprs]-[A-Za-z0-9-]{10,}'            # Slack token
  'gh[pousr]_[A-Za-z0-9]{36,}'              # GitHub token
  '(GMO_API_KEY|GMO_API_SECRET)[[:space:]]*[:=][[:space:]]*["'"'"']?[A-Za-z0-9+/=_-]{16,}' # GMO creds with a real value (base64 系の記号を含む)
)

# 除外: パターンをコメント / テストデータとして持つスキャナ自身と hook 群だけ。docs(*.md)や .env.example も
# 走査する(パターンはキー本体まで要求するので、解説中の言及やプレースホルダには当たらない)。
excludes=(':!scripts/secret-scan.sh' ':!scripts/secret-scan_test.sh' ':!scripts/pre-commit.sh' ':!.claude/hooks/*' ':!.githooks/*' ':!.codex/hooks/*')

found=0
for p in "${patterns[@]}"; do
  # -e: "-----BEGIN" で始まるパターンを option と解釈させない。
  if git grep -nIE -e "$p" -- "${excludes[@]}" 2>/dev/null; then
    found=1
  fi
done

if [ "$found" = "1" ]; then
  echo "!! secret-scan: 上記に秘密情報らしき文字列が tracked file に含まれています。除去して履歴も確認してください。" >&2
  exit 1
fi
echo "secret-scan: clean"
exit 0
